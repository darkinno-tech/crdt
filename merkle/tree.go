// Package merkle provides deterministic state digests for anti-entropy.
package merkle

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"sync"

	frame "github.com/DarkInno/crdt/encoding"
)

var ErrInvalidState = errors.New("merkle: invalid canonical state frame")

// Tree stores key-to-value digests. Insert values should be canonical CRDT
// state bytes. Tree is safe for concurrent use, but is not a transport
// protocol or an authority on state validity.
type Tree struct {
	mu      sync.RWMutex
	entries map[string][sha256.Size]byte
}

func NewTree() *Tree { return &Tree{entries: make(map[string][sha256.Size]byte)} }

func (t *Tree) Insert(key string, value []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[key] = sha256.Sum256(value)
}

// Delete removes key from t. It is safe to call for a missing key.
func (t *Tree) Delete(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
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
// digests. Leaf and inner-node domain separators prevent structural
// ambiguity; an odd node is paired with a zero digest at that level.
func (t *Tree) Root() [sha256.Size]byte {
	if t == nil {
		return emptyRoot()
	}
	t.mu.RLock()
	entries := cloneEntries(t.entries)
	t.mu.RUnlock()
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	leaves := make([][sha256.Size]byte, 0, len(keys))
	for _, key := range keys {
		var length [binary.MaxVarintLen64]byte
		size := binary.PutUvarint(length[:], uint64(len(key)))
		encoded := make([]byte, 0, 1+size+len(key)+sha256.Size)
		encoded = append(encoded, 0)
		encoded = append(encoded, length[:size]...)
		encoded = append(encoded, key...)
		value := entries[key]
		encoded = append(encoded, value[:]...)
		leaves = append(leaves, sha256.Sum256(encoded))
	}
	if len(leaves) == 0 {
		return emptyRoot()
	}
	for len(leaves) > 1 {
		next := make([][sha256.Size]byte, 0, (len(leaves)+1)/2)
		for i := 0; i < len(leaves); i += 2 {
			right := [sha256.Size]byte{}
			if i+1 < len(leaves) {
				right = leaves[i+1]
			}
			encoded := make([]byte, 0, 1+2*sha256.Size)
			encoded = append(encoded, 1)
			encoded = append(encoded, leaves[i][:]...)
			encoded = append(encoded, right[:]...)
			next = append(next, sha256.Sum256(encoded))
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

func emptyRoot() [sha256.Size]byte { return sha256.Sum256([]byte{2}) }
