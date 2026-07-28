// Command crdt-sync-probe exercises CRDT delta delivery over real HTTP links.
// It is a short-lived test utility, not a production replication service.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/darkinno/crdt/counter"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/set"
)

const (
	defaultListen = "127.0.0.1:49511"
	maxBodyBytes  = 1 << 20
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "crdt.sync-probe/string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

type probe struct {
	counter *counter.GCounter
	set     *set.ORSet[string]
	token   [sha256.Size]byte
}

type probeState struct {
	Counts   map[string]uint64 `json:"counts"`
	Elements []string          `json:"elements"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("crdt-sync-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "", "serve or send")
	listen := flags.String("listen", defaultListen, "server listen address")
	target := flags.String("target", "", "comma-separated server base URLs when mode=send")
	token := flags.String("token", "", "shared bearer token; prefer -token-file to avoid process argument exposure")
	tokenFile := flags.String("token-file", "", "path to a file containing the shared bearer token")
	replica := flags.String("replica", "", "non-empty logical replica ID")
	increment := flags.Uint64("counter-increment", 1, "counter increment to deliver; zero skips counter delivery")
	element := flags.String("element", "probe", "OR-Set element to deliver; empty skips set delivery")
	duplicates := flags.Int("duplicates", 3, "deliver each generated delta this many times")
	timeout := flags.Duration("timeout", 15*time.Second, "network timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	authToken, err := loadToken(*token, *tokenFile)
	if err != nil || *replica == "" || *duplicates <= 0 || *timeout <= 0 {
		return errors.New("a non-empty -token or -token-file, -replica, positive -duplicates, and positive -timeout are required")
	}

	switch *mode {
	case "serve":
		if err := serve(*listen, *replica, authToken, *timeout); err != nil {
			return fmt.Errorf("serve probe: %w", err)
		}
		return nil
	case "send":
		if *target == "" || (*increment == 0 && *element == "") {
			return errors.New("-target and at least one non-empty mutation are required for send")
		}
		if err := send(*target, *replica, authToken, *increment, *element, *duplicates, *timeout); err != nil {
			return fmt.Errorf("send probe: %w", err)
		}
		return nil
	default:
		return errors.New("-mode must be serve or send")
	}
}

func loadToken(value, path string) (result string, err error) {
	if value != "" && path != "" {
		return "", errors.New("token and token-file are mutually exclusive")
	}
	if path == "" {
		if value == "" {
			return "", errors.New("token is empty")
		}
		return value, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(data) > 1024 {
		return "", errors.New("token file exceeds 1024 bytes")
	}
	result = strings.TrimSpace(string(data))
	if result == "" {
		return "", errors.New("token is empty")
	}
	return result, nil
}

func newProbe(replicaID, token string) (*probe, error) {
	counterValue, err := counter.NewGCounter(replicaID)
	if err != nil {
		return nil, err
	}
	setValue, err := set.NewORSet(replicaID, stringCodec{})
	if err != nil {
		return nil, err
	}
	return &probe{counter: counterValue, set: setValue, token: sha256.Sum256([]byte(token))}, nil
}

func serve(listen, replicaID, token string, timeout time.Duration) error {
	value, err := newProbe(replicaID, token)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           value,
		ReadHeaderTimeout: timeout,
		ReadTimeout:       timeout,
		WriteTimeout:      timeout,
		IdleTimeout:       timeout,
	}
	return server.ListenAndServe()
}

func (p *probe) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !p.authorized(request) {
		writeError(writer, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/state":
		writeJSON(writer, http.StatusOK, p.state())
	case request.Method == http.MethodPost && request.URL.Path == "/counter":
		p.applyCounter(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/orset":
		p.applyORSet(writer, request)
	default:
		writeError(writer, http.StatusNotFound, errors.New("not found"))
	}
}

func (p *probe) authorized(request *http.Request) bool {
	provided := sha256.Sum256([]byte(request.Header.Get("X-CRDT-Probe-Token")))
	return subtle.ConstantTimeCompare(provided[:], p.token[:]) == 1
}

func (p *probe) applyCounter(writer http.ResponseWriter, request *http.Request) {
	encoded, err := readRequest(request)
	if err == nil {
		var decoded counter.GCounterDelta
		decoded, err = counter.UnmarshalGCounterDeltaWithLimits(encoded, transportLimits())
		if err == nil {
			err = p.counter.ApplyDelta(decoded)
		}
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, p.state())
}

func (p *probe) applyORSet(writer http.ResponseWriter, request *http.Request) {
	encoded, err := readRequest(request)
	if err == nil {
		var decoded set.ORSetDelta[string]
		decoded, err = set.UnmarshalORSetDeltaWithLimits(encoded, stringCodec{}, transportLimits())
		if err == nil {
			err = p.set.ApplyDelta(decoded)
		}
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, p.state())
}

func (p *probe) state() probeState {
	elements := p.set.Elements()
	sort.Strings(elements)
	return probeState{Counts: p.counter.Counts(), Elements: elements}
}

func send(targetList, replicaID, token string, increment uint64, element string, duplicates int, timeout time.Duration) error {
	targets, err := parseTargets(targetList)
	if err != nil {
		return err
	}
	value, err := newProbe(replicaID, token)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	var counterDelta, setDelta []byte
	if increment != 0 {
		delta, err := value.counter.Increment(increment)
		if err != nil {
			return err
		}
		counterDelta, err = delta.MarshalBinary()
		if err != nil {
			return err
		}
	}
	if element != "" {
		delta, err := value.set.Add(element)
		if err != nil {
			return err
		}
		setDelta, err = delta.MarshalBinary(stringCodec{})
		if err != nil {
			return err
		}
	}
	reports := make(map[string]probeState, len(targets))
	for _, baseURL := range targets {
		if counterDelta != nil {
			if err := postRepeated(client, baseURL+"/counter", token, counterDelta, duplicates); err != nil {
				return fmt.Errorf("deliver counter to %s: %w", baseURL, err)
			}
		}
		if setDelta != nil {
			if err := postRepeated(client, baseURL+"/orset", token, setDelta, duplicates); err != nil {
				return fmt.Errorf("deliver OR-Set to %s: %w", baseURL, err)
			}
		}
		state, err := fetchState(client, baseURL, token)
		if err != nil {
			return fmt.Errorf("read state from %s: %w", baseURL, err)
		}
		reports[baseURL] = state
	}
	return json.NewEncoder(os.Stdout).Encode(reports)
}

func parseTargets(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		target := strings.TrimRight(strings.TrimSpace(part), "/")
		parsed, err := url.ParseRequestURI(target)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid target %q", part)
		}
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		result = append(result, target)
	}
	if len(result) == 0 {
		return nil, errors.New("no targets provided")
	}
	return result, nil
}

func fetchState(client *http.Client, baseURL, token string) (probeState, error) {
	request, err := http.NewRequest(http.MethodGet, baseURL+"/state", nil)
	if err != nil {
		return probeState{}, err
	}
	request.Header.Set("X-CRDT-Probe-Token", token)
	response, err := client.Do(request)
	if err != nil {
		return probeState{}, err
	}
	if response.StatusCode != http.StatusOK {
		if closeErr := response.Body.Close(); closeErr != nil {
			return probeState{}, closeErr
		}
		return probeState{}, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	var state probeState
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxBodyBytes)).Decode(&state)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return probeState{}, decodeErr
	}
	if closeErr != nil {
		return probeState{}, closeErr
	}
	return state, nil
}

func postRepeated(client *http.Client, endpoint, token string, data []byte, duplicates int) error {
	for count := 0; count < duplicates; count++ {
		request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("X-CRDT-Probe-Token", token)
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxBodyBytes))
		closeErr := response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("deliver delta: unexpected HTTP status %s", response.Status)
		}
	}
	return nil
}

func readRequest(request *http.Request) (data []byte, err error) {
	defer func() {
		if closeErr := request.Body.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	data, err = io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxBodyBytes {
		return nil, errors.New("request body is empty or exceeds transport limit")
	}
	return data, nil
}

func transportLimits() frame.DecoderLimits {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = maxBodyBytes
	limits.MaxPayload = maxBodyBytes - 1024
	limits.MaxElements = 1 << 16
	limits.MaxTags = 1 << 16
	limits.MaxStringBytes = 1 << 16
	return limits
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
