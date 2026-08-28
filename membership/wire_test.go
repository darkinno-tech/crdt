package membership

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
)

func TestWireRoundTripsSignedMessages(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api", "mobile")
	encodedView, err := MarshalView(setup.view)
	if err != nil {
		t.Fatal(err)
	}
	decodedView, err := UnmarshalView(encodedView)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyView(decodedView, setup.authorityPublic); err != nil || decodedView.Hash() != setup.view.Hash() {
		t.Fatalf("view round trip = %v, %#v", err, decodedView)
	}

	gossip, err := NewGossip(setup.view, "api", setup.members["api"], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, err := gossip.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	encodedMessage, err := MarshalGossipMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	decodedMessage, err := UnmarshalGossipMessage(encodedMessage)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(message, decodedMessage) || verifyGossip(decodedMessage, setup.view.Members[0].PublicKey) != nil {
		t.Fatalf("gossip round trip = %#v", decodedMessage)
	}

	tag := crdt.Tag{ReplicaID: "api", WallTime: 2, Logical: 1}
	receipt, err := SignReceipt(Receipt{GroupID: setup.view.GroupID, Epoch: setup.view.Epoch, ViewHash: setup.view.Hash(), MemberID: "api", Incarnation: 1, Sequence: 1, CheckpointID: testCheckpointID("api", 1), Tags: []crdt.Tag{tag}}, setup.members["api"])
	if err != nil {
		t.Fatal(err)
	}
	encodedReceipt, err := MarshalReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decodedReceipt, err := UnmarshalReceipt(encodedReceipt)
	if err != nil || !reflect.DeepEqual(receipt, decodedReceipt) || verifyReceipt(decodedReceipt, setup.view.Members[0].PublicKey) != nil {
		t.Fatalf("receipt round trip = %v, %#v", err, decodedReceipt)
	}
}

func TestWireRejectsTruncationTrailingBytesAndNonCanonicalVarints(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api")
	encoded, err := MarshalView(setup.view)
	if err != nil {
		t.Fatal(err)
	}
	for index := range encoded {
		if _, err := UnmarshalView(encoded[:index]); !errors.Is(err, ErrInvalidView) {
			t.Fatalf("truncation at %d = %v", index, err)
		}
	}
	if _, err := UnmarshalView(append(encoded, 0)); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("trailing byte = %v", err)
	}
	// The group length varint follows the version byte. Encode the same short
	// length using an overlong varint; canonical decoding must reject it.
	nonCanonical := append([]byte{wireVersion, 0x89, 0x00}, encoded[2:]...)
	if _, err := UnmarshalView(nonCanonical); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("non-canonical varint = %v", err)
	}
	gossip, err := NewGossip(setup.view, "api", setup.members["api"], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, err := gossip.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	encodedMessage, err := MarshalGossipMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	for index := range encodedMessage {
		if _, err := UnmarshalGossipMessage(encodedMessage[:index]); !errors.Is(err, ErrInvalidGossip) {
			t.Fatalf("gossip truncation at %d = %v", index, err)
		}
	}
	if _, err := MarshalGossipMessage(GossipMessage{}); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("empty gossip marshal = %v", err)
	}
	receipt, err := SignReceipt(Receipt{GroupID: setup.view.GroupID, Epoch: 1, ViewHash: setup.view.Hash(), MemberID: "api", Incarnation: 1, Sequence: 1, CheckpointID: testCheckpointID("api", 1), Tags: []crdt.Tag{{ReplicaID: "api", WallTime: 1}}}, setup.members["api"])
	if err != nil {
		t.Fatal(err)
	}
	encodedReceipt, err := MarshalReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for index := range encodedReceipt {
		if _, err := UnmarshalReceipt(encodedReceipt[:index]); !errors.Is(err, ErrInvalidReceipt) {
			t.Fatalf("receipt truncation at %d = %v", index, err)
		}
	}
	if _, err := MarshalReceipt(Receipt{}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("empty receipt marshal = %v", err)
	}
}

func TestUnmarshalReceiptRejectsZeroCheckpointID(t *testing.T) {
	setup := newFixture(t, 1, "api")
	checkpointID := testCheckpointID("api", 1)
	receipt, err := SignReceipt(Receipt{
		GroupID:      setup.view.GroupID,
		Epoch:        setup.view.Epoch,
		ViewHash:     setup.view.Hash(),
		MemberID:     "api",
		Incarnation:  1,
		Sequence:     1,
		CheckpointID: checkpointID,
		Tags:         []crdt.Tag{{ReplicaID: "api", WallTime: 1}},
	}, setup.members["api"])
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	checkpointOffset := bytes.Index(encoded, checkpointID[:])
	if checkpointOffset < 0 {
		t.Fatal("encoded receipt did not contain checkpoint ID")
	}
	copy(encoded[checkpointOffset:checkpointOffset+len(checkpointID)], make([]byte, len(checkpointID)))
	if _, err := UnmarshalReceipt(encoded); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("UnmarshalReceipt(zero checkpoint ID) = %v, want %v", err, ErrInvalidReceipt)
	}
}
