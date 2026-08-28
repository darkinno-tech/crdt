package membership

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"

	"github.com/im10furry/crdt"
)

const (
	wireVersion    byte = 1
	maxWireBytes        = 1 << 20
	maxReceiptTags      = 8192
)

// MarshalView returns the bounded canonical wire representation of a signed
// View. Call VerifyView with the configured authority key after decoding.
func MarshalView(view View) ([]byte, error) {
	if !view.validUnsigned() || len(view.Signature) != ed25519.SignatureSize {
		return nil, ErrInvalidView
	}
	encoded := []byte{wireVersion}
	encoded = appendString(encoded, view.GroupID)
	encoded = binary.AppendUvarint(encoded, view.Epoch)
	encoded = append(encoded, view.PreviousHash[:]...)
	encoded = append(encoded, view.ManifestHash[:]...)
	encoded = binary.AppendUvarint(encoded, uint64(len(view.Members)))
	for _, member := range view.Members {
		encoded = appendString(encoded, member.ID)
		encoded = append(encoded, member.PublicKey...)
		encoded = binary.AppendUvarint(encoded, member.Incarnation)
	}
	encoded = append(encoded, view.Signature...)
	return encoded, nil
}

// UnmarshalView decodes a bounded canonical View. It validates only structure;
// callers must invoke VerifyView before treating it as authoritative.
func UnmarshalView(data []byte) (View, error) {
	reader, err := newWireReader(data)
	if err != nil {
		return View{}, err
	}
	groupID, err := reader.string(maxMemberIDSize)
	if err != nil {
		return View{}, ErrInvalidView
	}
	epoch, err := reader.uvarint()
	if err != nil {
		return View{}, ErrInvalidView
	}
	previous, err := reader.bytes(sha256.Size)
	if err != nil {
		return View{}, ErrInvalidView
	}
	manifest, err := reader.bytes(sha256.Size)
	if err != nil {
		return View{}, ErrInvalidView
	}
	memberCount, err := reader.uvarint()
	if err != nil || memberCount == 0 || memberCount > maxMembers {
		return View{}, ErrInvalidView
	}
	view := View{GroupID: groupID, Epoch: epoch}
	copy(view.PreviousHash[:], previous)
	copy(view.ManifestHash[:], manifest)
	view.Members = make([]Member, int(memberCount))
	for index := range view.Members {
		id, err := reader.string(maxMemberIDSize)
		if err != nil {
			return View{}, ErrInvalidView
		}
		key, err := reader.bytes(ed25519.PublicKeySize)
		if err != nil {
			return View{}, ErrInvalidView
		}
		incarnation, err := reader.uvarint()
		if err != nil {
			return View{}, ErrInvalidView
		}
		view.Members[index] = Member{ID: id, PublicKey: append(ed25519.PublicKey(nil), key...), Incarnation: incarnation}
	}
	signature, err := reader.bytes(ed25519.SignatureSize)
	if err != nil || !reader.done() {
		return View{}, ErrInvalidView
	}
	view.Signature = append([]byte(nil), signature...)
	if !view.validUnsigned() {
		return View{}, ErrInvalidView
	}
	return view, nil
}

// MarshalGossipMessage returns a bounded canonical signed heartbeat.
func MarshalGossipMessage(message GossipMessage) ([]byte, error) {
	if !message.validUnsigned() || len(message.Signature) != ed25519.SignatureSize {
		return nil, ErrInvalidGossip
	}
	encoded := []byte{wireVersion}
	encoded = appendString(encoded, message.GroupID)
	encoded = binary.AppendUvarint(encoded, message.Epoch)
	encoded = append(encoded, message.ViewHash[:]...)
	encoded = appendString(encoded, message.From)
	encoded = binary.AppendUvarint(encoded, message.Incarnation)
	encoded = binary.AppendUvarint(encoded, message.Counter)
	encoded = append(encoded, message.Signature...)
	return encoded, nil
}

// UnmarshalGossipMessage decodes a bounded canonical heartbeat. Observe
// verifies its signature against the current View before recording liveness.
func UnmarshalGossipMessage(data []byte) (GossipMessage, error) {
	reader, err := newWireReader(data)
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	groupID, err := reader.string(maxMemberIDSize)
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	epoch, err := reader.uvarint()
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	viewHash, err := reader.bytes(sha256.Size)
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	from, err := reader.string(maxMemberIDSize)
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	incarnation, err := reader.uvarint()
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	counter, err := reader.uvarint()
	if err != nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	signature, err := reader.bytes(ed25519.SignatureSize)
	if err != nil || !reader.done() {
		return GossipMessage{}, ErrInvalidGossip
	}
	message := GossipMessage{GroupID: groupID, Epoch: epoch, From: from, Incarnation: incarnation, Counter: counter, Signature: append([]byte(nil), signature...)}
	copy(message.ViewHash[:], viewHash)
	if !message.validUnsigned() {
		return GossipMessage{}, ErrInvalidGossip
	}
	return message, nil
}

// MarshalReceipt returns a bounded canonical signed tombstone receipt.
func MarshalReceipt(receipt Receipt) ([]byte, error) {
	if !receipt.validUnsigned() || len(receipt.Signature) != ed25519.SignatureSize || len(receipt.Tags) > maxReceiptTags {
		return nil, ErrInvalidReceipt
	}
	encoded := []byte{wireVersion}
	encoded = appendString(encoded, receipt.GroupID)
	encoded = binary.AppendUvarint(encoded, receipt.Epoch)
	encoded = append(encoded, receipt.ViewHash[:]...)
	encoded = appendString(encoded, receipt.MemberID)
	encoded = binary.AppendUvarint(encoded, receipt.Incarnation)
	encoded = binary.AppendUvarint(encoded, receipt.Sequence)
	encoded = append(encoded, receipt.CheckpointID[:]...)
	encoded = binary.AppendUvarint(encoded, uint64(len(receipt.Tags)))
	for _, tag := range receipt.Tags {
		encoded = appendString(encoded, tag.ReplicaID)
		encoded = binary.AppendUvarint(encoded, tag.WallTime)
		encoded = binary.AppendUvarint(encoded, tag.Logical)
	}
	encoded = append(encoded, receipt.Signature...)
	if len(encoded) > maxWireBytes {
		return nil, ErrInvalidReceipt
	}
	return encoded, nil
}

// UnmarshalReceipt decodes a bounded canonical receipt. GCBridge.Apply
// verifies signature, view binding, member incarnation, and replay sequence.
func UnmarshalReceipt(data []byte) (Receipt, error) {
	reader, err := newWireReader(data)
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	groupID, err := reader.string(maxMemberIDSize)
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	epoch, err := reader.uvarint()
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	viewHash, err := reader.bytes(sha256.Size)
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	memberID, err := reader.string(maxMemberIDSize)
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	incarnation, err := reader.uvarint()
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	sequence, err := reader.uvarint()
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	checkpointID, err := reader.bytes(sha256.Size)
	if err != nil {
		return Receipt{}, ErrInvalidReceipt
	}
	tagCount, err := reader.uvarint()
	if err != nil || tagCount > maxReceiptTags {
		return Receipt{}, ErrInvalidReceipt
	}
	receipt := Receipt{GroupID: groupID, Epoch: epoch, MemberID: memberID, Incarnation: incarnation, Sequence: sequence, Tags: make([]crdt.Tag, int(tagCount))}
	copy(receipt.ViewHash[:], viewHash)
	copy(receipt.CheckpointID[:], checkpointID)
	for index := range receipt.Tags {
		replicaID, err := reader.string(maxMemberIDSize)
		if err != nil {
			return Receipt{}, ErrInvalidReceipt
		}
		wallTime, err := reader.uvarint()
		if err != nil {
			return Receipt{}, ErrInvalidReceipt
		}
		logical, err := reader.uvarint()
		if err != nil {
			return Receipt{}, ErrInvalidReceipt
		}
		receipt.Tags[index] = crdt.Tag{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
	}
	signature, err := reader.bytes(ed25519.SignatureSize)
	if err != nil || !reader.done() {
		return Receipt{}, ErrInvalidReceipt
	}
	receipt.Signature = append([]byte(nil), signature...)
	if !receipt.validUnsigned() {
		return Receipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

type wireReader struct {
	data []byte
	off  int
}

func newWireReader(data []byte) (*wireReader, error) {
	if len(data) < 1 || len(data) > maxWireBytes || data[0] != wireVersion {
		return nil, ErrInvalidView
	}
	return &wireReader{data: data, off: 1}, nil
}

func (r *wireReader) uvarint() (uint64, error) {
	value, size := binary.Uvarint(r.data[r.off:])
	if size <= 0 {
		return 0, ErrInvalidView
	}
	canonical := binary.AppendUvarint(nil, value)
	if len(canonical) != size {
		return 0, ErrInvalidView
	}
	r.off += size
	return value, nil
}

func (r *wireReader) bytes(size int) ([]byte, error) {
	if size < 0 || len(r.data)-r.off < size {
		return nil, ErrInvalidView
	}
	value := r.data[r.off : r.off+size]
	r.off += size
	return value, nil
}

func (r *wireReader) string(max int) (string, error) {
	length, err := r.uvarint()
	if err != nil || length > uint64(max) || length > uint64(len(r.data)-r.off) {
		return "", ErrInvalidView
	}
	value, err := r.bytes(int(length))
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (r *wireReader) done() bool { return r.off == len(r.data) }
