package membership

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/set"
)

const receiptDomain = "im10furry/crdt/tombstone-receipt/v1"

var (
	ErrInvalidReceipt = errors.New("membership: invalid tombstone receipt")
	ErrReceiptReplay  = errors.New("membership: tombstone receipt replay")
)

// Receipt is an authenticated assertion by one crash-fault-trusted member
// that it has durably installed the listed tombstones in a non-zero checkpoint
// ID. A signature establishes origin and replay scope; it cannot prove
// honesty or durable storage against a Byzantine signer.
type Receipt struct {
	GroupID      string
	Epoch        uint64
	ViewHash     [sha256.Size]byte
	MemberID     string
	Incarnation  uint64
	Sequence     uint64
	CheckpointID [sha256.Size]byte
	Tags         []crdt.Tag
	Signature    []byte
}

// SignReceipt returns a canonical signed copy. Tags must be sorted and unique
// so a receipt has exactly one byte representation and cannot inflate GC
// accounting through duplicate acknowledgements.
func SignReceipt(receipt Receipt, privateKey ed25519.PrivateKey) (Receipt, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !receipt.validUnsigned() {
		return Receipt{}, ErrInvalidReceipt
	}
	receipt.Signature = nil
	payload, err := receipt.signingBytes()
	if err != nil {
		return Receipt{}, err
	}
	receipt.Signature = ed25519.Sign(privateKey, payload)
	receipt.Tags = append([]crdt.Tag(nil), receipt.Tags...)
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	return receipt, nil
}

func verifyReceipt(receipt Receipt, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize || !receipt.validUnsigned() || len(receipt.Signature) != ed25519.SignatureSize {
		return ErrInvalidReceipt
	}
	payload, err := receipt.signingBytes()
	if err != nil || !ed25519.Verify(publicKey, payload, receipt.Signature) {
		return ErrInvalidReceipt
	}
	return nil
}

func (r Receipt) validUnsigned() bool {
	if strings.TrimSpace(r.GroupID) == "" || r.Epoch == 0 || strings.TrimSpace(r.MemberID) == "" || r.Incarnation == 0 || r.Sequence == 0 || isZeroHash(r.ViewHash) || isZeroHash(r.CheckpointID) || len(r.Tags) > maxReceiptTags {
		return false
	}
	for index, tag := range r.Tags {
		if !tag.Valid() || (index > 0 && r.Tags[index-1].Compare(tag) >= 0) {
			return false
		}
	}
	return true
}

func (r Receipt) signingBytes() ([]byte, error) {
	if !r.validUnsigned() {
		return nil, ErrInvalidReceipt
	}
	encoded := make([]byte, 0, len(receiptDomain)+len(r.GroupID)+len(r.MemberID)+len(r.Tags)*40+128)
	encoded = appendString(encoded, receiptDomain)
	encoded = appendString(encoded, r.GroupID)
	encoded = binary.AppendUvarint(encoded, r.Epoch)
	encoded = append(encoded, r.ViewHash[:]...)
	encoded = appendString(encoded, r.MemberID)
	encoded = binary.AppendUvarint(encoded, r.Incarnation)
	encoded = binary.AppendUvarint(encoded, r.Sequence)
	encoded = append(encoded, r.CheckpointID[:]...)
	encoded = binary.AppendUvarint(encoded, uint64(len(r.Tags)))
	for _, tag := range r.Tags {
		encoded = appendString(encoded, tag.ReplicaID)
		encoded = binary.AppendUvarint(encoded, tag.WallTime)
		encoded = binary.AppendUvarint(encoded, tag.Logical)
	}
	return encoded, nil
}

// GCBridge accepts authenticated receipts only for the active signed view and
// feeds their exact tags to Coordinator. It serializes each member's sequence
// to reject stale/replayed packets before they reach the GC path.
type GCBridge[T comparable] struct {
	mu      sync.Mutex
	manager *Manager[T]
	lastSeq map[string]uint64
}

func NewGCBridge[T comparable](manager *Manager[T]) (*GCBridge[T], error) {
	if manager == nil || manager.Coordinator() == nil {
		return nil, ErrInvalidView
	}
	return &GCBridge[T]{manager: manager, lastSeq: make(map[string]uint64)}, nil
}

// Apply verifies receipt and performs exact acknowledgement/compaction on
// target. Callers must persist target's resulting OR-Set snapshot and HLC
// state before pruning acknowledgement records.
func (b *GCBridge[T]) Apply(receipt Receipt, target *set.ORSet[T]) (int, error) {
	if b == nil || target == nil {
		return 0, ErrInvalidReceipt
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	view := b.manager.View()
	if receipt.GroupID != view.GroupID || receipt.Epoch != view.Epoch || receipt.ViewHash != view.Hash() {
		return 0, ErrInvalidReceipt
	}
	member, ok := view.Member(receipt.MemberID)
	if !ok || member.Incarnation != receipt.Incarnation || verifyReceipt(receipt, member.PublicKey) != nil {
		return 0, ErrInvalidReceipt
	}
	if receipt.Sequence <= b.lastSeq[receipt.MemberID] {
		return 0, ErrReceiptReplay
	}
	removed, err := b.manager.Coordinator().AcknowledgeAndCompact(receipt.GroupID, receipt.MemberID, receipt.Epoch, receipt.Tags, target)
	if err != nil {
		return 0, err
	}
	b.lastSeq[receipt.MemberID] = receipt.Sequence
	return removed, nil
}

// SortedTags returns a sorted, duplicate-free copy suitable for a receipt.
// It is a convenience for producers that take tags from multiple chunks; it
// never infers a receipt from a frontier.
func SortedTags(tags []crdt.Tag) ([]crdt.Tag, error) {
	cloned := append([]crdt.Tag(nil), tags...)
	sort.Slice(cloned, func(left, right int) bool { return cloned[left].Compare(cloned[right]) < 0 })
	for index, tag := range cloned {
		if !tag.Valid() || (index > 0 && cloned[index-1] == tag) {
			return nil, ErrInvalidReceipt
		}
	}
	return cloned, nil
}
