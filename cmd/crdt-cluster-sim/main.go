// Command crdt-cluster-sim exercises run-v2 RGA synchronization over real HTTP
// links. It is a short-lived test utility, not a production relay.
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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/text"
)

const (
	defaultListen       = "127.0.0.1:49801"
	maxRequestBytes     = 16 << 20
	maxResponseBytes    = 1 << 20
	maxLogicalUsers     = 64
	maxInsertRunes      = 1_024
	defaultRandomSeed   = 20_260_729
	applyDurationHeader = "X-CRDT-Apply-Micros"
	// maximumReceiverNodes admits three devices, each using the largest
	// supported logical-user workload, while remaining bounded per receiver.
	maximumReceiverNodes = maxLogicalUsers * maxInsertRunes * 3
)

type clusterServer struct {
	rga   *text.RGA
	token [sha256.Size]byte
}

type clusterState struct {
	CanonicalSHA256 string `json:"canonical_sha256"`
	TextSHA256      string `json:"text_sha256"`
	Runes           int    `json:"runes"`
	Pending         int    `json:"pending"`
}

type latencySummary struct {
	Count           int     `json:"count"`
	P50Milliseconds float64 `json:"p50_ms"`
	P95Milliseconds float64 `json:"p95_ms"`
	P99Milliseconds float64 `json:"p99_ms"`
	MaxMilliseconds float64 `json:"max_ms"`
}

type targetReport struct {
	State                           clusterState   `json:"state"`
	Deliveries                      int            `json:"deliveries"`
	Latency                         latencySummary `json:"latency"`
	ServerApplyLatency              latencySummary `json:"server_apply_latency"`
	VerificationLatencyMilliseconds float64        `json:"verification_latency_ms"`
}

type syncReport struct {
	Source        string                  `json:"source"`
	LogicalUsers  int                     `json:"logical_users"`
	DeltaFrames   int                     `json:"delta_frames"`
	Duplicates    int                     `json:"duplicates"`
	JitterMinimum string                  `json:"jitter_min"`
	JitterMaximum string                  `json:"jitter_max"`
	TargetReports map[string]targetReport `json:"targets"`
}

type delivery struct {
	data []byte
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("crdt-cluster-sim", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "", "serve or send")
	listen := flags.String("listen", defaultListen, "loopback server listen address")
	targetList := flags.String("target", "", "comma-separated target base URLs when mode=send")
	token := flags.String("token", "", "shared bearer token; prefer -token-file")
	tokenFile := flags.String("token-file", "", "path to a shared bearer token")
	replica := flags.String("replica", "", "non-empty device or user replica ID")
	users := flags.Int("users", 1, "logical users emitted by this source")
	insertRunes := flags.Int("insert-runes", 64, "runes inserted by each user before one cut")
	duplicates := flags.Int("duplicates", 2, "deliver each generated frame this many times")
	jitterMinimum := flags.Duration("jitter-min", 0, "minimum client-side delivery jitter")
	jitterMaximum := flags.Duration("jitter-max", 0, "maximum client-side delivery jitter")
	timeout := flags.Duration("timeout", 30*time.Second, "HTTP and server timeout")
	seed := flags.Int64("seed", defaultRandomSeed, "deterministic delivery shuffle seed")
	if err := flags.Parse(args); err != nil {
		return err
	}

	authToken, err := loadToken(*token, *tokenFile)
	if err != nil || *replica == "" || *timeout <= 0 {
		return errors.New("a non-empty -token or -token-file, -replica, and positive -timeout are required")
	}
	if *users <= 0 || *users > maxLogicalUsers || *insertRunes <= 0 || *insertRunes > maxInsertRunes || *duplicates <= 0 || *jitterMinimum < 0 || *jitterMaximum < *jitterMinimum {
		return fmt.Errorf("users must be in [1,%d], insert-runes in [1,%d], duplicates positive, and jitter range valid", maxLogicalUsers, maxInsertRunes)
	}

	switch *mode {
	case "serve":
		return serve(*listen, *replica, authToken, *timeout)
	case "send":
		if *targetList == "" {
			return errors.New("-target is required for send")
		}
		report, err := send(*targetList, *replica, authToken, *users, *insertRunes, *duplicates, *jitterMinimum, *jitterMaximum, *timeout, *seed)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
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

func serve(listen, replicaID, token string, timeout time.Duration) error {
	if err := validateLoopbackListen(listen); err != nil {
		return err
	}
	server, err := newClusterServer(replicaID, token)
	if err != nil {
		return err
	}
	return (&http.Server{
		Addr:              listen,
		Handler:           server,
		ReadHeaderTimeout: timeout,
		ReadTimeout:       timeout,
		WriteTimeout:      timeout,
		IdleTimeout:       timeout,
	}).ListenAndServe()
}

func validateLoopbackListen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("cluster simulation server must listen on a loopback address")
	}
	return nil
}

func newClusterServer(replicaID, token string) (*clusterServer, error) {
	rga, err := text.NewWithOptions(replicaID, receiverOptions())
	if err != nil {
		return nil, err
	}
	return &clusterServer{rga: rga, token: sha256.Sum256([]byte(token))}, nil
}

func (server *clusterServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writeError(writer, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/state":
		state, err := server.state()
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err)
			return
		}
		writeJSON(writer, http.StatusOK, state)
	case request.Method == http.MethodPost && request.URL.Path == "/rga":
		server.applyRGA(writer, request)
	default:
		writeError(writer, http.StatusNotFound, errors.New("not found"))
	}
}

func (server *clusterServer) authorized(request *http.Request) bool {
	provided := sha256.Sum256([]byte(request.Header.Get("X-CRDT-Cluster-Token")))
	return subtle.ConstantTimeCompare(provided[:], server.token[:]) == 1
}

func (server *clusterServer) applyRGA(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	encoded, err := readRequest(request, maxRequestBytes)
	if err == nil {
		var delta text.Delta
		delta, err = text.UnmarshalRGARunDeltaWithLimits(encoded, frameLimits())
		if err == nil {
			err = server.rga.ApplyDelta(delta)
		}
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writer.Header().Set(applyDurationHeader, strconv.FormatInt(time.Since(started).Microseconds(), 10))
	writer.WriteHeader(http.StatusNoContent)
}

func (server *clusterServer) state() (clusterState, error) {
	encoded, err := server.rga.MarshalRunBinary()
	if err != nil {
		return clusterState{}, err
	}
	value := server.rga.String()
	return clusterState{
		CanonicalSHA256: sha256Hex(encoded),
		TextSHA256:      sha256Hex([]byte(value)),
		Runes:           utf8.RuneCountInString(value),
		Pending:         server.rga.PendingCount(),
	}, nil
}

func send(targetList, replicaPrefix, token string, users, insertRunes, duplicates int, jitterMinimum, jitterMaximum, timeout time.Duration, seed int64) (syncReport, error) {
	targets, err := parseTargets(targetList)
	if err != nil {
		return syncReport{}, err
	}
	deliveries, err := buildDeliveries(replicaPrefix, users, insertRunes, duplicates)
	if err != nil {
		return syncReport{}, err
	}
	client := &http.Client{Timeout: timeout}
	reports := make(map[string]targetReport, len(targets))
	for targetIndex, target := range targets {
		order := shuffledOrder(len(deliveries), seed+int64(targetIndex))
		random := rand.New(rand.NewSource(seed + int64(targetIndex)*10_000))
		samples := make([]time.Duration, 0, len(deliveries))
		applySamples := make([]time.Duration, 0, len(deliveries))
		for _, position := range order {
			if delay := randomJitter(random, jitterMinimum, jitterMaximum); delay > 0 {
				time.Sleep(delay)
			}
			started := time.Now()
			applyLatency, err := postDelta(client, target+"/rga", token, deliveries[position].data)
			if err != nil {
				return syncReport{}, fmt.Errorf("deliver to %s: %w", target, err)
			}
			samples = append(samples, time.Since(started))
			applySamples = append(applySamples, applyLatency)
		}
		verificationStarted := time.Now()
		state, err := fetchState(client, target, token)
		if err != nil {
			return syncReport{}, fmt.Errorf("read state from %s: %w", target, err)
		}
		reports[target] = targetReport{
			State:                           state,
			Deliveries:                      len(deliveries),
			Latency:                         summarizeLatencies(samples),
			ServerApplyLatency:              summarizeLatencies(applySamples),
			VerificationLatencyMilliseconds: durationMilliseconds(time.Since(verificationStarted)),
		}
	}
	return syncReport{
		Source:        replicaPrefix,
		LogicalUsers:  users,
		DeltaFrames:   users * 2,
		Duplicates:    duplicates,
		JitterMinimum: jitterMinimum.String(),
		JitterMaximum: jitterMaximum.String(),
		TargetReports: reports,
	}, nil
}

func buildDeliveries(replicaPrefix string, users, insertRunes, duplicates int) ([]delivery, error) {
	if users <= 0 || users > maxLogicalUsers || insertRunes <= 0 || insertRunes > maxInsertRunes || duplicates <= 0 {
		return nil, fmt.Errorf("users must be in [1,%d], insert-runes in [1,%d], and duplicates positive", maxLogicalUsers, maxInsertRunes)
	}
	result := make([]delivery, 0, users*2*duplicates)
	for user := 0; user < users; user++ {
		source, err := text.NewWithOptions(fmt.Sprintf("%s-user-%03d", replicaPrefix, user), receiverOptions())
		if err != nil {
			return nil, err
		}
		value := strings.Repeat(string(rune('a'+user%26)), insertRunes)
		insert, err := source.Insert(0, value)
		if err != nil {
			return nil, err
		}
		cut, err := source.Delete(0, 1)
		if err != nil {
			return nil, err
		}
		insertData, err := insert.MarshalRunBinaryWithLimits(frameLimits())
		if err != nil {
			return nil, err
		}
		cutData, err := cut.MarshalRunBinaryWithLimits(frameLimits())
		if err != nil {
			return nil, err
		}
		for copy := 0; copy < duplicates; copy++ {
			// The shuffle in send may further reorder this pair. Starting with the
			// cut guarantees tombstone-before-node coverage even without jitter.
			result = append(result, delivery{data: cutData}, delivery{data: insertData})
		}
	}
	return result, nil
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

func postDelta(client *http.Client, endpoint, token string, data []byte) (time.Duration, error) {
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-CRDT-Cluster-Token", token)
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	closeErr := response.Body.Close()
	if readErr != nil {
		return 0, readErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if response.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	applyLatency, err := parseApplyLatency(response.Header.Get(applyDurationHeader))
	if err != nil {
		return 0, err
	}
	return applyLatency, nil
}

func parseApplyLatency(value string) (time.Duration, error) {
	microseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || microseconds < 0 || microseconds > int64((1<<63-1)/int64(time.Microsecond)) {
		return 0, errors.New("invalid server apply duration")
	}
	return time.Duration(microseconds) * time.Microsecond, nil
}

func fetchState(client *http.Client, baseURL, token string) (clusterState, error) {
	request, err := http.NewRequest(http.MethodGet, baseURL+"/state", nil)
	if err != nil {
		return clusterState{}, err
	}
	request.Header.Set("X-CRDT-Cluster-Token", token)
	response, err := client.Do(request)
	if err != nil {
		return clusterState{}, err
	}
	if response.StatusCode != http.StatusOK {
		if closeErr := response.Body.Close(); closeErr != nil {
			return clusterState{}, closeErr
		}
		return clusterState{}, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	var state clusterState
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&state)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return clusterState{}, decodeErr
	}
	if closeErr != nil {
		return clusterState{}, closeErr
	}
	return state, nil
}

func readRequest(request *http.Request, maximum int) (data []byte, err error) {
	defer func() {
		if closeErr := request.Body.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	data, err = io.ReadAll(io.LimitReader(request.Body, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maximum {
		return nil, errors.New("request body is empty or exceeds transport limit")
	}
	return data, nil
}

func receiverOptions() text.Options {
	return text.Options{
		MaxNodes:        maximumReceiverNodes,
		MaxTombstones:   maximumReceiverNodes,
		MaxPendingNodes: 1 << 16,
		MaxPendingBytes: 32 << 20,
	}
}

func frameLimits() frame.DecoderLimits {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = maxRequestBytes
	limits.MaxPayload = maxRequestBytes - 1024
	return limits
}

func shuffledOrder(count int, seed int64) []int {
	order := make([]int, count)
	for index := range order {
		order[index] = index
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(order), func(left, right int) {
		order[left], order[right] = order[right], order[left]
	})
	return order
}

func randomJitter(random *rand.Rand, minimum, maximum time.Duration) time.Duration {
	if maximum == 0 {
		return 0
	}
	if minimum == maximum {
		return minimum
	}
	return minimum + time.Duration(random.Int63n(int64(maximum-minimum)+1))
}

func summarizeLatencies(samples []time.Duration) latencySummary {
	if len(samples) == 0 {
		return latencySummary{}
	}
	sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
	return latencySummary{
		Count:           len(samples),
		P50Milliseconds: durationMilliseconds(percentile(samples, 50)),
		P95Milliseconds: durationMilliseconds(percentile(samples, 95)),
		P99Milliseconds: durationMilliseconds(percentile(samples, 99)),
		MaxMilliseconds: durationMilliseconds(samples[len(samples)-1]),
	}
}

func percentile(samples []time.Duration, percentage int) time.Duration {
	position := (len(samples)*percentage + 99) / 100
	return samples[position-1]
}

func durationMilliseconds(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
