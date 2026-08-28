package extensions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/replica"
)

// HTTPClient maintains one HTTP/SSE live subscription for a manifest. It
// publishes changes with POST and receives change events over SSE. It has no
// replay log and does not reconnect automatically.
type HTTPClient struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	onChange func(replica.Change) error

	maxMessageBytes int
	maxActorBytes   int
	writeTimeout    time.Duration
	httpClient      *http.Client
	header          http.Header
	publishURL      string
	context         context.Context
	cancel          context.CancelFunc
	body            io.ReadCloser
	done            chan struct{}
	closeOnce       sync.Once
	errMu           sync.RWMutex
	err             error
}

// ConnectHTTP starts an authenticated SSE subscription, verifies the exact
// manifest supplied by the relay, and then receives live changes. A successful
// return means the relay registered the subscription before it sent the stream
// manifest. endpoint is the base mount URL, for example
// "https://sync.example.com/crdt". The configured HTTPClient must not have a
// global Timeout because SSE is a long-lived response; use request contexts and
// WriteTimeout instead.
func ConnectHTTP(ctx context.Context, endpoint string, manifest replica.Manifest, config ClientConfig) (*HTTPClient, error) {
	if config.OnChange == nil {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeClientLimits(config)
	if err != nil {
		return nil, err
	}
	if _, err := replica.NewSessionWithPolicy(manifest, config.Policy); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	eventsURL, err := httpGroupURL(endpoint, manifest.GroupID, "events")
	if err != nil {
		return nil, err
	}
	publishURL, err := httpGroupURL(endpoint, manifest.GroupID, "changes")
	if err != nil {
		return nil, err
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	connectionContext, cancelConnection := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(connectionContext, http.MethodGet, eventsURL, nil)
	if err != nil {
		cancelConnection()
		return nil, err
	}
	request.Header = cloneHeader(config.Header)
	request.Header.Set("Accept", "text/event-stream")
	response, err := openHTTPStream(ctx, limits.handshakeTimeout, httpClient, request, cancelConnection)
	if err != nil {
		cancelConnection()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		closeResponse(response)
		cancelConnection()
		return nil, fmt.Errorf("connect HTTP extensions stream: unexpected status %s", response.Status)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		closeResponse(response)
		cancelConnection()
		return nil, errInvalidWireMessage
	}
	hello, err := decodeSSEData(response.Header.Get("X-CRDT-Manifest"), controlLimit(limits.maxMessageBytes))
	if err != nil {
		closeResponse(response)
		cancelConnection()
		return nil, errInvalidWireMessage
	}
	remote, err := unmarshalHello(hello)
	if err != nil {
		closeResponse(response)
		cancelConnection()
		return nil, err
	}
	if err := manifest.Compatible(remote); err != nil {
		closeResponse(response)
		cancelConnection()
		return nil, err
	}
	reader := bufio.NewReaderSize(response.Body, sseLineLimit(limits.maxMessageBytes))
	client := &HTTPClient{
		manifest:        manifest,
		policy:          config.Policy,
		onChange:        config.OnChange,
		maxMessageBytes: limits.maxMessageBytes,
		maxActorBytes:   limits.maxActorBytes,
		writeTimeout:    limits.writeTimeout,
		httpClient:      httpClient,
		header:          cloneHeader(config.Header),
		publishURL:      publishURL,
		context:         connectionContext,
		cancel:          cancelConnection,
		body:            response.Body,
		done:            make(chan struct{}),
	}
	go client.readLoop(reader)
	return client, nil
}

// Publish validates and posts a canonical change envelope. A POST failure does
// not close the independent SSE subscription; callers decide whether and how
// to retry from their durable outbox.
func (client *HTTPClient) Publish(ctx context.Context, change replica.Change) error {
	if client == nil {
		return ErrClosed
	}
	select {
	case <-client.context.Done():
		return ErrClosed
	default:
	}
	verified, err := replica.NewChangeWithPolicy(client.manifest, change.Dot, change.Delta(), client.policy)
	if err != nil {
		return fmt.Errorf("validate change: %w", err)
	}
	encoded, err := marshalChange(verified)
	if err != nil {
		return err
	}
	if len(verified.Dot.Actor) > client.maxActorBytes || len(encoded) > client.maxMessageBytes {
		return errInvalidWireMessage
	}
	writeContext, cancel := context.WithTimeout(ctx, client.writeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(writeContext, http.MethodPost, client.publishURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header = cloneHeader(client.header)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("publish HTTP change: unexpected status %s", response.Status)
	}
	return nil
}

// Done closes when the SSE receive loop stops.
func (client *HTTPClient) Done() <-chan struct{} {
	if client == nil {
		return nil
	}
	return client.done
}

// Err returns the first non-local stream or callback error.
func (client *HTTPClient) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.errMu.RLock()
	defer client.errMu.RUnlock()
	return client.err
}

// Close stops the SSE stream without claiming durable delivery.
func (client *HTTPClient) Close() error {
	if client == nil {
		return ErrClosed
	}
	client.stop(nil)
	<-client.done
	return nil
}

func (client *HTTPClient) readLoop(reader *bufio.Reader) {
	defer close(client.done)
	expectManifest := true
	for {
		event, data, err := readSSEEvent(reader, client.maxMessageBytes)
		if err != nil {
			select {
			case <-client.context.Done():
				return
			default:
				client.stop(err)
				return
			}
		}
		if expectManifest {
			if event != "manifest" {
				client.stop(errInvalidWireMessage)
				return
			}
			remote, err := unmarshalHello(data)
			if err != nil || client.manifest.Compatible(remote) != nil {
				client.stop(errInvalidWireMessage)
				return
			}
			expectManifest = false
			continue
		}
		if event != "change" {
			client.stop(errInvalidWireMessage)
			return
		}
		dot, delta, err := unmarshalChange(data, client.maxMessageBytes, client.maxActorBytes)
		if err != nil {
			client.stop(err)
			return
		}
		change, err := replica.NewChangeWithPolicy(client.manifest, dot, delta, client.policy)
		if err != nil {
			client.stop(fmt.Errorf("validate received change: %w", err))
			return
		}
		if err := client.onChange(change); err != nil {
			client.stop(fmt.Errorf("apply received change: %w", err))
			return
		}
	}
}

func (client *HTTPClient) stop(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		client.errMu.Lock()
		if client.err == nil {
			client.err = err
		}
		client.errMu.Unlock()
	}
	client.closeOnce.Do(func() {
		client.cancel()
		_ = client.body.Close()
	})
}

func httpGroupURL(endpoint, groupID, operation string) (string, error) {
	if strings.TrimSpace(groupID) == "" || (operation != "changes" && operation != "events") {
		return "", ErrInvalidConfig
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidConfig
	}
	routeID := base64.RawURLEncoding.EncodeToString([]byte(groupID))
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/http/groups/" + routeID + "/" + operation
	parsed.RawPath = ""
	return parsed.String(), nil
}

func sseLineLimit(maxDataBytes int) int {
	return len("data: ") + base64.StdEncoding.EncodedLen(maxDataBytes) + 2
}

func readSSEEvent(reader *bufio.Reader, maxDataBytes int) (string, []byte, error) {
	eventLine, err := readSSELine(reader)
	if err != nil {
		return "", nil, err
	}
	dataLine, err := readSSELine(reader)
	if err != nil {
		return "", nil, err
	}
	terminator, err := readSSELine(reader)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(eventLine, "event: ") || !strings.HasPrefix(dataLine, "data: ") || terminator != "" {
		return "", nil, errInvalidWireMessage
	}
	event := strings.TrimPrefix(eventLine, "event: ")
	encoded := strings.TrimPrefix(dataLine, "data: ")
	if event == "" || len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(maxDataBytes) {
		return "", nil, errInvalidWireMessage
	}
	data, err := decodeSSEData(encoded, maxDataBytes)
	if err != nil {
		return "", nil, err
	}
	return event, data, nil
}

func decodeSSEData(encoded string, maxDataBytes int) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(maxDataBytes) {
		return nil, errInvalidWireMessage
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxDataBytes || base64.StdEncoding.EncodeToString(data) != encoded {
		return nil, errInvalidWireMessage
	}
	return data, nil
}

func readSSELine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return string(line), nil
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	return header.Clone()
}

type httpStreamResult struct {
	response *http.Response
	err      error
}

func openHTTPStream(ctx context.Context, timeout time.Duration, client *http.Client, request *http.Request, cancel context.CancelFunc) (*http.Response, error) {
	results := make(chan httpStreamResult, 1)
	go func() {
		response, err := client.Do(request)
		results <- httpStreamResult{response: response, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-results:
		return result.response, result.err
	case <-ctx.Done():
		cancel()
		go closeLateHTTPStreamResult(results)
		return nil, ctx.Err()
	case <-timer.C:
		cancel()
		go closeLateHTTPStreamResult(results)
		return nil, context.DeadlineExceeded
	}
}

func closeLateHTTPStreamResult(results <-chan httpStreamResult) {
	result := <-results
	closeResponse(result.response)
}
