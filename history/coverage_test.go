package history

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/snapshot"
)

func TestHistoryHelperBoundaries(t *testing.T) {
	options := DefaultOptions()
	if _, err := NewManager(nil, options); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil executor = %v", err)
	}
	if _, err := NewManager(newTestExecutor(), Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid manager options = %v", err)
	}
	if err := validateScope("", options.MaxScopeBytes); err == nil {
		t.Fatal("empty scope accepted")
	}
	if err := validateScope("bad\nvalue", options.MaxScopeBytes); err == nil {
		t.Fatal("control scope accepted")
	}
	if err := validateScope("valid/scope", options.MaxScopeBytes); err != nil {
		t.Fatal(err)
	}
	if validCommand(nil, options.MaxCommandBytes) || validCommand(make([]byte, options.MaxCommandBytes+1), options.MaxCommandBytes) {
		t.Fatal("invalid command accepted")
	}
	if _, ok := appendBoundedBytes([]byte{1}, []byte{2}, 1); ok {
		t.Fatal("bounded bytes overflow accepted")
	}
	if _, ok := appendBoundedUvarint([]byte{1}, 2, 1); ok {
		t.Fatal("bounded varint overflow accepted")
	}
	if value, ok := decodeCount(2, 2); !ok || value != 2 {
		t.Fatalf("bounded count = %d, %t", value, ok)
	}
	if _, ok := decodeCount(3, 2); ok {
		t.Fatal("over-limit count accepted")
	}
	if _, ok := decodeCount(0, -1); ok {
		t.Fatal("negative count limit accepted")
	}
	if _, _, ok := readID([]byte{1}, 0); ok {
		t.Fatal("short ID accepted")
	}
	if _, err := appendEntries(nil, []entry{{scope: "scope", undo: nil, redo: []byte("redo")}}, options); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("invalid entry = %v", err)
	}
	limited := options
	limited.MaxStateBytes = len(historyMagic) + 1 + sha256.Size
	if _, err := marshalManager(nil, nil, limited); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("tiny manager record = %v", err)
	}
	if _, _, err := unmarshalManager(nil, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("empty manager record = %v", err)
	}
	if _, _, err := readEntries([]byte{0x80}, 0, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("truncated entries = %v", err)
	}
	var nilManager *Manager
	if nilManager.CanUndo() || nilManager.CanRedo() || nilManager.Len() != 0 {
		t.Fatal("nil manager state mismatch")
	}
	if _, err := nilManager.MarshalBinary(); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil manager marshal = %v", err)
	}
	if _, err := nilManager.Undo(); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil manager undo = %v", err)
	}
	if _, err := nilManager.Redo(); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil manager redo = %v", err)
	}
	nilManager.Clear()
	if _, err := (ExecutorFunc(nil)).Execute("scope", []byte("command")); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil executor function = %v", err)
	}
	if _, err := NewManagerFromBinary(nil, options, []byte("bad")); !errors.Is(err, ErrExecutor) {
		t.Fatalf("nil restored executor = %v", err)
	}
}

func TestManagerRejectsInvalidExecutorResultsWithoutMovingStacks(t *testing.T) {
	invalid := ExecutorFunc(func(string, []byte) (Result, error) { return Result{}, nil })
	manager, err := NewManager(invalid, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute("text/body", []byte("forward")); !errors.Is(err, ErrInvalidCommand) || manager.Len() != 0 {
		t.Fatalf("invalid forward result = %v len=%d", err, manager.Len())
	}

	stage := 0
	manager, err = NewManager(ExecutorFunc(func(_ string, command []byte) (Result, error) {
		switch string(command) {
		case "forward":
			return Result{Reverse: []byte("undo")}, nil
		case "undo":
			stage++
			return Result{Reverse: []byte("redo")}, nil
		case "redo":
			stage++
			return Result{}, nil
		default:
			return Result{}, errors.New("unexpected command")
		}
	}), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute("text/body", []byte("forward")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Undo(); err != nil || stage != 1 || !manager.CanRedo() {
		t.Fatalf("undo result = %v stage=%d", err, stage)
	}
	if _, err := manager.Redo(); !errors.Is(err, ErrInvalidCommand) || manager.CanUndo() || !manager.CanRedo() || stage != 2 {
		t.Fatalf("invalid redo result = %v undo=%t redo=%t stage=%d", err, manager.CanUndo(), manager.CanRedo(), stage)
	}
}

func TestRepositoryHelperBoundaries(t *testing.T) {
	options := DefaultRepositoryOptions()
	if _, err := NewRepository(RepositoryOptions{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid repository options = %v", err)
	}
	if _, err := NewRepositoryFromBinary(RepositoryOptions{}, []byte("bad")); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid restore options = %v", err)
	}
	if _, err := normalizeState(State{}, options); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("empty state = %v", err)
	}
	if _, err := normalizeState(State{Snapshots: []Snapshot{{Scope: "bad scope"}}}, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bad scope state = %v", err)
	}
	if _, err := normalizeSnapshot(snapshot.Snapshot{}, options); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("empty snapshot = %v", err)
	}
	if _, err := appendState(nil, State{}, options, 1); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("empty append state = %v", err)
	}
	if _, _, err := readState([]byte{0}, 0, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero snapshot state = %v", err)
	}
	if parents := canonicalParents([]ID{{1}, {1}, {}}); len(parents) != 1 || !parents[0].valid() {
		t.Fatalf("canonical parents = %#v", parents)
	}
	if parentsCanonical([]ID{{2}, {1}}) || parentsCanonical([]ID{{}}) {
		t.Fatal("non-canonical parents accepted")
	}
	if _, err := versionID([]ID{{}}, State{}, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bad version ID = %v", err)
	}

	repository, err := NewRepository(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{9}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unknown branch source = %v", err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate branch = %v", err)
	}
	if err := repository.Fork("copy", "missing"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing fork source = %v", err)
	}
	if _, err := repository.Merge("main", "missing", State{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing merge source = %v", err)
	}
	if _, ok := repository.Version(ID{1}); ok {
		t.Fatal("unknown version found")
	}
	if repository.Len() != 0 || (&Repository{}).Len() != 0 || (&Repository{}).History("main") != nil {
		t.Fatal("empty repository state mismatch")
	}
	if _, err := (*Repository)(nil).Commit("main", State{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil commit = %v", err)
	}
	if _, err := (*Repository)(nil).MarshalBinary(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil marshal = %v", err)
	}
	if got := (ID{1, 2}).String(); got[:4] != "0102" {
		t.Fatalf("ID string = %q", got)
	}
}

func TestHistoryDecodersExerciseCanonicalRejections(t *testing.T) {
	managerBytes, err := marshalManager([]entry{{scope: "list/tasks", undo: []byte("pop:x"), redo: []byte("push:x")}}, nil, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < len(managerBytes)-sha256.Size; index++ {
		mutated := append([]byte(nil), managerBytes...)
		mutated[index] ^= 0x01
		resign(mutated)
		_, _, _ = unmarshalManager(mutated, DefaultOptions())
	}

	repositoryBytes := mustRepositoryBytes(t)
	for index := 0; index < len(repositoryBytes)-sha256.Size; index++ {
		mutated := append([]byte(nil), repositoryBytes...)
		mutated[index] ^= 0x01
		resign(mutated)
		_, _, _, _ = unmarshalRepository(mutated, DefaultRepositoryOptions())
	}
}

func TestSnapshotNormalizationClockAndFrameBoundaries(t *testing.T) {
	state, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	withoutClock, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeSnapshot(withoutClock, DefaultRepositoryOptions()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("HLC snapshot without clock = %v", err)
	}
	withClock, err := snapshot.NewWithClockState(state, nil, clock.State{ReplicaID: "writer", WallTime: 1})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultRepositoryOptions()
	options.MaxReplicaIDBytes = 1
	if _, err := normalizeSnapshot(withClock, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("long clock replica ID = %v", err)
	}
	validOptions := DefaultRepositoryOptions()
	if _, err := normalizeSnapshot(withClock, validOptions); err != nil {
		t.Fatalf("valid HLC snapshot = %v", err)
	}
	wrongHeader := withClock
	wrongHeader.TypeID = crdt.TypeIDGCounterState
	if _, err := normalizeSnapshot(wrongHeader, validOptions); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("wrong public header = %v", err)
	}
	counterState, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	counterSnapshot, err := snapshot.New(counterState, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeSnapshot(counterSnapshot, validOptions); err != nil {
		t.Fatalf("valid counter snapshot = %v", err)
	}
	frontier := make(map[string]crdt.Tag, 2)
	frontier["a"] = crdt.Tag{ReplicaID: "a", WallTime: 1}
	frontier["b"] = crdt.Tag{ReplicaID: "b", WallTime: 1}
	// Recreate the snapshot through its public constructor to keep its immutable
	// internals valid while exercising the repository's frontier quota.
	largeFrontier, err := snapshot.New(counterState, frontier)
	if err != nil {
		t.Fatal(err)
	}
	frontierOptions := validOptions
	frontierOptions.MaxFrontierEntries = 1
	if _, err := normalizeSnapshot(largeFrontier, frontierOptions); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("frontier limit = %v", err)
	}
	if _, err := normalizeState(State{Snapshots: []Snapshot{{Scope: "counter", Value: counterSnapshot}, {Scope: "counter", Value: counterSnapshot}}}, validOptions); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate snapshots = %v", err)
	}
	if normalized, err := normalizeState(State{Snapshots: []Snapshot{{Scope: "z", Value: counterSnapshot}, {Scope: "a", Value: counterSnapshot}}}, validOptions); err != nil || normalized.Snapshots[0].Scope != "a" {
		t.Fatalf("state canonicalization = %#v, %v", normalized, err)
	}
}

func TestRepositoryMarshalRollbackOnEncodedLimit(t *testing.T) {
	options := DefaultRepositoryOptions()
	repository, err := NewRepository(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	value := mustText(t, "writer")
	if _, err := value.Insert(0, "seed"); err != nil {
		t.Fatal(err)
	}
	repository.mu.Lock()
	repository.options.MaxEncodedBytes = 64
	repository.mu.Unlock()
	if _, err := repository.Commit("main", stateForText(t, value)); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("encoded limit = %v", err)
	}
	if repository.Len() != 0 {
		t.Fatalf("failed commit retained version: %d", repository.Len())
	}
	repository.mu.Lock()
	repository.versionBytes = repository.options.MaxEncodedBytes
	if err := repository.checkEncodedLimitLocked(); !errors.Is(err, ErrResourceLimit) {
		repository.mu.Unlock()
		t.Fatalf("checked encoded limit = %v", err)
	}
	repository.versionBytes = -1
	if _, ok := repository.encodedSizeLocked(); ok {
		repository.mu.Unlock()
		t.Fatal("negative version bytes accepted")
	}
	repository.mu.Unlock()
	if _, ok := encodedRecordSize(-1, 1, 10); ok {
		t.Fatal("negative parent count accepted")
	}
	if _, ok := encodedRecordSize(1, -1, 10); ok {
		t.Fatal("negative state size accepted")
	}
	if _, ok := encodedRecordSize(1, 128, 16); ok {
		t.Fatal("oversized record accepted")
	}
	if size, ok := encodedRecordSize(1, 8, 128); !ok || size <= 0 {
		t.Fatalf("valid record size = %d, %t", size, ok)
	}
	clean, err := NewRepository(DefaultRepositoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	clean.mu.Lock()
	if err := clean.checkEncodedLimitLocked(); err != nil {
		clean.mu.Unlock()
		t.Fatalf("empty encoded size = %v", err)
	}
	clean.mu.Unlock()
}

func TestRepositoryBranchAndMarshalErrorPaths(t *testing.T) {
	options := DefaultRepositoryOptions()
	options.MaxBranches = 1
	repository, err := NewRepository(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("second", ID{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("branch limit = %v", err)
	}
	if _, ok := repository.Head("missing"); ok {
		t.Fatal("missing head found")
	}
	if len((&Repository{}).Branches()) != 0 || (*Repository)(nil).Branches() != nil {
		t.Fatal("nil branch enumeration mismatch")
	}
	var nilRepository *Repository
	if err := nilRepository.CreateBranch("main", ID{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil create branch = %v", err)
	}
	if err := nilRepository.Fork("copy", "main"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil fork = %v", err)
	}
	if _, ok := nilRepository.Head("main"); ok {
		t.Fatal("nil repository head found")
	}
	if _, ok := nilRepository.Version(ID{1}); ok {
		t.Fatal("nil repository version found")
	}

	broken, err := NewRepository(DefaultRepositoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	broken.versions[ID{1}] = versionRecord{id: ID{2}}
	if _, err := broken.MarshalBinary(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid record marshal = %v", err)
	}
	if _, err := broken.commitLocked("missing", []ID{{9}}, State{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing commit parent = %v", err)
	}
	tooManyParents := make([]ID, broken.options.MaxParents+1)
	for index := range tooManyParents {
		tooManyParents[index][0] = byte(index + 1)
	}
	if _, err := broken.commitLocked("main", tooManyParents, State{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("parent limit = %v", err)
	}
	if _, err := broken.commitLocked("main", nil, State{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid commit state = %v", err)
	}
	if _, err := appendState(nil, State{Snapshots: []Snapshot{{Scope: "bad scope"}}}, DefaultRepositoryOptions(), 1024); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid append state = %v", err)
	}
	validBytes := mustRepositoryBytes(t)
	valid, err := NewRepositoryFromBinary(DefaultRepositoryOptions(), validBytes)
	if err != nil {
		t.Fatal(err)
	}
	valid.mu.Lock()
	for id, record := range valid.versions {
		record.encodedSize = 0
		valid.versions[id] = record
		break
	}
	valid.mu.Unlock()
	if _, err := valid.MarshalBinary(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing record size = %v", err)
	}
	valid, err = NewRepositoryFromBinary(DefaultRepositoryOptions(), validBytes)
	if err != nil {
		t.Fatal(err)
	}
	valid.mu.Lock()
	valid.branches["ghost"] = branchHead{id: ID{9}, has: true}
	valid.mu.Unlock()
	if _, err := valid.MarshalBinary(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unknown branch head = %v", err)
	}
}

func mustRepositoryBytes(t testing.TB) []byte {
	t.Helper()
	repository, err := NewRepository(DefaultRepositoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	value := mustText(t, "writer")
	if _, err := value.Insert(0, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("main", stateForText(t, value)); err != nil {
		t.Fatal(err)
	}
	encoded, err := repository.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func resign(data []byte) {
	payloadEnd := len(data) - sha256.Size
	digest := sha256.Sum256(data[:payloadEnd])
	copy(data[payloadEnd:], digest[:])
}
