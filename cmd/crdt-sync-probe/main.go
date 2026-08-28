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
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/set"
	"github.com/im10furry/crdt/text"
)

const (
	defaultListen          = "127.0.0.1:49511"
	maxResponseBytes       = 1 << 20
	maxSmallRequestBytes   = 1 << 20
	maxRGARequestBytes     = 16 << 20
	maxRGARunesPerDelivery = 200_000
	applyDurationHeader    = "X-CRDT-Apply-Micros"
)

type rgaProtocol string

const (
	rgaProtocolDisabled rgaProtocol = "disabled"
	rgaProtocolV1       rgaProtocol = "v1"
	rgaProtocolRunV2    rgaProtocol = "run-v2"

	// defaultRGAProtocol is the wire format used by new probe sessions. The
	// scalar v1 format remains available only when both endpoints explicitly
	// opt in, so a legacy frame cannot be silently accepted by this path.
	defaultRGAProtocol rgaProtocol = rgaProtocolRunV2
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "crdt.sync-probe/string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

type probe struct {
	counter     *counter.GCounter
	set         *set.ORSet[string]
	rga         *text.RGA
	rgaProtocol rgaProtocol
	token       [sha256.Size]byte
}

type textState struct {
	Protocol string `json:"protocol"`
	Runes    int    `json:"runes"`
	SHA256   string `json:"sha256"`
	Pending  int    `json:"pending"`
}

type probeState struct {
	Counts   map[string]uint64 `json:"counts"`
	Elements []string          `json:"elements"`
	Text     textState         `json:"text"`
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
	allowNonLoopback := flags.Bool("allow-non-loopback", false, "allow a non-loopback listener for a controlled test only")
	increment := flags.Uint64("counter-increment", 1, "counter increment to deliver; zero skips counter delivery")
	element := flags.String("element", "probe", "OR-Set element to deliver; empty skips set delivery")
	rgaRunes := flags.Int("rga-runes", 0, "number of one-rune RGA characters to deliver; zero skips RGA delivery")
	rgaRune := flags.String("rga-rune", "x", "one UTF-8 rune repeated for each RGA character")
	rgaProtocolName := flags.String("rga-protocol", string(defaultRGAProtocol), "RGA frame protocol: run-v2 (stable default), v1 (stable legacy compatibility), or disabled")
	duplicates := flags.Int("duplicates", 3, "deliver each generated delta this many times")
	timeout := flags.Duration("timeout", 15*time.Second, "network timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	authToken, err := loadToken(*token, *tokenFile)
	protocol, protocolErr := parseRGAProtocol(*rgaProtocolName)
	if err != nil {
		return err
	}
	if protocolErr != nil {
		return protocolErr
	}
	if *replica == "" || *rgaRunes < 0 || *duplicates <= 0 || *timeout <= 0 {
		return errors.New("a non-empty -token or -token-file, -replica, positive -duplicates, and positive -timeout are required")
	}
	switch *mode {
	case "serve":
		if err := serve(*listen, *replica, authToken, protocol, *timeout, *allowNonLoopback); err != nil {
			return fmt.Errorf("serve probe: %w", err)
		}
		return nil
	case "send":
		if *target == "" || (*increment == 0 && *element == "" && *rgaRunes == 0) {
			return errors.New("-target and at least one non-empty mutation are required for send")
		}
		if *rgaRunes > 0 && protocol == rgaProtocolDisabled {
			return errors.New("-rga-runes requires run-v2 (the default) or explicit -rga-protocol=v1")
		}
		if err := send(*target, *replica, authToken, protocol, *increment, *element, *rgaRunes, *rgaRune, *duplicates, *timeout); err != nil {
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

func parseRGAProtocol(value string) (rgaProtocol, error) {
	switch rgaProtocol(value) {
	case rgaProtocolDisabled, rgaProtocolV1, rgaProtocolRunV2:
		return rgaProtocol(value), nil
	default:
		return "", errors.New("-rga-protocol must be disabled, v1, or run-v2")
	}
}

func newProbe(replicaID, token string, protocol rgaProtocol) (*probe, error) {
	if _, err := parseRGAProtocol(string(protocol)); err != nil {
		return nil, err
	}
	counterValue, err := counter.NewGCounter(replicaID)
	if err != nil {
		return nil, err
	}
	setValue, err := set.NewORSet(replicaID, stringCodec{})
	if err != nil {
		return nil, err
	}
	rgaValue, err := text.NewWithOptions(replicaID, rgaOptions())
	if err != nil {
		return nil, err
	}
	return &probe{
		counter:     counterValue,
		set:         setValue,
		rga:         rgaValue,
		rgaProtocol: protocol,
		token:       sha256.Sum256([]byte(token)),
	}, nil
}

func serve(listen, replicaID, token string, protocol rgaProtocol, timeout time.Duration, allowNonLoopback bool) error {
	if err := validateListenAddress(listen, allowNonLoopback); err != nil {
		return err
	}
	value, err := newProbe(replicaID, token, protocol)
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

func validateListenAddress(address string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if allowNonLoopback || strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("probe server must listen on loopback unless -allow-non-loopback is set")
	}
	return nil
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
	case request.Method == http.MethodPost && request.URL.Path == "/rga":
		p.applyRGA(writer, request)
	default:
		writeError(writer, http.StatusNotFound, errors.New("not found"))
	}
}

func (p *probe) authorized(request *http.Request) bool {
	provided := sha256.Sum256([]byte(request.Header.Get("X-CRDT-Probe-Token")))
	return subtle.ConstantTimeCompare(provided[:], p.token[:]) == 1
}

func (p *probe) applyCounter(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	encoded, err := readRequest(request, maxSmallRequestBytes)
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
	writeApplied(writer, started)
}

func (p *probe) applyORSet(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	encoded, err := readRequest(request, maxSmallRequestBytes)
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
	writeApplied(writer, started)
}

func (p *probe) applyRGA(writer http.ResponseWriter, request *http.Request) {
	if p.rgaProtocol == rgaProtocolDisabled {
		writeError(writer, http.StatusNotFound, errors.New("RGA transport is disabled"))
		return
	}
	started := time.Now()
	encoded, err := readRequest(request, maxRGARequestBytes)
	if err == nil {
		var decoded text.Delta
		decoded, err = unmarshalRGADelta(encoded, p.rgaProtocol, rgaTransportLimits())
		if err == nil {
			err = p.rga.ApplyDelta(decoded)
		}
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeApplied(writer, started)
}

func (p *probe) state() probeState {
	elements := p.set.Elements()
	sort.Strings(elements)
	value := p.rga.String()
	digest := sha256.Sum256([]byte(value))
	return probeState{
		Counts:   p.counter.Counts(),
		Elements: elements,
		Text: textState{
			Protocol: string(p.rgaProtocol),
			Runes:    utf8.RuneCountInString(value),
			SHA256:   fmt.Sprintf("%x", digest),
			Pending:  p.rga.PendingCount(),
		},
	}
}

func send(targetList, replicaID, token string, protocol rgaProtocol, increment uint64, element string, rgaRunes int, rgaRune string, duplicates int, timeout time.Duration) error {
	targets, err := parseTargets(targetList)
	if err != nil {
		return err
	}
	value, err := newProbe(replicaID, token, protocol)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	var counterDelta, setDelta, rgaDelta []byte
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
	if rgaRunes != 0 {
		if protocol == rgaProtocolDisabled {
			return errors.New("RGA delivery requires explicit v1 or run-v2 protocol")
		}
		rgaDelta, err = newRGADelta(replicaID, protocol, rgaRunes, rgaRune)
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
		if rgaDelta != nil {
			if err := postRepeated(client, baseURL+"/rga", token, rgaDelta, duplicates); err != nil {
				return fmt.Errorf("deliver RGA to %s: %w", baseURL, err)
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

func newRGADelta(replicaID string, protocol rgaProtocol, runes int, value string) ([]byte, error) {
	if runes <= 0 || runes > maxRGARunesPerDelivery {
		return nil, fmt.Errorf("RGA rune count must be in [1,%d]", maxRGARunesPerDelivery)
	}
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) != 1 {
		return nil, errors.New("RGA rune must be exactly one valid UTF-8 rune")
	}
	source, err := text.NewWithOptions(replicaID, rgaOptions())
	if err != nil {
		return nil, err
	}
	delta, err := source.Insert(0, strings.Repeat(value, runes))
	if err != nil {
		return nil, err
	}
	return marshalRGADelta(delta, protocol, rgaTransportLimits())
}

func marshalRGADelta(delta text.Delta, protocol rgaProtocol, limits frame.DecoderLimits) ([]byte, error) {
	switch protocol {
	case rgaProtocolV1:
		return delta.MarshalBinaryWithLimits(limits)
	case rgaProtocolRunV2:
		return delta.MarshalRunBinaryWithLimits(limits)
	default:
		return nil, errors.New("unsupported RGA protocol")
	}
}

func unmarshalRGADelta(data []byte, protocol rgaProtocol, limits frame.DecoderLimits) (text.Delta, error) {
	switch protocol {
	case rgaProtocolV1:
		return text.UnmarshalRGADeltaWithLimits(data, limits)
	case rgaProtocolRunV2:
		return text.UnmarshalRGARunDeltaWithLimits(data, limits)
	default:
		return text.Delta{}, errors.New("unsupported RGA protocol")
	}
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
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&state)
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
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		closeErr := response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("deliver delta: unexpected HTTP status %s", response.Status)
		}
	}
	return nil
}

func readRequest(request *http.Request, maxBytes int) (data []byte, err error) {
	if maxBytes <= 0 {
		return nil, errors.New("request body limit must be positive")
	}
	defer func() {
		if closeErr := request.Body.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	data, err = io.ReadAll(io.LimitReader(request.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxBytes {
		return nil, errors.New("request body is empty or exceeds transport limit")
	}
	return data, nil
}

func transportLimits() frame.DecoderLimits {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = maxSmallRequestBytes
	limits.MaxPayload = maxSmallRequestBytes - 1024
	limits.MaxElements = 1 << 16
	limits.MaxTags = 1 << 16
	limits.MaxStringBytes = 1 << 16
	return limits
}

func rgaTransportLimits() frame.DecoderLimits {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = maxRGARequestBytes
	limits.MaxPayload = maxRGARequestBytes - 1024
	limits.MaxElements = maxRGARunesPerDelivery
	limits.MaxTags = maxRGARunesPerDelivery
	limits.MaxStringBytes = 1 << 16
	return limits
}

func rgaOptions() text.Options {
	return text.Options{
		MaxNodes:        maxRGARunesPerDelivery * 2,
		MaxTombstones:   maxRGARunesPerDelivery * 2,
		MaxPendingNodes: maxRGARunesPerDelivery,
		MaxPendingBytes: maxRGARunesPerDelivery * 128,
	}
}

// writeApplied acknowledges a validated, idempotently applied delta without
// rebuilding complete state on the delivery hot path. Callers obtain canonical
// convergence data from the final /state request.
func writeApplied(writer http.ResponseWriter, started time.Time) {
	writer.Header().Set(applyDurationHeader, strconv.FormatInt(time.Since(started).Microseconds(), 10))
	writer.WriteHeader(http.StatusNoContent)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
