package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/snapshot"
)

const repositoryFormatVersion byte = 1

var repositoryMagic = [...]byte{'C', 'R', 'D', 'V'}

// ID is the content address of one immutable version. It includes canonical
// parent IDs and canonical local snapshots, so changing either creates a new
// ID. It is not a replica identity, cryptographic signature, or authorization
// token.
type ID [sha256.Size]byte

// String returns the lowercase hexadecimal content address.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

func (id ID) valid() bool { return id != ID{} }

// Snapshot names one complete CRDT snapshot within a logical version. Scope
// names let a host version several independently typed CRDTs together without
// creating a new combined replication frame.
type Snapshot struct {
	Scope string
	Value snapshot.Snapshot
}

// State is the complete local materialization recorded at one version point.
// Its snapshots are copied and kept in canonical scope order by Repository.
type State struct {
	Snapshots []Snapshot
}

// Version is an immutable node in the local version DAG. Parents are in
// canonical content-address order. A merge version has two or more parents.
type Version struct {
	ID      ID
	Parents []ID
	State   State
}

// RepositoryOptions bounds local version browsing and persistence metadata.
// MaxEncodedBytes bounds the complete MarshalBinary result, while
// MaxSnapshotBytes bounds one embedded CRDT state frame before it is decoded.
type RepositoryOptions struct {
	MaxVersions        int
	MaxBranches        int
	MaxParents         int
	MaxSnapshots       int
	MaxScopeBytes      int
	MaxSnapshotBytes   int
	MaxFrontierEntries int
	MaxReplicaIDBytes  int
	MaxEncodedBytes    int
}

// DefaultRepositoryOptions returns a conservative bounded local history
// policy. A production host should select limits from its document count,
// checkpoint policy, and storage budget.
func DefaultRepositoryOptions() RepositoryOptions {
	return RepositoryOptions{
		MaxVersions:        10_000,
		MaxBranches:        128,
		MaxParents:         8,
		MaxSnapshots:       128,
		MaxScopeBytes:      256,
		MaxSnapshotBytes:   16 << 20,
		MaxFrontierEntries: 1 << 20,
		MaxReplicaIDBytes:  256,
		MaxEncodedBytes:    64 << 20,
	}
}

func (o RepositoryOptions) valid() bool {
	return o.MaxVersions > 0 && o.MaxBranches > 0 && o.MaxParents > 0 &&
		o.MaxSnapshots > 0 && o.MaxScopeBytes > 0 && o.MaxSnapshotBytes > 0 &&
		o.MaxFrontierEntries > 0 && o.MaxReplicaIDBytes > 0 &&
		o.MaxEncodedBytes > sha256.Size+len(repositoryMagic)+1
}

type branchHead struct {
	id  ID
	has bool
}

type versionRecord struct {
	id          ID
	parents     []ID
	state       State
	encodedSize int
}

// Repository is a concurrent-safe, local content-addressed version DAG. It
// does not merge CRDTs itself: a caller first materializes a merged State by
// using each concrete CRDT's merge and snapshot APIs, then records that State
// with Merge. This prevents the metadata layer from guessing type-specific
// conflict semantics or bypassing HLC recovery requirements.
type Repository struct {
	mu       sync.RWMutex
	options  RepositoryOptions
	versions map[ID]versionRecord
	branches map[string]branchHead
	// versionBytes is the exact sum of canonical version-record bytes. It lets
	// Commit enforce the complete-record budget without serializing every old
	// snapshot again; MarshalBinary remains the explicit persistence boundary.
	versionBytes int
}

// NewRepository creates an empty local version repository.
func NewRepository(options RepositoryOptions) (*Repository, error) {
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	return &Repository{options: options, versions: make(map[ID]versionRecord), branches: make(map[string]branchHead)}, nil
}

// NewRepositoryFromBinary restores a complete local version DAG. Callers must
// still validate and restore a selected concrete snapshot through that CRDT's
// own recovery constructor before reusing a replica ID.
func NewRepositoryFromBinary(options RepositoryOptions, data []byte) (*Repository, error) {
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	versions, branches, versionBytes, err := unmarshalRepository(data, options)
	if err != nil {
		return nil, err
	}
	return &Repository{options: options, versions: versions, branches: branches, versionBytes: versionBytes}, nil
}

// CreateBranch creates an empty branch when from is zero, or a branch rooted
// at an existing version. Branch names are local metadata and are never sent
// as CRDT fields.
func (r *Repository) CreateBranch(name string, from ID) error {
	if r == nil {
		return ErrInvalidState
	}
	if validateScope(name, r.options.MaxScopeBytes) != nil {
		return ErrInvalidCommand
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.branches[name]; exists {
		return ErrInvalidState
	}
	if len(r.branches) >= r.options.MaxBranches {
		return ErrResourceLimit
	}
	if from.valid() {
		if _, exists := r.versions[from]; !exists {
			return ErrInvalidState
		}
	}
	r.branches[name] = branchHead{id: from, has: from.valid()}
	if err := r.checkEncodedLimitLocked(); err != nil {
		delete(r.branches, name)
		return err
	}
	return nil
}

// Fork creates target at source's current head. Source must have at least one
// version; use CreateBranch with a zero ID for a new genesis branch.
func (r *Repository) Fork(target, source string) error {
	if r == nil {
		return ErrInvalidState
	}
	r.mu.RLock()
	head, exists := r.branches[source]
	r.mu.RUnlock()
	if !exists || !head.has {
		return ErrInvalidState
	}
	return r.CreateBranch(target, head.id)
}

// Commit records one new version on branch. The branch must already exist;
// its prior head becomes the sole parent, or the version is a genesis commit
// for an empty branch.
func (r *Repository) Commit(branch string, state State) (ID, error) {
	if r == nil {
		return ID{}, ErrInvalidState
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	head, exists := r.branches[branch]
	if !exists {
		return ID{}, ErrInvalidState
	}
	parents := make([]ID, 0, 1)
	if head.has {
		parents = append(parents, head.id)
	}
	return r.commitLocked(branch, parents, state)
}

// Merge records a host-materialized merge snapshot on target. target and
// source must both have heads. The resulting version has the two distinct
// heads as parents in canonical order. This method intentionally does not call
// Merge on arbitrary snapshot bytes because concrete CRDT and schema semantics
// must remain the authority for conflict resolution.
func (r *Repository) Merge(target, source string, state State) (ID, error) {
	if r == nil {
		return ID{}, ErrInvalidState
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	left, leftOK := r.branches[target]
	right, rightOK := r.branches[source]
	if !leftOK || !rightOK || !left.has || !right.has {
		return ID{}, ErrInvalidState
	}
	parents := []ID{left.id}
	if right.id != left.id {
		parents = append(parents, right.id)
	}
	return r.commitLocked(target, parents, state)
}

func (r *Repository) commitLocked(branch string, parents []ID, state State) (ID, error) {
	if len(parents) > r.options.MaxParents {
		return ID{}, ErrResourceLimit
	}
	parents = canonicalParents(parents)
	for _, parent := range parents {
		if _, exists := r.versions[parent]; !exists {
			return ID{}, ErrInvalidState
		}
	}
	normalized, err := normalizeState(state, r.options)
	if err != nil {
		return ID{}, err
	}
	id, encodedState, err := versionData(parents, normalized, r.options)
	if err != nil {
		return ID{}, err
	}
	previous := r.branches[branch]
	newVersion := false
	if _, exists := r.versions[id]; !exists {
		if len(r.versions) >= r.options.MaxVersions {
			return ID{}, ErrResourceLimit
		}
		recordSize, ok := encodedRecordSize(len(parents), len(encodedState), r.options.MaxEncodedBytes)
		if !ok {
			return ID{}, ErrResourceLimit
		}
		r.versions[id] = versionRecord{id: id, parents: append([]ID(nil), parents...), state: cloneState(normalized), encodedSize: recordSize}
		r.versionBytes += recordSize
		newVersion = true
	}
	r.branches[branch] = branchHead{id: id, has: true}
	if err := r.checkEncodedLimitLocked(); err != nil {
		r.branches[branch] = previous
		if newVersion {
			r.versionBytes -= r.versions[id].encodedSize
			delete(r.versions, id)
		}
		return ID{}, err
	}
	return id, nil
}

// Head returns the current content address for branch.
func (r *Repository) Head(branch string) (ID, bool) {
	if r == nil {
		return ID{}, false
	}
	r.mu.RLock()
	head, exists := r.branches[branch]
	r.mu.RUnlock()
	return head.id, exists && head.has
}

// Version returns a copy of one immutable version.
func (r *Repository) Version(id ID) (Version, bool) {
	if r == nil {
		return Version{}, false
	}
	r.mu.RLock()
	record, exists := r.versions[id]
	r.mu.RUnlock()
	if !exists {
		return Version{}, false
	}
	return Version{ID: record.id, Parents: append([]ID(nil), record.parents...), State: cloneState(record.state)}, true
}

// Branches returns sorted local branch names.
func (r *Repository) Branches() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	result := make([]string, 0, len(r.branches))
	for branch := range r.branches {
		result = append(result, branch)
	}
	r.mu.RUnlock()
	sort.Strings(result)
	return result
}

// History returns branch's reachable versions in deterministic newest-first
// depth-first order. Shared ancestors appear only once.
func (r *Repository) History(branch string) []Version {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	head, exists := r.branches[branch]
	if !exists || !head.has {
		r.mu.RUnlock()
		return nil
	}
	result := make([]Version, 0)
	seen := make(map[ID]struct{})
	var visit func(ID)
	visit = func(id ID) {
		if _, done := seen[id]; done {
			return
		}
		record, ok := r.versions[id]
		if !ok {
			return
		}
		seen[id] = struct{}{}
		result = append(result, Version{ID: record.id, Parents: append([]ID(nil), record.parents...), State: cloneState(record.state)})
		for _, parent := range record.parents {
			visit(parent)
		}
	}
	visit(head.id)
	r.mu.RUnlock()
	return result
}

// Len returns the number of retained immutable versions.
func (r *Repository) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.versions)
}

// MarshalBinary returns a canonical, checksummed local version-DAG record. It
// contains complete snapshots and should receive the same encryption-at-rest,
// authorization, retention, and backup treatment as application data.
func (r *Repository) MarshalBinary() ([]byte, error) {
	if r == nil {
		return nil, ErrInvalidState
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.marshalLocked()
}

func (r *Repository) marshalLocked() ([]byte, error) {
	if len(r.versions) > r.options.MaxVersions || len(r.branches) > r.options.MaxBranches {
		return nil, ErrResourceLimit
	}
	encoded := make([]byte, 0, 1024)
	encoded = append(encoded, repositoryMagic[:]...)
	encoded = append(encoded, repositoryFormatVersion)
	var ok bool
	encoded, ok = appendBoundedUvarint(encoded, uint64(len(r.versions)), r.options.MaxEncodedBytes-sha256.Size)
	if !ok {
		return nil, ErrResourceLimit
	}
	ids := make([]ID, 0, len(r.versions))
	for id := range r.versions {
		ids = append(ids, id)
	}
	sortIDs(ids)
	for _, id := range ids {
		record := r.versions[id]
		if id != record.id || len(record.parents) > r.options.MaxParents || !parentsCanonical(record.parents) {
			return nil, ErrInvalidState
		}
		recordStart := len(encoded)
		if len(encoded) > r.options.MaxEncodedBytes-sha256.Size-len(id) {
			return nil, ErrResourceLimit
		}
		encoded = append(encoded, id[:]...)
		encoded, ok = appendBoundedUvarint(encoded, uint64(len(record.parents)), r.options.MaxEncodedBytes-sha256.Size)
		if !ok {
			return nil, ErrResourceLimit
		}
		for _, parent := range record.parents {
			if len(encoded) > r.options.MaxEncodedBytes-sha256.Size-len(parent) {
				return nil, ErrResourceLimit
			}
			encoded = append(encoded, parent[:]...)
		}
		var err error
		encoded, err = appendState(encoded, record.state, r.options, r.options.MaxEncodedBytes-sha256.Size)
		if err != nil {
			return nil, err
		}
		if record.encodedSize <= 0 || len(encoded)-recordStart != record.encodedSize {
			return nil, ErrInvalidState
		}
	}
	branches := make([]string, 0, len(r.branches))
	for branch := range r.branches {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	encoded, ok = appendBoundedUvarint(encoded, uint64(len(branches)), r.options.MaxEncodedBytes-sha256.Size)
	if !ok {
		return nil, ErrResourceLimit
	}
	for _, branch := range branches {
		if validateScope(branch, r.options.MaxScopeBytes) != nil {
			return nil, ErrInvalidState
		}
		encoded, ok = appendBoundedBytes(encoded, []byte(branch), r.options.MaxEncodedBytes-sha256.Size)
		if !ok {
			return nil, ErrResourceLimit
		}
		head := r.branches[branch]
		if head.has {
			if _, exists := r.versions[head.id]; !exists || len(encoded) > r.options.MaxEncodedBytes-sha256.Size-1-len(head.id) {
				return nil, ErrInvalidState
			}
			encoded = append(encoded, 1)
			encoded = append(encoded, head.id[:]...)
			continue
		}
		if len(encoded) >= r.options.MaxEncodedBytes-sha256.Size {
			return nil, ErrResourceLimit
		}
		encoded = append(encoded, 0)
	}
	if len(encoded) > r.options.MaxEncodedBytes-sha256.Size {
		return nil, ErrResourceLimit
	}
	digest := sha256.Sum256(encoded)
	return append(encoded, digest[:]...), nil
}

func unmarshalRepository(data []byte, options RepositoryOptions) (map[ID]versionRecord, map[string]branchHead, int, error) {
	if len(data) < len(repositoryMagic)+1+sha256.Size || len(data) > options.MaxEncodedBytes {
		return nil, nil, 0, ErrInvalidState
	}
	payloadEnd := len(data) - sha256.Size
	digest := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(digest[:], data[payloadEnd:]) || !bytes.Equal(data[:len(repositoryMagic)], repositoryMagic[:]) || data[len(repositoryMagic)] != repositoryFormatVersion {
		return nil, nil, 0, ErrInvalidState
	}
	position := len(repositoryMagic) + 1
	count, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
	versionCount, bounded := decodeCount(count, options.MaxVersions)
	if !ok || !bounded {
		return nil, nil, 0, ErrInvalidState
	}
	position = next
	versions := make(map[ID]versionRecord, versionCount)
	versionBytes := 0
	var previous ID
	for index := 0; index < versionCount; index++ {
		recordStart := position
		id, next, ok := readID(data[:payloadEnd], position)
		if !ok || !id.valid() || (index > 0 && bytes.Compare(previous[:], id[:]) >= 0) {
			return nil, nil, 0, ErrInvalidState
		}
		position = next
		parentCount, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
		parentLimit, bounded := decodeCount(parentCount, options.MaxParents)
		if !ok || !bounded {
			return nil, nil, 0, ErrInvalidState
		}
		position = next
		parents := make([]ID, 0, parentLimit)
		var previousParent ID
		for parentIndex := 0; parentIndex < parentLimit; parentIndex++ {
			parent, next, ok := readID(data[:payloadEnd], position)
			if !ok || !parent.valid() || (parentIndex > 0 && bytes.Compare(previousParent[:], parent[:]) >= 0) {
				return nil, nil, 0, ErrInvalidState
			}
			position = next
			parents = append(parents, parent)
			previousParent = parent
		}
		state, next, err := readState(data[:payloadEnd], position, options)
		if err != nil {
			return nil, nil, 0, err
		}
		position = next
		derived, err := versionID(parents, state, options)
		if err != nil || derived != id {
			return nil, nil, 0, ErrInvalidState
		}
		recordSize := position - recordStart
		if recordSize <= 0 || versionBytes > options.MaxEncodedBytes-recordSize {
			return nil, nil, 0, ErrInvalidState
		}
		versions[id] = versionRecord{id: id, parents: parents, state: state, encodedSize: recordSize}
		versionBytes += recordSize
		previous = id
	}
	for _, record := range versions {
		for _, parent := range record.parents {
			if _, exists := versions[parent]; !exists {
				return nil, nil, 0, ErrInvalidState
			}
		}
	}
	branchCount, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
	branchLimit, bounded := decodeCount(branchCount, options.MaxBranches)
	if !ok || !bounded {
		return nil, nil, 0, ErrInvalidState
	}
	position = next
	branches := make(map[string]branchHead, branchLimit)
	previousBranch := ""
	for index := 0; index < branchLimit; index++ {
		name, next, ok := frame.ReadBytes(data[:payloadEnd], position, options.MaxScopeBytes)
		if !ok {
			return nil, nil, 0, ErrInvalidState
		}
		position = next
		branch := string(name)
		if validateScope(branch, options.MaxScopeBytes) != nil || (index > 0 && previousBranch >= branch) {
			return nil, nil, 0, ErrInvalidState
		}
		if position >= payloadEnd {
			return nil, nil, 0, ErrInvalidState
		}
		flag := data[position]
		position++
		head := branchHead{}
		if flag == 1 {
			id, next, ok := readID(data[:payloadEnd], position)
			if !ok {
				return nil, nil, 0, ErrInvalidState
			}
			position = next
			if _, exists := versions[id]; !exists {
				return nil, nil, 0, ErrInvalidState
			}
			head = branchHead{id: id, has: true}
		} else if flag != 0 {
			return nil, nil, 0, ErrInvalidState
		}
		branches[branch] = head
		previousBranch = branch
	}
	if position != payloadEnd {
		return nil, nil, 0, ErrInvalidState
	}
	return versions, branches, versionBytes, nil
}

func normalizeState(state State, options RepositoryOptions) (State, error) {
	if len(state.Snapshots) == 0 || len(state.Snapshots) > options.MaxSnapshots {
		return State{}, ErrResourceLimit
	}
	result := State{Snapshots: make([]Snapshot, len(state.Snapshots))}
	for index, value := range state.Snapshots {
		if validateScope(value.Scope, options.MaxScopeBytes) != nil {
			return State{}, ErrInvalidState
		}
		normalized, err := normalizeSnapshot(value.Value, options)
		if err != nil {
			return State{}, err
		}
		result.Snapshots[index] = Snapshot{Scope: value.Scope, Value: normalized}
	}
	sort.Slice(result.Snapshots, func(left, right int) bool { return result.Snapshots[left].Scope < result.Snapshots[right].Scope })
	for index := 1; index < len(result.Snapshots); index++ {
		if result.Snapshots[index-1].Scope == result.Snapshots[index].Scope {
			return State{}, ErrInvalidState
		}
	}
	return result, nil
}

func normalizeSnapshot(value snapshot.Snapshot, options RepositoryOptions) (snapshot.Snapshot, error) {
	state := value.Bytes()
	if len(state) == 0 || len(state) > options.MaxSnapshotBytes {
		return snapshot.Snapshot{}, ErrResourceLimit
	}
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || value.FormatVersion != decoded.Version() || value.TypeID != decoded.TypeID || value.CodecID != decoded.CodecID {
		return snapshot.Snapshot{}, ErrInvalidState
	}
	kind, known := crdt.FrameTypeForState(decoded.TypeID)
	if !known {
		return snapshot.Snapshot{}, ErrInvalidState
	}
	frontier := value.Frontier()
	if len(frontier) > options.MaxFrontierEntries {
		return snapshot.Snapshot{}, ErrResourceLimit
	}
	for replicaID, tag := range frontier {
		if len(replicaID) == 0 || len(replicaID) > options.MaxReplicaIDBytes || replicaID != tag.ReplicaID || !tag.Valid() {
			return snapshot.Snapshot{}, ErrInvalidState
		}
	}
	clockState, hasClock := value.ClockState()
	if kind.UsesHLC {
		if !hasClock || len(clockState.ReplicaID) == 0 || len(clockState.ReplicaID) > options.MaxReplicaIDBytes {
			return snapshot.Snapshot{}, ErrInvalidState
		}
		result, err := snapshot.NewWithClockState(state, frontier, clockState)
		if err != nil {
			return snapshot.Snapshot{}, ErrInvalidState
		}
		return result, nil
	}
	if hasClock {
		return snapshot.Snapshot{}, ErrInvalidState
	}
	result, err := snapshot.New(state, frontier)
	if err != nil {
		return snapshot.Snapshot{}, ErrInvalidState
	}
	return result, nil
}

func cloneState(state State) State {
	cloned := State{Snapshots: make([]Snapshot, len(state.Snapshots))}
	for index, value := range state.Snapshots {
		// snapshot.Snapshot has no mutators and every public accessor copies its
		// private bytes/maps, so a value copy preserves its immutable boundary.
		cloned.Snapshots[index] = Snapshot{Scope: value.Scope, Value: value.Value}
	}
	return cloned
}

func versionID(parents []ID, state State, options RepositoryOptions) (ID, error) {
	id, _, err := versionData(parents, state, options)
	return id, err
}

func versionData(parents []ID, state State, options RepositoryOptions) (ID, []byte, error) {
	if !parentsCanonical(parents) || len(parents) > options.MaxParents {
		return ID{}, nil, ErrInvalidState
	}
	encoded, err := appendState(nil, state, options, options.MaxEncodedBytes)
	if err != nil {
		return ID{}, nil, err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("crdt-history-version-v1"))
	_, _ = hash.Write(frame.AppendUvarint(nil, uint64(len(parents))))
	for _, parent := range parents {
		_, _ = hash.Write(parent[:])
	}
	_, _ = hash.Write(encoded)
	var id ID
	copy(id[:], hash.Sum(nil))
	return id, encoded, nil
}

func (r *Repository) checkEncodedLimitLocked() error {
	size, ok := r.encodedSizeLocked()
	if !ok || size > r.options.MaxEncodedBytes {
		return ErrResourceLimit
	}
	return nil
}

func (r *Repository) encodedSizeLocked() (int, bool) {
	maximum := r.options.MaxEncodedBytes
	size := len(repositoryMagic) + 1 + sha256.Size
	if size > maximum || r.versionBytes < 0 || r.versionBytes > maximum-size {
		return 0, false
	}
	size += r.versionBytes
	countBytes := frame.UvarintSize(uint64(len(r.versions)))
	if countBytes > maximum-size {
		return 0, false
	}
	size += countBytes
	branchCountBytes := frame.UvarintSize(uint64(len(r.branches)))
	if branchCountBytes > maximum-size {
		return 0, false
	}
	size += branchCountBytes
	for name, head := range r.branches {
		entrySize := frame.UvarintSize(uint64(len(name))) + len(name) + 1
		if head.has {
			entrySize += len(head.id)
		}
		if entrySize < 0 || entrySize > maximum-size {
			return 0, false
		}
		size += entrySize
	}
	return size, true
}

func encodedRecordSize(parentCount, stateSize, maximum int) (int, bool) {
	if parentCount < 0 || stateSize < 0 {
		return 0, false
	}
	size := sha256.Size + frame.UvarintSize(uint64(parentCount)) + parentCount*sha256.Size + stateSize
	return size, size >= 0 && size <= maximum
}

func appendState(encoded []byte, state State, options RepositoryOptions, maximum int) ([]byte, error) {
	normalized, err := normalizeState(state, options)
	if err != nil {
		return nil, err
	}
	var ok bool
	encoded, ok = appendBoundedUvarint(encoded, uint64(len(normalized.Snapshots)), maximum)
	if !ok {
		return nil, ErrResourceLimit
	}
	for _, value := range normalized.Snapshots {
		encoded, ok = appendBoundedBytes(encoded, []byte(value.Scope), maximum)
		if !ok {
			return nil, ErrResourceLimit
		}
		stateBytes := value.Value.Bytes()
		encoded, ok = appendBoundedBytes(encoded, stateBytes, maximum)
		if !ok {
			return nil, ErrResourceLimit
		}
		frontier := value.Value.Frontier()
		if len(frontier) > options.MaxFrontierEntries {
			return nil, ErrResourceLimit
		}
		encoded, ok = appendBoundedUvarint(encoded, uint64(len(frontier)), maximum)
		if !ok {
			return nil, ErrResourceLimit
		}
		ids := make([]string, 0, len(frontier))
		for replicaID := range frontier {
			ids = append(ids, replicaID)
		}
		sort.Strings(ids)
		for _, replicaID := range ids {
			tag := frontier[replicaID]
			if len(replicaID) == 0 || len(replicaID) > options.MaxReplicaIDBytes || !tag.Valid() ||
				len(encoded) > maximum-frame.TagSize(tag) {
				return nil, ErrResourceLimit
			}
			encoded = frame.AppendTag(encoded, tag)
		}
		clockState, hasClock := value.Value.ClockState()
		if len(encoded) >= maximum {
			return nil, ErrResourceLimit
		}
		if !hasClock {
			encoded = append(encoded, 0)
			continue
		}
		tag := crdt.Tag{ReplicaID: clockState.ReplicaID, WallTime: clockState.WallTime, Logical: clockState.Logical}
		if len(clockState.ReplicaID) == 0 || len(clockState.ReplicaID) > options.MaxReplicaIDBytes || !tag.Valid() ||
			len(encoded) > maximum-1-frame.TagSize(tag) {
			return nil, ErrResourceLimit
		}
		encoded = append(encoded, 1)
		encoded = frame.AppendTag(encoded, tag)
	}
	return encoded, nil
}

func readState(data []byte, position int, options RepositoryOptions) (State, int, error) {
	count, next, ok := frame.ReadUvarint(data, position)
	snapshotCount, bounded := decodeCount(count, options.MaxSnapshots)
	if !ok || snapshotCount == 0 || !bounded {
		return State{}, 0, ErrInvalidState
	}
	position = next
	state := State{Snapshots: make([]Snapshot, 0, snapshotCount)}
	previousScope := ""
	for index := 0; index < snapshotCount; index++ {
		scope, next, ok := frame.ReadBytes(data, position, options.MaxScopeBytes)
		if !ok {
			return State{}, 0, ErrInvalidState
		}
		position = next
		name := string(scope)
		if validateScope(name, options.MaxScopeBytes) != nil || (index > 0 && previousScope >= name) {
			return State{}, 0, ErrInvalidState
		}
		stateBytes, next, ok := frame.ReadBytes(data, position, options.MaxSnapshotBytes)
		if !ok {
			return State{}, 0, ErrInvalidState
		}
		position = next
		frontierCount, next, ok := frame.ReadUvarint(data, position)
		frontierLimit, bounded := decodeCount(frontierCount, options.MaxFrontierEntries)
		if !ok || !bounded {
			return State{}, 0, ErrInvalidState
		}
		position = next
		frontier := make(map[string]crdt.Tag, frontierLimit)
		previousReplicaID := ""
		for frontierIndex := 0; frontierIndex < frontierLimit; frontierIndex++ {
			tag, next, ok := frame.ReadTag(data, position, options.MaxReplicaIDBytes)
			if !ok || tag.ReplicaID <= previousReplicaID {
				return State{}, 0, ErrInvalidState
			}
			position = next
			frontier[tag.ReplicaID] = tag
			previousReplicaID = tag.ReplicaID
		}
		if position >= len(data) {
			return State{}, 0, ErrInvalidState
		}
		flag := data[position]
		position++
		var saved snapshot.Snapshot
		switch flag {
		case 0:
			candidate, err := snapshot.New(stateBytes, frontier)
			if err != nil {
				return State{}, 0, ErrInvalidState
			}
			saved = candidate
		case 1:
			tag, next, ok := frame.ReadTag(data, position, options.MaxReplicaIDBytes)
			if !ok {
				return State{}, 0, ErrInvalidState
			}
			position = next
			candidate, err := snapshot.NewWithClockState(stateBytes, frontier, clock.State{ReplicaID: tag.ReplicaID, WallTime: tag.WallTime, Logical: tag.Logical})
			if err != nil {
				return State{}, 0, ErrInvalidState
			}
			saved = candidate
		default:
			return State{}, 0, ErrInvalidState
		}
		normalized, err := normalizeSnapshot(saved, options)
		if err != nil {
			return State{}, 0, err
		}
		state.Snapshots = append(state.Snapshots, Snapshot{Scope: name, Value: normalized})
		previousScope = name
	}
	return state, position, nil
}

func canonicalParents(parents []ID) []ID {
	parents = append([]ID(nil), parents...)
	sortIDs(parents)
	result := parents[:0]
	for _, parent := range parents {
		if !parent.valid() || (len(result) > 0 && result[len(result)-1] == parent) {
			continue
		}
		result = append(result, parent)
	}
	return result
}

func parentsCanonical(parents []ID) bool {
	var previous ID
	for index, parent := range parents {
		if !parent.valid() || (index > 0 && bytes.Compare(previous[:], parent[:]) >= 0) {
			return false
		}
		previous = parent
	}
	return true
}

func sortIDs(ids []ID) {
	sort.Slice(ids, func(left, right int) bool { return bytes.Compare(ids[left][:], ids[right][:]) < 0 })
}

func readID(data []byte, position int) (ID, int, bool) {
	var id ID
	if position < 0 || len(data)-position < len(id) {
		return ID{}, 0, false
	}
	copy(id[:], data[position:position+len(id)])
	return id, position + len(id), true
}

func appendBoundedUvarint(encoded []byte, value uint64, maximum int) ([]byte, bool) {
	additional := frame.UvarintSize(value)
	if len(encoded) > maximum || additional > maximum-len(encoded) {
		return nil, false
	}
	return frame.AppendUvarint(encoded, value), true
}
