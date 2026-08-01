package extensions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// YJSStoreFormat pins a document to one Yjs binary update encoding. A
// document must never accept both encodings: V1 and V2 updates are different
// wire contracts even though they describe the same Yjs data model.
type YJSStoreFormat string

const (
	// YJSStoreFormatV1 is the update encoding used by the standard
	// y-websocket provider.
	YJSStoreFormatV1 YJSStoreFormat = "v1"
	// YJSStoreFormatV2 is Yjs's separately negotiated V2 update encoding.
	YJSStoreFormatV2 YJSStoreFormat = "v2"
)

const (
	maxYJSStoreUpdateBytes      = 64 << 20
	maxYJSStoreStateVectorBytes = 1 << 20
	maxYJSStoreSnapshotBytes    = 128 << 20
	maxYJSStoreMergeUpdates     = 1 << 16
)

var (
	// ErrYJSStoreLimit reports an update, state vector, snapshot, or merge
	// request that exceeds a caller-selected resource boundary.
	ErrYJSStoreLimit = errors.New("crdt extensions: Yjs store limit exceeded")
	// ErrYJSStoreRejected reports a syntactically invalid, wrong-format, or
	// semantically rejected Yjs request. The Yjs engine deliberately does not
	// return document bytes in this error.
	ErrYJSStoreRejected = errors.New("crdt extensions: Yjs store rejected request")
	// ErrYJSStoreUnavailable reports an unavailable or corrupt sidecar. It is
	// intentionally separate from ErrYJSStoreRejected so callers never treat a
	// failed durable write as a rejected client update.
	ErrYJSStoreUnavailable = errors.New("crdt extensions: Yjs store unavailable")
)

// YJSDocument is the immutable identity negotiated for one durable Yjs
// document. Tenant, room, epoch, schema, and format are all persisted by the
// sidecar and therefore cannot be selected independently by a client update.
//
// Epoch is part of the identity rather than an update field. A migration or
// destructive reset must create a new epoch; it must not mix updates from two
// histories under one state vector.
type YJSDocument struct {
	Tenant string
	Room   string
	Epoch  uint64
	Schema string
	Format YJSStoreFormat
}

// YJSStoreConfig configures the Go client for the Yjs-aware sidecar. Endpoint
// may be plain HTTP only for a loopback sidecar. Any non-loopback endpoint
// must use HTTPS, with network authentication and TLS lifecycle owned by the
// embedding application.
//
// The limits are enforced twice: before the Go client sends a request and by
// the sidecar before it decodes base64 or invokes Yjs. They are intentionally
// explicit so a caller cannot accidentally inherit unbounded document work.
type YJSStoreConfig struct {
	Endpoint            string
	Token               string
	MaxUpdateBytes      int
	MaxStateVectorBytes int
	MaxSnapshotBytes    int
	MaxMergeUpdates     int
	HTTPClient          *http.Client
}

// YJSApplyResult reports the durable recovery cursor after an update. Applied
// is false for an idempotent replay; in that case Cursor still names the
// already durable document state.
type YJSApplyResult struct {
	Applied     bool
	Cursor      uint64
	StateVector []byte
}

// YJSSnapshot is a complete, format-pinned recovery unit. Update is a Yjs
// merged snapshot, not a Go CRDT frame. StateVector and Cursor were observed
// atomically with the persisted snapshot by the sidecar.
type YJSSnapshot struct {
	Update      []byte
	StateVector []byte
	Cursor      uint64
}

// YJSStore provides semantic Yjs operations. Its implementation must be a
// maintained Yjs runtime; the Go package intentionally does not parse or
// translate Yjs updates into the repository's RGA protocols.
type YJSStore interface {
	Apply(context.Context, YJSDocument, []byte) (YJSApplyResult, error)
	StateVector(context.Context, YJSDocument) ([]byte, error)
	Diff(context.Context, YJSDocument, []byte) ([]byte, error)
	Snapshot(context.Context, YJSDocument) (YJSSnapshot, error)
	Merge(context.Context, YJSDocument, [][]byte) ([]byte, error)
}

type yjsStoreClient struct {
	endpoint            *url.URL
	token               string
	maxUpdateBytes      int
	maxStateVectorBytes int
	maxSnapshotBytes    int
	maxMergeUpdates     int
	httpClient          *http.Client
}

var _ YJSStore = (*yjsStoreClient)(nil)

// NewYJSStore creates a bounded client for the separately deployed Yjs
// semantic runtime. It never starts a Node process, persists document bytes,
// or exposes an HTTP endpoint from the Go process.
func NewYJSStore(config YJSStoreConfig) (YJSStore, error) {
	endpoint, err := validateYJSStoreEndpoint(config.Endpoint)
	if err != nil || !validYJSStoreToken(config.Token) ||
		config.MaxUpdateBytes <= 0 || config.MaxUpdateBytes > maxYJSStoreUpdateBytes ||
		config.MaxStateVectorBytes <= 0 || config.MaxStateVectorBytes > maxYJSStoreStateVectorBytes ||
		config.MaxSnapshotBytes < config.MaxUpdateBytes || config.MaxSnapshotBytes > maxYJSStoreSnapshotBytes ||
		config.MaxMergeUpdates <= 0 || config.MaxMergeUpdates > maxYJSStoreMergeUpdates {
		return nil, invalidConfig("extensions.new_yjs_store", ErrInvalidConfig)
	}
	client := newYJSStoreHTTPClient(config.HTTPClient)
	return &yjsStoreClient{
		endpoint:            endpoint,
		token:               config.Token,
		maxUpdateBytes:      config.MaxUpdateBytes,
		maxStateVectorBytes: config.MaxStateVectorBytes,
		maxSnapshotBytes:    config.MaxSnapshotBytes,
		maxMergeUpdates:     config.MaxMergeUpdates,
		httpClient:          client,
	}, nil
}

// newYJSStoreHTTPClient copies the caller's transport and timeout settings but
// never allows a bearer-authenticated request to follow a sidecar redirect.
// A YJSStore endpoint is a configured trust boundary, not a discovery URL.
func newYJSStoreHTTPClient(source *http.Client) *http.Client {
	if source == nil {
		source = &http.Client{Timeout: 10 * time.Second}
	}
	client := *source
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func (client *yjsStoreClient) Apply(ctx context.Context, document YJSDocument, update []byte) (YJSApplyResult, error) {
	if err := client.validateDocumentAndBytes(document, update, client.maxUpdateBytes, false); err != nil {
		return YJSApplyResult{}, err
	}
	var response yjsApplyResponse
	err := client.do(ctx, "apply", yjsApplyRequest{Document: document.yjsJSON(), Update: base64.StdEncoding.EncodeToString(update)}, &response, encodedYJSStoreBytes(client.maxStateVectorBytes)+4096)
	if err != nil {
		return YJSApplyResult{}, err
	}
	vector, err := decodeYJSStoreBytes(response.StateVector, client.maxStateVectorBytes)
	if err != nil {
		return YJSApplyResult{}, err
	}
	return YJSApplyResult{Applied: response.Applied, Cursor: response.Cursor, StateVector: vector}, nil
}

func (client *yjsStoreClient) StateVector(ctx context.Context, document YJSDocument) ([]byte, error) {
	if err := client.validateDocument(document); err != nil {
		return nil, err
	}
	var response yjsStateVectorResponse
	if err := client.do(ctx, "state-vector", yjsDocumentRequest{Document: document.yjsJSON()}, &response, encodedYJSStoreBytes(client.maxStateVectorBytes)+4096); err != nil {
		return nil, err
	}
	return decodeYJSStoreBytes(response.StateVector, client.maxStateVectorBytes)
}

func (client *yjsStoreClient) Diff(ctx context.Context, document YJSDocument, remoteVector []byte) ([]byte, error) {
	if err := client.validateDocumentAndBytes(document, remoteVector, client.maxStateVectorBytes, false); err != nil {
		return nil, err
	}
	var response yjsUpdateResponse
	if err := client.do(ctx, "diff", yjsDiffRequest{Document: document.yjsJSON(), StateVector: base64.StdEncoding.EncodeToString(remoteVector)}, &response, encodedYJSStoreBytes(client.maxSnapshotBytes)+4096); err != nil {
		return nil, err
	}
	return decodeYJSStoreBytes(response.Update, client.maxSnapshotBytes)
}

func (client *yjsStoreClient) Snapshot(ctx context.Context, document YJSDocument) (YJSSnapshot, error) {
	if err := client.validateDocument(document); err != nil {
		return YJSSnapshot{}, err
	}
	var response yjsSnapshotResponse
	maximum := encodedYJSStoreBytes(client.maxSnapshotBytes) + encodedYJSStoreBytes(client.maxStateVectorBytes) + 4096
	if err := client.do(ctx, "snapshot", yjsDocumentRequest{Document: document.yjsJSON()}, &response, maximum); err != nil {
		return YJSSnapshot{}, err
	}
	update, err := decodeYJSStoreBytes(response.Update, client.maxSnapshotBytes)
	if err != nil {
		return YJSSnapshot{}, err
	}
	vector, err := decodeYJSStoreBytes(response.StateVector, client.maxStateVectorBytes)
	if err != nil {
		return YJSSnapshot{}, err
	}
	return YJSSnapshot{Update: update, StateVector: vector, Cursor: response.Cursor}, nil
}

func (client *yjsStoreClient) Merge(ctx context.Context, document YJSDocument, updates [][]byte) ([]byte, error) {
	if err := client.validateDocument(document); err != nil {
		return nil, err
	}
	if len(updates) == 0 || len(updates) > client.maxMergeUpdates {
		return nil, ErrYJSStoreLimit
	}
	encoded := make([]string, len(updates))
	total := 0
	for index, update := range updates {
		if err := client.validateUpdate(update); err != nil {
			return nil, err
		}
		if len(update) > client.maxSnapshotBytes-total {
			return nil, ErrYJSStoreLimit
		}
		total += len(update)
		encoded[index] = base64.StdEncoding.EncodeToString(update)
	}
	var response yjsUpdateResponse
	if err := client.do(ctx, "merge", yjsMergeRequest{Document: document.yjsJSON(), Updates: encoded}, &response, encodedYJSStoreBytes(client.maxSnapshotBytes)+4096); err != nil {
		return nil, err
	}
	return decodeYJSStoreBytes(response.Update, client.maxSnapshotBytes)
}

func (client *yjsStoreClient) validateDocumentAndBytes(document YJSDocument, data []byte, maximum int, allowEmpty bool) error {
	if err := client.validateDocument(document); err != nil {
		return err
	}
	if len(data) > maximum || (!allowEmpty && len(data) == 0) {
		return ErrYJSStoreLimit
	}
	return nil
}

func (client *yjsStoreClient) validateUpdate(update []byte) error {
	if len(update) == 0 || len(update) > client.maxUpdateBytes {
		return ErrYJSStoreLimit
	}
	return nil
}

func (client *yjsStoreClient) validateDocument(document YJSDocument) error {
	if !validYJSStoreIdentifier(document.Tenant) || !validYJSStoreIdentifier(document.Room) || !validYJSStoreIdentifier(document.Schema) ||
		(document.Format != YJSStoreFormatV1 && document.Format != YJSStoreFormatV2) {
		return ErrYJSStoreRejected
	}
	return nil
}

func (client *yjsStoreClient) do(ctx context.Context, operation string, payload any, response any, maximumResponse int) error {
	if client == nil || client.endpoint == nil || client.httpClient == nil || maximumResponse <= 0 {
		return ErrYJSStoreUnavailable
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Yjs store request: %w", ErrYJSStoreRejected)
	}
	endpoint := *client.endpoint
	endpoint.Path = "/v1/yjs/" + operation
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build Yjs store request: %w", ErrYJSStoreUnavailable)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	result, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Yjs store: %w", ErrYJSStoreUnavailable)
	}
	defer func() { _ = result.Body.Close() }()
	if result.StatusCode >= http.StatusMultipleChoices && result.StatusCode < http.StatusBadRequest {
		return fmt.Errorf("yjs store redirected request: %w", ErrYJSStoreUnavailable)
	}
	maximum := maximumResponse
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		maximum = 4096
	}
	data, err := readYJSStoreResponse(result.Body, maximum)
	if err != nil {
		return err
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return yjsStoreHTTPError(operation, result.StatusCode, data)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("decode Yjs store response: %w", ErrYJSStoreUnavailable)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode Yjs store response: %w", ErrYJSStoreUnavailable)
	}
	return nil
}

func validateYJSStoreEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint == nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, ErrInvalidConfig
	}
	if endpoint.Scheme == "http" && !isYJSStoreLoopback(endpoint.Hostname()) {
		return nil, ErrInvalidConfig
	}
	endpoint.Path = ""
	return endpoint, nil
}

func isYJSStoreLoopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validYJSStoreIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == ':' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func validYJSStoreToken(value string) bool {
	if len(value) < 32 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func readYJSStoreResponse(source io.Reader, maximum int) ([]byte, error) {
	if maximum <= 0 {
		return nil, ErrYJSStoreUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(source, int64(maximum)+1))
	if err != nil {
		return nil, fmt.Errorf("read Yjs store response: %w", ErrYJSStoreUnavailable)
	}
	if len(data) > maximum {
		return nil, ErrYJSStoreLimit
	}
	return data, nil
}

func encodedYJSStoreBytes(length int) int {
	if length <= 0 {
		return 0
	}
	return base64.StdEncoding.EncodedLen(length)
}

func decodeYJSStoreBytes(encoded string, maximum int) ([]byte, error) {
	if maximum <= 0 || len(encoded) > encodedYJSStoreBytes(maximum) || encoded == "" {
		return nil, ErrYJSStoreLimit
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) > maximum || len(decoded) == 0 {
		return nil, ErrYJSStoreRejected
	}
	return decoded, nil
}

func yjsStoreHTTPError(operation string, status int, body []byte) error {
	var response struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &response) != nil {
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreUnavailable)
	}
	switch response.Code {
	case "unauthorized":
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrUnauthorized)
	case "limit_exceeded":
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreLimit)
	case "invalid_request", "invalid_update", "wrong_format":
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreRejected)
	case "corrupt_store", "unavailable":
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreUnavailable)
	default:
		if status >= http.StatusInternalServerError {
			return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreUnavailable)
		}
		return fmt.Errorf("yjs store %s failed: %w", operation, ErrYJSStoreRejected)
	}
}

type yjsStoreDocument struct {
	Tenant string         `json:"tenant"`
	Room   string         `json:"room"`
	Epoch  string         `json:"epoch"`
	Schema string         `json:"schema"`
	Format YJSStoreFormat `json:"format"`
}

func (document YJSDocument) yjsJSON() yjsStoreDocument {
	return yjsStoreDocument{Tenant: document.Tenant, Room: document.Room, Epoch: fmt.Sprintf("%d", document.Epoch), Schema: document.Schema, Format: document.Format}
}

type yjsDocumentRequest struct {
	Document yjsStoreDocument `json:"document"`
}

type yjsApplyRequest struct {
	Document yjsStoreDocument `json:"document"`
	Update   string           `json:"update"`
}

type yjsDiffRequest struct {
	Document    yjsStoreDocument `json:"document"`
	StateVector string           `json:"stateVector"`
}

type yjsMergeRequest struct {
	Document yjsStoreDocument `json:"document"`
	Updates  []string         `json:"updates"`
}

type yjsApplyResponse struct {
	Applied     bool   `json:"applied"`
	Cursor      uint64 `json:"cursor"`
	StateVector string `json:"stateVector"`
}

type yjsStateVectorResponse struct {
	StateVector string `json:"stateVector"`
}

type yjsUpdateResponse struct {
	Update string `json:"update"`
}

type yjsSnapshotResponse struct {
	Update      string `json:"update"`
	StateVector string `json:"stateVector"`
	Cursor      uint64 `json:"cursor"`
}
