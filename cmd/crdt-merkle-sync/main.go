// Command crdt-merkle-sync repairs bounded G-Counter state directories over
// authenticated HTTP by reconciling their Merkle roots. It is an operations
// tool, not a replacement for application authentication, durable outboxes,
// or a group-wide transaction protocol.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/merkle"
)

const (
	defaultListen         = "127.0.0.1:49821"
	defaultMaxStateBytes  = 1 << 20
	defaultMaxObjects     = 1_024
	defaultMaxElements    = 1 << 16
	defaultMaxStringBytes = 1 << 10
	maximumObjects        = 4_096

	maximumTokenBytes    = 1 << 10
	maximumObjectKeySize = 128
	maximumResponseBytes = 1 << 20
	stateFileExtension   = ".frame"
	temporaryDirectory   = ".crdt-merkle-tmp"
	apiVersion           = 1
	tokenHeader          = "X-CRDT-Merkle-Token"
)

var errStateNotFound = errors.New("state not found")

type storeOptions struct {
	maxStateBytes int
	maxObjects    int
	maxElements   int
	maxStringSize int
}

func defaultStoreOptions() storeOptions {
	return storeOptions{
		maxStateBytes: defaultMaxStateBytes,
		maxObjects:    defaultMaxObjects,
		maxElements:   defaultMaxElements,
		maxStringSize: defaultMaxStringBytes,
	}
}

func (options storeOptions) validate() error {
	defaults := frame.DefaultLimits()
	if options.maxStateBytes <= 0 || options.maxStateBytes > defaults.MaxFrameBytes {
		return fmt.Errorf("max-state-bytes must be in [1,%d]", defaults.MaxFrameBytes)
	}
	if options.maxObjects <= 0 || options.maxObjects > maximumObjects {
		return fmt.Errorf("max-objects must be in [1,%d]", maximumObjects)
	}
	if options.maxElements <= 0 || options.maxElements > defaults.MaxElements {
		return fmt.Errorf("max-elements must be in [1,%d]", defaults.MaxElements)
	}
	if options.maxStringSize <= 0 || options.maxStringSize > defaults.MaxStringBytes {
		return fmt.Errorf("max-string-bytes must be in [1,%d]", defaults.MaxStringBytes)
	}
	return nil
}

func (options storeOptions) decoderLimits() frame.DecoderLimits {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = options.maxStateBytes
	// A frame cannot carry more payload bytes than its complete bounded body.
	limits.MaxPayload = options.maxStateBytes
	limits.MaxElements = options.maxElements
	limits.MaxTags = options.maxElements
	limits.MaxStringBytes = options.maxStringSize
	return limits
}

// stateMerger provides a closed, concrete semantic merge for one state frame.
// A valid generic frame is deliberately not enough to admit a new type here.
type stateMerger interface {
	validate([]byte, frame.DecoderLimits) error
	merge([]byte, []byte, frame.DecoderLimits) ([]byte, error)
}

type gCounterMerger struct{}

func (gCounterMerger) validate(data []byte, limits frame.DecoderLimits) error {
	value, err := counter.NewGCounter("crdt-merkle-sync-validator")
	if err != nil {
		return err
	}
	return value.UnmarshalBinaryWithLimits(data, limits)
}

func (gCounterMerger) merge(left, right []byte, limits frame.DecoderLimits) ([]byte, error) {
	local, err := counter.NewGCounter("crdt-merkle-sync-merge")
	if err != nil {
		return nil, err
	}
	if err := local.UnmarshalBinaryWithLimits(left, limits); err != nil {
		return nil, err
	}
	remote, err := counter.NewGCounter("crdt-merkle-sync-merge")
	if err != nil {
		return nil, err
	}
	if err := remote.UnmarshalBinaryWithLimits(right, limits); err != nil {
		return nil, err
	}
	if err := local.Merge(remote); err != nil {
		return nil, err
	}
	return local.MarshalBinary()
}

var stateMergers = map[uint64]stateMerger{
	crdt.TypeIDGCounterState: gCounterMerger{},
}

type stateRecord struct {
	data   []byte
	typeID uint64
}

type inventoryEntry struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	TypeID uint64 `json:"type_id"`
}

type storeInventory struct {
	Root    string           `json:"root"`
	Entries []inventoryEntry `json:"entries"`
}

type stateStore struct {
	mu      sync.RWMutex
	dir     string
	options storeOptions
	states  map[string]stateRecord
	tree    *merkle.Tree
}

func openStateStore(directory string, options storeOptions) (*stateStore, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("state-dir is empty")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("stat state directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("state-dir is not a directory")
	}

	store := &stateStore{
		dir:     directory,
		options: options,
		states:  make(map[string]stateRecord),
		tree:    merkle.NewTree(),
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read state directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == temporaryDirectory && entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("state directory entry %q is not a regular state file", entry.Name())
		}
		key, err := keyFromStateFilename(entry.Name())
		if err != nil {
			return nil, err
		}
		if len(store.states) >= options.maxObjects {
			return nil, fmt.Errorf("state directory exceeds max-objects %d", options.maxObjects)
		}
		data, err := readBoundedFile(filepath.Join(directory, entry.Name()), options.maxStateBytes)
		if err != nil {
			return nil, fmt.Errorf("read state %q: %w", key, err)
		}
		typeID, err := validateState(data, options.decoderLimits())
		if err != nil {
			return nil, fmt.Errorf("validate state %q: %w", key, err)
		}
		store.states[key] = stateRecord{data: data, typeID: typeID}
		store.tree.Insert(key, data)
	}
	return store, nil
}

func (store *stateStore) inventory() storeInventory {
	store.mu.RLock()
	entries := make([]inventoryEntry, 0, len(store.states))
	for key, record := range store.states {
		digest := sha256.Sum256(record.data)
		entries = append(entries, inventoryEntry{
			Key:    key,
			SHA256: hex.EncodeToString(digest[:]),
			TypeID: record.typeID,
		})
	}
	root := store.tree.Root()
	store.mu.RUnlock()
	sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
	return storeInventory{Root: hex.EncodeToString(root[:]), Entries: entries}
}

func (store *stateStore) root() string {
	store.mu.RLock()
	root := store.tree.Root()
	store.mu.RUnlock()
	return hex.EncodeToString(root[:])
}

func (store *stateStore) rootResponse() rootResponse {
	store.mu.RLock()
	root := store.tree.Root()
	entries := len(store.states)
	store.mu.RUnlock()
	return rootResponse{Version: apiVersion, Root: hex.EncodeToString(root[:]), Entries: entries}
}

func (store *stateStore) state(key string) ([]byte, stateRecord, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, stateRecord{}, err
	}
	store.mu.RLock()
	record, exists := store.states[key]
	store.mu.RUnlock()
	if !exists {
		return nil, stateRecord{}, errStateNotFound
	}
	return append([]byte(nil), record.data...), stateRecord{typeID: record.typeID}, nil
}

// merge installs the CRDT join of key's state and incoming. It serializes the
// read-merge-write sequence so two HTTP clients cannot overwrite one another.
func (store *stateStore) merge(key string, incoming []byte) ([]byte, bool, error) {
	if err := validateObjectKey(key); err != nil {
		return nil, false, err
	}
	incomingType, err := validateState(incoming, store.options.decoderLimits())
	if err != nil {
		return nil, false, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.states[key]
	if !exists && len(store.states) >= store.options.maxObjects {
		return nil, false, fmt.Errorf("state directory exceeds max-objects %d", store.options.maxObjects)
	}
	merged := append([]byte(nil), incoming...)
	if exists {
		if current.typeID != incomingType {
			return nil, false, fmt.Errorf("state type mismatch for key %q: %d != %d", key, current.typeID, incomingType)
		}
		merger := stateMergers[incomingType]
		merged, err = merger.merge(current.data, incoming, store.options.decoderLimits())
		if err != nil {
			return nil, false, fmt.Errorf("merge state %q: %w", key, err)
		}
	}
	if bytes.Equal(current.data, merged) {
		return append([]byte(nil), current.data...), false, nil
	}
	if err := store.writeLocked(key, merged); err != nil {
		return nil, false, err
	}
	store.states[key] = stateRecord{data: append([]byte(nil), merged...), typeID: incomingType}
	store.tree.Insert(key, merged)
	return append([]byte(nil), merged...), true, nil
}

func (store *stateStore) writeLocked(key string, data []byte) error {
	temporaryPath := filepath.Join(store.dir, temporaryDirectory)
	if err := os.MkdirAll(temporaryPath, 0o700); err != nil {
		return fmt.Errorf("create temporary state directory: %w", err)
	}
	temporaryFile, err := os.CreateTemp(temporaryPath, "state-*")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	temporaryName := temporaryFile.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporaryFile.Write(data); err != nil {
		if closeErr := temporaryFile.Close(); closeErr != nil {
			return fmt.Errorf("write temporary state: %w (close temporary state: %v)", err, closeErr)
		}
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		if closeErr := temporaryFile.Close(); closeErr != nil {
			return fmt.Errorf("sync temporary state: %w (close temporary state: %v)", err, closeErr)
		}
		return fmt.Errorf("sync temporary state: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(temporaryName, filepath.Join(store.dir, key+stateFileExtension)); err != nil {
		return fmt.Errorf("replace state %q: %w", key, err)
	}
	directory, err := os.Open(store.dir)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync state directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close state directory: %w", closeErr)
	}
	return nil
}

func validateState(data []byte, limits frame.DecoderLimits) (uint64, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return 0, err
	}
	merger, exists := stateMergers[decoded.TypeID]
	if !exists {
		return 0, fmt.Errorf("unsupported state type ID %d", decoded.TypeID)
	}
	if err := merger.validate(data, limits); err != nil {
		return 0, err
	}
	return decoded.TypeID, nil
}

func keyFromStateFilename(name string) (string, error) {
	if !strings.HasSuffix(name, stateFileExtension) {
		return "", fmt.Errorf("state directory entry %q must end in %s", name, stateFileExtension)
	}
	key := strings.TrimSuffix(name, stateFileExtension)
	if err := validateObjectKey(key); err != nil {
		return "", fmt.Errorf("invalid state filename %q: %w", name, err)
	}
	return key, nil
}

func validateObjectKey(key string) error {
	if len(key) == 0 || len(key) > maximumObjectKeySize || key == "." || key == ".." {
		return fmt.Errorf("object key must contain 1 to %d safe bytes", maximumObjectKeySize)
	}
	for _, value := range key {
		alphaNumeric := value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
		if !alphaNumeric && value != '-' && value != '_' && value != '.' {
			return fmt.Errorf("object key %q contains an unsafe character", key)
		}
	}
	return nil
}

type rootResponse struct {
	Version int    `json:"version"`
	Root    string `json:"root"`
	Entries int    `json:"entries"`
}

type inventoryResponse struct {
	Version int              `json:"version"`
	Root    string           `json:"root"`
	Entries []inventoryEntry `json:"entries"`
}

type syncReport struct {
	InitialLocalRoot    string `json:"initial_local_root"`
	InitialRemoteRoot   string `json:"initial_remote_root"`
	FinalRoot           string `json:"final_root"`
	AlreadyEqual        bool   `json:"already_equal"`
	RemoteStatesFetched int    `json:"remote_states_fetched"`
	LocalStateWrites    int    `json:"local_state_writes"`
	RemoteMergeRequests int    `json:"remote_merge_requests"`
}

type syncServer struct {
	store *stateStore
	token [sha256.Size]byte
}

func newSyncServer(store *stateStore, token string) (*syncServer, error) {
	if store == nil || strings.TrimSpace(token) == "" {
		return nil, errors.New("store and token are required")
	}
	return &syncServer{store: store, token: sha256.Sum256([]byte(token))}, nil
}

func (server *syncServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !server.authorized(request) {
		writeError(writer, http.StatusUnauthorized, errors.New("unauthorized"))
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v1/merkle/root":
		writeJSON(writer, http.StatusOK, server.store.rootResponse())
	case request.Method == http.MethodGet && request.URL.Path == "/v1/merkle/inventory":
		inventory := server.store.inventory()
		writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: inventory.Root, Entries: inventory.Entries})
	case strings.HasPrefix(request.URL.Path, "/v1/state/"):
		server.handleState(writer, request)
	default:
		writeError(writer, http.StatusNotFound, errors.New("not found"))
	}
}

func (server *syncServer) authorized(request *http.Request) bool {
	provided := sha256.Sum256([]byte(request.Header.Get(tokenHeader)))
	return subtle.ConstantTimeCompare(provided[:], server.token[:]) == 1
}

func (server *syncServer) handleState(writer http.ResponseWriter, request *http.Request) {
	key := strings.TrimPrefix(request.URL.Path, "/v1/state/")
	if err := validateObjectKey(key); err != nil {
		writeError(writer, http.StatusNotFound, errors.New("not found"))
		return
	}
	switch request.Method {
	case http.MethodGet:
		data, _, err := server.store.state(key)
		if errors.Is(err, errStateNotFound) {
			writeError(writer, http.StatusNotFound, errStateNotFound)
			return
		}
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		digest := sha256.Sum256(data)
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("X-CRDT-State-SHA256", hex.EncodeToString(digest[:]))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(data)
	case http.MethodPut:
		data, err := readRequestBody(request, server.store.options.maxStateBytes)
		if err == nil {
			_, _, err = server.store.merge(key, data)
		}
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeError(writer, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("crdt-merkle-sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "", "serve, sync, or gcounter-add")
	stateDirectory := flags.String("state-dir", "", "directory containing one bounded G-Counter state frame per key")
	listen := flags.String("listen", defaultListen, "loopback server listen address")
	target := flags.String("target", "", "remote HTTP(S) base URL when mode=sync")
	token := flags.String("token", "", "shared token; prefer -token-file to avoid process argument exposure")
	tokenFile := flags.String("token-file", "", "path to a file containing the shared token")
	timeout := flags.Duration("timeout", 15*time.Second, "HTTP and server timeout")
	allowNonLoopback := flags.Bool("allow-non-loopback", false, "permit a non-loopback HTTP listener")
	maxStateBytes := flags.Int("max-state-bytes", defaultMaxStateBytes, "maximum bytes in one state frame")
	maxObjects := flags.Int("max-objects", defaultMaxObjects, "maximum state files in state-dir")
	maxElements := flags.Int("max-elements", defaultMaxElements, "maximum G-Counter components per frame")
	maxStringBytes := flags.Int("max-string-bytes", defaultMaxStringBytes, "maximum G-Counter replica ID bytes")
	key := flags.String("key", "", "state object key for gcounter-add")
	replica := flags.String("replica", "", "G-Counter replica ID for gcounter-add")
	amount := flags.Uint64("amount", 1, "positive G-Counter increment for gcounter-add")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	options := storeOptions{
		maxStateBytes: *maxStateBytes,
		maxObjects:    *maxObjects,
		maxElements:   *maxElements,
		maxStringSize: *maxStringBytes,
	}
	store, err := openStateStore(*stateDirectory, options)
	if err != nil {
		return err
	}

	switch *mode {
	case "serve":
		authToken, err := loadToken(*token, *tokenFile)
		if err != nil {
			return err
		}
		return serve(*listen, authToken, *timeout, *allowNonLoopback, store)
	case "sync":
		authToken, err := loadToken(*token, *tokenFile)
		if err != nil {
			return err
		}
		endpoint, err := parseTarget(*target)
		if err != nil {
			return err
		}
		report, err := synchronize(&http.Client{Timeout: *timeout}, endpoint, authToken, store)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(report)
	case "gcounter-add":
		if *amount == 0 || strings.TrimSpace(*replica) == "" {
			return errors.New("gcounter-add requires non-empty -replica and positive -amount")
		}
		if err := addGCounter(store, *key, *replica, *amount); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(store.inventory())
	default:
		return errors.New("-mode must be serve, sync, or gcounter-add")
	}
}

func serve(listen, token string, timeout time.Duration, allowNonLoopback bool, store *stateStore) error {
	if err := validateListenAddress(listen, allowNonLoopback); err != nil {
		return err
	}
	server, err := newSyncServer(store, token)
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
		return errors.New("server must listen on loopback unless -allow-non-loopback is set")
	}
	return nil
}

func addGCounter(store *stateStore, key, replicaID string, amount uint64) error {
	if err := validateObjectKey(key); err != nil {
		return err
	}
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		return err
	}
	if current, record, err := store.state(key); err == nil {
		if record.typeID != crdt.TypeIDGCounterState {
			return fmt.Errorf("state type mismatch for key %q", key)
		}
		if err := value.UnmarshalBinaryWithLimits(current, store.options.decoderLimits()); err != nil {
			return err
		}
	} else if !errors.Is(err, errStateNotFound) {
		return err
	}
	if _, err := value.Increment(amount); err != nil {
		return err
	}
	state, err := value.MarshalBinary()
	if err != nil {
		return err
	}
	_, _, err = store.merge(key, state)
	return err
}

func synchronize(client *http.Client, target, token string, local *stateStore) (syncReport, error) {
	if client == nil || local == nil {
		return syncReport{}, errors.New("HTTP client and local store are required")
	}
	initialLocalRoot := local.root()
	remoteRoot, err := fetchRoot(client, target, token)
	if err != nil {
		return syncReport{}, fmt.Errorf("fetch remote Merkle root: %w", err)
	}
	report := syncReport{InitialLocalRoot: initialLocalRoot, InitialRemoteRoot: remoteRoot.Root}
	if initialLocalRoot == remoteRoot.Root {
		report.AlreadyEqual = true
		report.FinalRoot = initialLocalRoot
		return report, nil
	}

	remoteInventory, err := fetchInventory(client, target, token)
	if err != nil {
		return syncReport{}, fmt.Errorf("fetch remote inventory: %w", err)
	}
	if remoteInventory.Root != remoteRoot.Root || len(remoteInventory.Entries) != remoteRoot.Entries {
		return syncReport{}, errors.New("remote Merkle root changed during inventory discovery; retry sync")
	}
	remoteEntries, err := inventoryIndex(remoteInventory)
	if err != nil {
		return syncReport{}, fmt.Errorf("validate remote inventory: %w", err)
	}
	localEntries := inventoryIndexFromStore(local.inventory())
	keys := combinedKeys(localEntries, remoteEntries)
	for _, key := range keys {
		localEntry, localExists := localEntries[key]
		remoteEntry, remoteExists := remoteEntries[key]
		switch {
		case !localExists:
			state, err := fetchState(client, target, token, remoteEntry, local.options)
			if err != nil {
				return syncReport{}, fmt.Errorf("fetch remote state %q: %w", key, err)
			}
			report.RemoteStatesFetched++
			if _, changed, err := local.merge(key, state); err != nil {
				return syncReport{}, fmt.Errorf("merge remote-only state %q locally: %w", key, err)
			} else if changed {
				report.LocalStateWrites++
			}
		case !remoteExists:
			state, _, err := local.state(key)
			if err != nil {
				return syncReport{}, fmt.Errorf("read local-only state %q: %w", key, err)
			}
			if err := putState(client, target, token, key, state); err != nil {
				return syncReport{}, fmt.Errorf("merge local-only state %q remotely: %w", key, err)
			}
			report.RemoteMergeRequests++
		case localEntry.SHA256 != remoteEntry.SHA256:
			remoteState, err := fetchState(client, target, token, remoteEntry, local.options)
			if err != nil {
				return syncReport{}, fmt.Errorf("fetch divergent state %q: %w", key, err)
			}
			report.RemoteStatesFetched++
			merged, changed, err := local.merge(key, remoteState)
			if err != nil {
				return syncReport{}, fmt.Errorf("merge divergent state %q locally: %w", key, err)
			}
			if changed {
				report.LocalStateWrites++
			}
			if err := putState(client, target, token, key, merged); err != nil {
				return syncReport{}, fmt.Errorf("merge divergent state %q remotely: %w", key, err)
			}
			report.RemoteMergeRequests++
		}
	}

	finalLocalRoot := local.root()
	finalRemote, err := fetchRoot(client, target, token)
	if err != nil {
		return syncReport{}, fmt.Errorf("fetch final remote Merkle root: %w", err)
	}
	if finalLocalRoot != finalRemote.Root {
		return syncReport{}, errors.New("merkle roots still differ after repair; concurrent writes or a failed semantic merge require retry")
	}
	report.FinalRoot = finalLocalRoot
	return report, nil
}

func inventoryIndexFromStore(inventory storeInventory) map[string]inventoryEntry {
	result := make(map[string]inventoryEntry, len(inventory.Entries))
	for _, entry := range inventory.Entries {
		result[entry.Key] = entry
	}
	return result
}

func inventoryIndex(inventory inventoryResponse) (map[string]inventoryEntry, error) {
	if inventory.Version != apiVersion || !validDigest(inventory.Root) {
		return nil, errors.New("unsupported or malformed inventory")
	}
	if len(inventory.Entries) > maximumObjects {
		return nil, fmt.Errorf("inventory exceeds maximum %d objects", maximumObjects)
	}
	result := make(map[string]inventoryEntry, len(inventory.Entries))
	previous := ""
	for _, entry := range inventory.Entries {
		if err := validateObjectKey(entry.Key); err != nil || !validDigest(entry.SHA256) {
			return nil, errors.New("inventory contains an invalid entry")
		}
		if entry.Key <= previous {
			return nil, errors.New("inventory entries are not strictly sorted")
		}
		if _, supported := stateMergers[entry.TypeID]; !supported {
			return nil, fmt.Errorf("inventory contains unsupported state type ID %d", entry.TypeID)
		}
		result[entry.Key] = entry
		previous = entry.Key
	}
	return result, nil
}

func combinedKeys(left, right map[string]inventoryEntry) []string {
	keys := make([]string, 0, len(left)+len(right))
	for key := range left {
		keys = append(keys, key)
	}
	for key := range right {
		if _, exists := left[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func fetchRoot(client *http.Client, target, token string) (rootResponse, error) {
	var result rootResponse
	if err := getJSON(client, target+"/v1/merkle/root", token, &result); err != nil {
		return rootResponse{}, err
	}
	if result.Version != apiVersion || result.Entries < 0 || result.Entries > maximumObjects || !validDigest(result.Root) {
		return rootResponse{}, errors.New("malformed remote Merkle root response")
	}
	return result, nil
}

func fetchInventory(client *http.Client, target, token string) (inventoryResponse, error) {
	var result inventoryResponse
	if err := getJSON(client, target+"/v1/merkle/inventory", token, &result); err != nil {
		return inventoryResponse{}, err
	}
	return result, nil
}

func fetchState(client *http.Client, target, token string, expected inventoryEntry, options storeOptions) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, target+"/v1/state/"+expected.Key, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(tokenHeader, token)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	data, readErr := readHTTPBody(response, options.maxStateBytes)
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expected.SHA256 || response.Header.Get("X-CRDT-State-SHA256") != expected.SHA256 {
		return nil, errors.New("remote state digest does not match inventory")
	}
	typeID, err := validateState(data, options.decoderLimits())
	if err != nil {
		return nil, err
	}
	if typeID != expected.TypeID {
		return nil, errors.New("remote state type does not match inventory")
	}
	return data, nil
}

func putState(client *http.Client, target, token, key string, state []byte) error {
	request, err := http.NewRequest(http.MethodPut, target+"/v1/state/"+key, bytes.NewReader(state))
	if err != nil {
		return err
	}
	request.Header.Set(tokenHeader, token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_, readErr := readHTTPBody(response, maximumResponseBytes)
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	return nil
}

func getJSON(client *http.Client, endpoint, token string, target any) error {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set(tokenHeader, token)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	data, readErr := readHTTPBody(response, maximumResponseBytes)
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	return nil
}

func readHTTPBody(response *http.Response, maximum int) ([]byte, error) {
	data, readErr := readLimited(response.Body, maximum)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readBoundedFile(path string, maximum int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := readBounded(file, maximum)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readBounded(reader io.Reader, maximum int) ([]byte, error) {
	data, err := readLimited(reader, maximum)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("body is empty or exceeds %d bytes", maximum)
	}
	return data, nil
}

func readRequestBody(request *http.Request, maximum int) (data []byte, err error) {
	data, readErr := readBounded(request.Body, maximum)
	closeErr := request.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readLimited(reader io.Reader, maximum int) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("maximum byte limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximum {
		return nil, fmt.Errorf("body exceeds %d bytes", maximum)
	}
	return data, nil
}

func loadToken(value, path string) (string, error) {
	if value != "" && path != "" {
		return "", errors.New("token and token-file are mutually exclusive")
	}
	if path == "" {
		if strings.TrimSpace(value) == "" {
			return "", errors.New("token is empty")
		}
		return value, nil
	}
	data, err := readBoundedFile(path, maximumTokenBytes)
	if err != nil {
		return "", err
	}
	if len(data) > maximumTokenBytes {
		return "", errors.New("token file exceeds 1024 bytes")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("token is empty")
	}
	return token, nil
}

func parseTarget(value string) (string, error) {
	target := strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.ParseRequestURI(target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid target %q", value)
	}
	return target, nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}
