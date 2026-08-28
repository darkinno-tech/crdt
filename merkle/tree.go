// Package merkle provides deterministic state digests for anti-entropy.
package merkle

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"sync"

	frame "github.com/darkinno-tech/crdt/encoding"
)

var ErrInvalidState = errors.New("merkle: invalid canonical state frame")

// Tree stores key-to-value digests. Insert values should be canonical CRDT
// state bytes. Tree is safe for concurrent use, but is not a transport
// protocol or an authority on state validity.
type Tree struct {
	mu            sync.RWMutex
	entries       map[string][sha256.Size]byte
	generation    uint64
	cachedRoot    [sha256.Size]byte
	cachedRootFor uint64
	hasCachedRoot bool
}

type treeEntry struct {
	key    string
	digest [sha256.Size]byte
}

// Entry is one immutable key-to-digest binding in a Tree. It is useful for
// bounded anti-entropy inventories: callers can compare a remote inventory
// without transferring the canonical values that produced its digests.
//
// Entry does not authenticate its key or digest. The transport still needs to
// authenticate the peer and validate any value fetched after reconciliation.
type Entry struct {
	Key    string
	Digest [sha256.Size]byte
}

func NewTree() *Tree { return &Tree{entries: make(map[string][sha256.Size]byte)} }

func (t *Tree) Insert(key string, value []byte) {
	t.InsertDigest(key, sha256.Sum256(value))
}

// InsertDigest records a precomputed canonical-value digest. It avoids
// rehashing values when a caller receives a Merkle inventory whose leaf
// digests have already been calculated by an authenticated peer.
func (t *Tree) InsertDigest(key string, digest [sha256.Size]byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if current, exists := t.entries[key]; exists && current == digest {
		return
	}
	t.entries[key] = digest
	t.invalidateRootLocked()
}

// Entries returns a sorted, detached inventory of the tree's leaf bindings.
// The caller owns the returned slice. It deliberately exposes digests only;
// Tree never retains the input values supplied to Insert.
func (t *Tree) Entries() []Entry {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	entries := make([]Entry, 0, len(t.entries))
	for key, digest := range t.entries {
		entries = append(entries, Entry{Key: key, Digest: digest})
	}
	t.mu.RUnlock()
	sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
	return entries
}

// Delete removes key from t. It is safe to call for a missing key.
func (t *Tree) Delete(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.entries[key]; !exists {
		return
	}
	delete(t.entries, key)
	t.invalidateRootLocked()
}

// InsertState validates state as a complete CRDT frame envelope before
// hashing it. Prefer this method when roots are exchanged for anti-entropy;
// callers must have decoded any type-specific payload before accepting it.
func (t *Tree) InsertState(key string, state []byte) error {
	if _, err := frame.UnmarshalFrame(state, frame.DefaultLimits()); err != nil {
		return ErrInvalidState
	}
	t.Insert(key, state)
	return nil
}

// Root returns a deterministic binary Merkle root over sorted key/value
// digests. It caches the result for the current immutable tree generation, so
// repeated anti-entropy checks do not re-copy, sort, and hash unchanged state.
// Leaf and inner-node domain separators prevent structural ambiguity; an odd
// node is paired with a zero digest at that level.
func (t *Tree) Root() [sha256.Size]byte {
	if t == nil {
		return emptyRoot()
	}
	t.mu.RLock()
	if t.hasCachedRoot && t.cachedRootFor == t.generation {
		root := t.cachedRoot
		t.mu.RUnlock()
		return root
	}
	entries := snapshotEntries(t.entries)
	generation := t.generation
	t.mu.RUnlock()
	root := rootForEntries(entries)

	// Do not install a root calculated from an older snapshot after a writer
	// has advanced the generation. Returning it is still correct for the
	// captured snapshot, and the next call will calculate the current root.
	t.mu.Lock()
	if t.generation == generation {
		t.cachedRoot = root
		t.cachedRootFor = generation
		t.hasCachedRoot = true
	}
	t.mu.Unlock()
	return root
}

func (t *Tree) invalidateRootLocked() {
	t.generation++
	t.hasCachedRoot = false
}

func rootForEntries(entries []treeEntry) [sha256.Size]byte {
	if len(entries) == 0 {
		return emptyRoot()
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].key < entries[right].key })
	maxKeyBytes := 0
	for _, entry := range entries {
		maxKeyBytes = max(maxKeyBytes, len(entry.key))
	}
	leaves := make([][sha256.Size]byte, len(entries))
	encodedLeaf := make([]byte, 0, 1+binary.MaxVarintLen64+maxKeyBytes+sha256.Size)
	for index, entry := range entries {
		var length [binary.MaxVarintLen64]byte
		size := binary.PutUvarint(length[:], uint64(len(entry.key)))
		encodedLeaf = encodedLeaf[:0]
		encodedLeaf = append(encodedLeaf, 0)
		encodedLeaf = append(encodedLeaf, length[:size]...)
		encodedLeaf = append(encodedLeaf, entry.key...)
		encodedLeaf = append(encodedLeaf, entry.digest[:]...)
		leaves[index] = sha256.Sum256(encodedLeaf)
	}
	var encodedInner [1 + 2*sha256.Size]byte
	encodedInner[0] = 1
	for len(leaves) > 1 {
		next := make([][sha256.Size]byte, 0, (len(leaves)+1)/2)
		for i := 0; i < len(leaves); i += 2 {
			right := [sha256.Size]byte{}
			if i+1 < len(leaves) {
				right = leaves[i+1]
			}
			copy(encodedInner[1:1+sha256.Size], leaves[i][:])
			copy(encodedInner[1+sha256.Size:], right[:])
			next = append(next, sha256.Sum256(encodedInner[:]))
		}
		leaves = next
	}
	return leaves[0]
}

// Diff returns keys whose digest differs between two trees.
func Diff(left, right *Tree) (leftOnly, rightOnly, different []string) {
	if left == nil {
		left = NewTree()
	}
	if right == nil {
		right = NewTree()
	}
	if left.Root() == right.Root() {
		return nil, nil, nil
	}
	left.mu.RLock()
	leftEntries := cloneEntries(left.entries)
	left.mu.RUnlock()
	right.mu.RLock()
	rightEntries := cloneEntries(right.entries)
	right.mu.RUnlock()
	for key, value := range leftEntries {
		if remote, ok := rightEntries[key]; !ok {
			leftOnly = append(leftOnly, key)
		} else if value != remote {
			different = append(different, key)
		}
	}
	for key := range rightEntries {
		if _, ok := leftEntries[key]; !ok {
			rightOnly = append(rightOnly, key)
		}
	}
	sort.Strings(leftOnly)
	sort.Strings(rightOnly)
	sort.Strings(different)
	return
}

func cloneEntries(source map[string][sha256.Size]byte) map[string][sha256.Size]byte {
	clone := make(map[string][sha256.Size]byte, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// snapshotEntries copies the immutable key/digest pairs while callers hold a
// read lock only for the map traversal. Root sorts and hashes this detached
// slice so writers retain the same short critical section they had with the
// former map clone.
func snapshotEntries(source map[string][sha256.Size]byte) []treeEntry {
	entries := make([]treeEntry, 0, len(source))
	for key, digest := range source {
		entries = append(entries, treeEntry{key: key, digest: digest})
	}
	return entries
}

func emptyRoot() [sha256.Size]byte { return sha256.Sum256([]byte{2}) }
