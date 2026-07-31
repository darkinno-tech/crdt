package persistence

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	frame "github.com/DarkInno/crdt/encoding"
)

var fileMagic = [...]byte{'C', 'R', 'F', 'S'}

const fileVersion byte = 1

// FileConfig configures the single-file Store reference. MaxStoreBytes bounds
// every complete file before it is read into memory or atomically replaced.
// It is independent from Config.MaxRecordBytes because a file can contain
// multiple named checkpoints.
type FileConfig struct {
	Config
	MaxStoreBytes int
}

func (config FileConfig) normalized() (FileConfig, error) {
	if config.MaxStoreBytes <= 0 {
		return FileConfig{}, ErrInvalidConfig
	}
	normalized, err := config.Config.normalized()
	if err != nil {
		return FileConfig{}, err
	}
	config.Config = normalized
	return config, nil
}

// FileStore is a dependency-free local Store reference. It keeps canonical
// records in memory and atomically replaces one private file with fsync and
// rename on each Save. It is appropriate only when one active process owns a
// protected local path; unlike bbolt it provides no inter-process lock.
type FileStore struct {
	path    string
	config  FileConfig
	mu      sync.RWMutex
	records map[string][]byte
	closed  atomic.Bool
}

var _ Store = (*FileStore)(nil)

// OpenFile opens or creates a file-backed checkpoint Store at path. Existing
// files must be regular, private (at most 0600), and valid under config. The
// parent directory must already exist and be protected by the host.
func OpenFile(path string, config FileConfig) (*FileStore, error) {
	if path == "" {
		return nil, ErrInvalidConfig
	}
	var err error
	config, err = config.normalized()
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("file persistence store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, ErrInvalidConfig
	}
	records, err := loadFileRecords(path, config)
	if err != nil {
		return nil, err
	}
	return &FileStore{path: path, config: config, records: records}, nil
}

// Close marks the Store closed. FileStore has no open file descriptor; the
// method exists so callers can treat it like every other Store backend.
func (store *FileStore) Close() error {
	if store == nil || !store.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	store.mu.Lock()
	store.records = nil
	store.mu.Unlock()
	return nil
}

// Save validates and atomically replaces name's complete checkpoint. A Save
// does not coordinate any other application database or acknowledge a peer.
func (store *FileStore) Save(name string, checkpoint Checkpoint) error {
	if store == nil || store.closed.Load() {
		return ErrClosed
	}
	if !store.config.validName(name) {
		return ErrInvalidCheckpoint
	}
	normalized, err := normalizeCheckpoint(checkpoint, store.config.Config)
	if err != nil {
		return err
	}
	record, err := marshalCheckpoint(normalized, store.config.Config)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed.Load() {
		return ErrClosed
	}
	candidate := cloneRecords(store.records)
	candidate[name] = record
	encoded, err := marshalFileRecords(candidate, store.config)
	if err != nil {
		return err
	}
	replaced, err := replaceFile(store.path, encoded)
	if replaced {
		// A directory-sync failure occurs after rename. Keep the in-memory view
		// aligned with the file so a retry cannot erase a checkpoint that may
		// already survive a crash; callers still receive the durability error.
		store.records = candidate
	}
	if err != nil {
		return fmt.Errorf("save file persistence checkpoint: %w", err)
	}
	return nil
}

// Load returns one freshly validated checkpoint. found is false when name was
// never saved. Invalid stored bytes fail closed with ErrCorruptStore.
func (store *FileStore) Load(name string) (checkpoint Checkpoint, found bool, err error) {
	if store == nil || store.closed.Load() {
		return Checkpoint{}, false, ErrClosed
	}
	if !store.config.validName(name) {
		return Checkpoint{}, false, ErrInvalidCheckpoint
	}
	store.mu.RLock()
	if store.closed.Load() {
		store.mu.RUnlock()
		return Checkpoint{}, false, ErrClosed
	}
	record, ok := store.records[name]
	record = append([]byte(nil), record...)
	store.mu.RUnlock()
	if !ok {
		return Checkpoint{}, false, nil
	}
	checkpoint, version, err := decodeCheckpoint(record, store.config.Config)
	if err != nil {
		return Checkpoint{}, false, ErrCorruptStore
	}
	if store.config.Format.MigrateOnLoad && version != store.config.Format.Version {
		return store.migrateAndLoad(name)
	}
	return checkpoint, true, nil
}

// Delete atomically removes name's local recovery boundary by replacing the
// complete file. found is false when name was never saved. A successful delete
// does not retire relay data or CRDT tombstones; callers must enforce their own
// retention and rejoin policy before removing a checkpoint.
func (store *FileStore) Delete(name string) (found bool, err error) {
	if store == nil || store.closed.Load() {
		return false, ErrClosed
	}
	if !store.config.validName(name) {
		return false, ErrInvalidCheckpoint
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed.Load() {
		return false, ErrClosed
	}
	if _, found = store.records[name]; !found {
		return false, nil
	}
	candidate := cloneRecords(store.records)
	delete(candidate, name)
	encoded, err := marshalFileRecords(candidate, store.config)
	if err != nil {
		return false, err
	}
	replaced, err := replaceFile(store.path, encoded)
	if replaced {
		// See Save: after rename the store must retain the new view even when
		// parent-directory sync reports an error.
		store.records = candidate
	}
	if err != nil {
		return false, fmt.Errorf("delete file persistence checkpoint: %w", err)
	}
	return true, nil
}

func (store *FileStore) migrateAndLoad(name string) (checkpoint Checkpoint, found bool, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed.Load() {
		return Checkpoint{}, false, ErrClosed
	}
	record, found := store.records[name]
	if !found {
		return Checkpoint{}, false, nil
	}
	checkpoint, version, err := decodeCheckpoint(record, store.config.Config)
	if err != nil {
		return Checkpoint{}, false, ErrCorruptStore
	}
	if version == store.config.Format.Version {
		return checkpoint, true, nil
	}
	checkpoint, err = migrateCheckpoint(checkpoint, version, store.config.Config)
	if err != nil {
		return Checkpoint{}, false, ErrMigration
	}
	record, err = marshalCheckpoint(checkpoint, store.config.Config)
	if err != nil {
		return Checkpoint{}, false, ErrMigration
	}
	candidate := cloneRecords(store.records)
	candidate[name] = record
	encoded, err := marshalFileRecords(candidate, store.config)
	if err != nil {
		return Checkpoint{}, false, err
	}
	replaced, err := replaceFile(store.path, encoded)
	if replaced {
		store.records = candidate
	}
	if err != nil {
		return Checkpoint{}, false, fmt.Errorf("migrate file persistence checkpoint: %w", err)
	}
	return checkpoint, true, nil
}

func loadFileRecords(path string, config FileConfig) (map[string][]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string][]byte), nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect file persistence store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrInvalidConfig
	}
	if info.Size() > int64(config.MaxStoreBytes) {
		return nil, ErrCorruptStore
	}
	// #nosec G304 -- OpenFile accepts an application-owned path; Lstat above
	// rejects symlinks, non-regular files, and group/other-readable files.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file persistence store: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(config.MaxStoreBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read file persistence store: %w", err)
	}
	if len(data) > config.MaxStoreBytes {
		return nil, ErrCorruptStore
	}
	records, err := unmarshalFileRecords(data, config)
	if err != nil {
		return nil, ErrCorruptStore
	}
	return records, nil
}

func marshalFileRecords(records map[string][]byte, config FileConfig) ([]byte, error) {
	encoded := make([]byte, 0, minFileCapacity(config.MaxStoreBytes))
	encoded = append(encoded, fileMagic[:]...)
	encoded = append(encoded, fileVersion)
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	encoded = frame.AppendUvarint(encoded, uint64(len(names)))
	for _, name := range names {
		if !config.validName(name) {
			return nil, ErrInvalidCheckpoint
		}
		record := records[name]
		if _, _, err := decodeCheckpoint(record, config.Config); err != nil {
			return nil, ErrCorruptStore
		}
		encoded = appendBytes(encoded, []byte(name))
		encoded = appendBytes(encoded, record)
		if len(encoded) > config.MaxStoreBytes-sha256.Size {
			return nil, ErrInvalidCheckpoint
		}
	}
	digest := sha256.Sum256(encoded)
	encoded = append(encoded, digest[:]...)
	if len(encoded) > config.MaxStoreBytes {
		return nil, ErrInvalidCheckpoint
	}
	return encoded, nil
}

func unmarshalFileRecords(data []byte, config FileConfig) (map[string][]byte, error) {
	if len(data) < len(fileMagic)+1+1+sha256.Size || len(data) > config.MaxStoreBytes {
		return nil, ErrCorruptStore
	}
	payloadEnd := len(data) - sha256.Size
	digest := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(digest[:], data[payloadEnd:]) || !bytes.Equal(data[:len(fileMagic)], fileMagic[:]) || data[len(fileMagic)] != fileVersion {
		return nil, ErrCorruptStore
	}
	position := len(fileMagic) + 1
	count, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
	remaining := payloadEnd - position
	if !ok || remaining < 0 || count > uint64(uint(remaining)) {
		return nil, ErrCorruptStore
	}
	position = next
	records := make(map[string][]byte)
	previousName := ""
	for index := uint64(0); index < count; index++ {
		nameBytes, next, ok := frame.ReadBytes(data[:payloadEnd], position, config.MaxNameBytes)
		if !ok {
			return nil, ErrCorruptStore
		}
		name := string(nameBytes)
		if !config.validName(name) || name <= previousName {
			return nil, ErrCorruptStore
		}
		position = next
		record, next, ok := frame.ReadBytes(data[:payloadEnd], position, config.MaxRecordBytes)
		if !ok || len(record) == 0 {
			return nil, ErrCorruptStore
		}
		if _, _, err := decodeCheckpoint(record, config.Config); err != nil {
			return nil, ErrCorruptStore
		}
		records[name] = append([]byte(nil), record...)
		previousName = name
		position = next
	}
	if position != payloadEnd {
		return nil, ErrCorruptStore
	}
	return records, nil
}

func replaceFile(path string, data []byte) (replaced bool, err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".crdt-checkpoint-")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if written, err := temporary.Write(data); err != nil || written != len(data) {
		_ = temporary.Close()
		if err != nil {
			return false, err
		}
		return false, io.ErrShortWrite
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, err
	}
	// #nosec G304 -- directory is derived from the application-owned FileStore
	// path and was checked as an existing directory by OpenFile.
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return true, err
	}
	defer func() { _ = directoryHandle.Close() }()
	if err := directoryHandle.Sync(); err != nil {
		return true, err
	}
	return true, nil
}

func cloneRecords(records map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(records)+1)
	for name, record := range records {
		cloned[name] = append([]byte(nil), record...)
	}
	return cloned
}

func minFileCapacity(maximum int) int {
	if maximum < 4096 {
		return maximum
	}
	return 4096
}
