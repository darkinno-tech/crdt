package durable

import (
	"crypto/sha256"
	"sync"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/merkle"
)

// MerkleIndex is a small in-process helper for an application's durable event
// inventory. It mirrors only HLC identities and canonical event digests; it
// never stores CRDT payloads. Applications must persist the equivalent index
// with their concrete CRDT checkpoint before reporting Root to a v3 client.
//
// Put is idempotent for the same immutable event and rejects a same-HLC,
// different-digest conflict. Reconcile refuses local-only history instead of
// turning a Merkle mismatch into accidental data loss.
type MerkleIndex struct {
	mu     sync.RWMutex
	leaves map[string]MerkleLeaf
	tree   *merkle.Tree
}

// NewMerkleIndex constructs an empty HLC/Merkle inventory.
func NewMerkleIndex() *MerkleIndex {
	return &MerkleIndex{leaves: make(map[string]MerkleLeaf), tree: merkle.NewTree()}
}

// Put records one durably accepted v3 event. Call it only after the same
// application transaction has persisted the concrete CRDT state and event
// identity; the helper itself is not a persistence layer.
func (index *MerkleIndex) Put(event Event) error {
	if index == nil || index.tree == nil {
		return ErrInvalidConfig
	}
	leaf, err := merkleLeafForEvent(event)
	if err != nil {
		return errInvalidWire
	}
	key := merkleLeafKey(leaf.HLC)
	index.mu.Lock()
	defer index.mu.Unlock()
	if existing, exists := index.leaves[key]; exists {
		if existing.Digest != leaf.Digest {
			return ErrMerkleDiverged
		}
		return nil
	}
	index.leaves[key] = leaf
	index.tree.InsertDigest(key, leaf.Digest)
	return nil
}

// Root returns the current canonical inventory root. A nil index returns the
// canonical empty root so callers can use it for an empty durable checkpoint.
func (index *MerkleIndex) Root() [sha256.Size]byte {
	if index == nil || index.tree == nil {
		return merkle.NewTree().Root()
	}
	return index.tree.Root()
}

// Reconcile compares a complete remote inventory with this local inventory
// and returns the sorted relay HLC identities missing locally. A local-only or
// different-digest leaf is a fail-closed divergence, not a deletion request.
func (index *MerkleIndex) Reconcile(remote []MerkleLeaf) ([]crdt.Tag, error) {
	if index == nil || index.tree == nil {
		return nil, ErrInvalidConfig
	}
	if len(remote) > 0 && validateMerkleLeaves(remote, uint64(len(remote)), ^uint64(0), frame.DefaultLimits().MaxStringBytes) != nil {
		return nil, errInvalidWire
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	remoteKeys := make(map[string]struct{}, len(remote))
	missing := make([]crdt.Tag, 0)
	for _, leaf := range remote {
		key := merkleLeafKey(leaf.HLC)
		remoteKeys[key] = struct{}{}
		if local, exists := index.leaves[key]; !exists {
			missing = append(missing, leaf.HLC)
		} else if local.Digest != leaf.Digest {
			return nil, ErrMerkleDiverged
		}
	}
	for key := range index.leaves {
		if _, exists := remoteKeys[key]; !exists {
			return nil, ErrMerkleDiverged
		}
	}
	return missing, nil
}
