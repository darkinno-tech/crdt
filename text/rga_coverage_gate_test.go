package text

import (
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/snapshot"
)

func TestRGARunWireFailureAndLimitPaths(t *testing.T) {
	var nilRGA *RGA
	if _, err := nilRGA.MarshalRunBinary(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil MarshalRunBinary = %v", err)
	}
	if err := nilRGA.UnmarshalRunBinary(nil); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil UnmarshalRunBinary = %v", err)
	}
	if _, err := nilRGA.SnapshotRunCurrentState(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil SnapshotRunCurrentState = %v", err)
	}

	first := Position{ReplicaID: "source", WallTime: 1}
	second := Position{ReplicaID: "source", WallTime: 2}
	nodes := map[Position]node{
		first:  {rune: 'a'},
		second: {parent: first, rune: 'b'},
	}
	limits := frame.DefaultLimits()
	if _, err := marshalRGARun(crdt.TypeIDRGARunState, map[Position]node{second: nodes[second]}, nil, limits); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("incomplete run state = %v", err)
	}
	if _, err := marshalRGARun(crdt.TypeIDRGARunDelta, map[Position]node{{}: {rune: 'x'}}, nil, limits); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid run delta = %v", err)
	}
	limitedNodes := limits
	limitedNodes.MaxElements = 1
	if _, err := marshalRGARun(crdt.TypeIDRGARunState, nodes, nil, limitedNodes); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("node-limited run state = %v", err)
	}
	limitedString := limits
	limitedString.MaxStringBytes = 1
	if _, err := runPayloadSize([][]runNode{{{id: first, item: nodes[first]}}}, nil, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string-limited run payload = %v", err)
	}
	limitedPayload := limits
	limitedPayload.MaxPayload = 1
	if _, err := runPayloadSize([][]runNode{{{id: first, item: nodes[first]}}}, nil, limitedPayload); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload-limited run payload = %v", err)
	}
	if size, err := runPayloadSize([][]runNode{{{id: first, item: nodes[first]}, {id: second, item: nodes[second]}}}, map[Position]struct{}{first: {}}, limits); err != nil || size == 0 {
		t.Fatalf("chain run payload = %d, %v", size, err)
	}
	longParent := Position{ReplicaID: "long", WallTime: 1}
	if _, err := runPayloadSize([][]runNode{{{id: first, item: node{parent: longParent, rune: 'a'}}}}, nil, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("parent string-limited run payload = %v", err)
	}
	if _, err := runPayloadSize(nil, map[Position]struct{}{longParent: {}}, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tombstone string-limited run payload = %v", err)
	}
	encodedNode, err := appendRunNode(nil, runNode{id: second, item: nodes[second]})
	if err != nil {
		t.Fatal(err)
	}
	if scalar, ok := encodeRunScalar('界'); !ok || scalar != uint64('界') {
		t.Fatalf("encodeRunScalar = %d, %v", scalar, ok)
	}
	if _, ok := encodeRunScalar(rune(-1)); ok {
		t.Fatal("encodeRunScalar accepted a negative scalar")
	}
	if _, ok := decodeRunScalar(uint64(utf8.MaxRune) + 1); ok {
		t.Fatal("decodeRunScalar accepted an out-of-range scalar")
	}
	if _, err := appendRunNode(nil, runNode{id: second, item: node{rune: rune(-1)}}); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("append invalid scalar = %v", err)
	}
	if got, item, _, ok := readRunNode(encodedNode, 0, limits); !ok || got != second || item != nodes[second] {
		t.Fatalf("readRunNode = %#v, %#v, %v", got, item, ok)
	}
	if _, _, _, ok := readRunNode(nil, 0, limits); ok {
		t.Fatal("empty run node accepted")
	}
	badParent := frame.AppendTag(nil, first)
	badParent = frame.AppendUvarint(badParent, 2)
	if _, _, _, ok := readRunNode(badParent, 0, limits); ok {
		t.Fatal("invalid parent flag accepted")
	}
	anchor := Position{ReplicaID: "anchor", WallTime: 1}
	branch := Position{ReplicaID: "other", WallTime: 1}
	completeNodes := map[Position]node{
		anchor: {rune: 'a'},
		first:  {parent: anchor, rune: 'b'},
		second: {parent: first, rune: 'c'},
		branch: {parent: anchor, rune: 'd'},
	}
	complete, err := marshalRGARun(crdt.TypeIDRGARunState, completeNodes, map[Position]struct{}{branch: {}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	decodedNodes, decodedTombstones, err := unmarshalRGARun(complete, crdt.TypeIDRGARunState, limits, true, nil)
	if err != nil || len(decodedNodes) != len(completeNodes) || len(decodedTombstones) != 1 {
		t.Fatalf("mixed run state decode = nodes=%d tombstones=%d err=%v", len(decodedNodes), len(decodedTombstones), err)
	}

	for name, payload := range map[string][]byte{
		"bad-block-kind":   frame.AppendUvarint(frame.AppendUvarint(nil, 1), 2),
		"truncated-node":   frame.AppendUvarint(frame.AppendUvarint(nil, 1), runBlockNode),
		"short-chain":      frame.AppendUvarint(frame.AppendUvarint(frame.AppendUvarint(nil, 1), runBlockChain), 1),
		"trailing-payload": append(frame.AppendUvarint(nil, 0), 0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGARunDelta, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := unmarshalRGARun(data, crdt.TypeIDRGARunDelta, limits, false, nil); !errors.Is(err, frame.ErrInvalidFrame) {
				t.Fatalf("unmarshal invalid run = %v", err)
			}
		})
	}
	chainParentFlag := frame.AppendUvarint(nil, 1)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, runBlockChain)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, 2)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, uint64(len(first.ReplicaID)))
	chainParentFlag = append(chainParentFlag, first.ReplicaID...)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, 2)
	data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGARunDelta, Payload: chainParentFlag})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unmarshalRGARun(data, crdt.TypeIDRGARunDelta, limits, false, nil); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid chain parent flag = %v", err)
	}
}

func TestRGARunInstallSnapshotAndJSONCoverage(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(Delta{nodes: map[Position]node{}, tombstones: map[Position]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	plain, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshot(plain); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("snapshot without clock = %v", err)
	}
	if _, err := NewFromClockWithOptions(clock.State{ReplicaID: "source"}, Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}
	resourceLimited, err := NewWithOptions("target", Options{MaxNodes: 1, MaxTombstones: 1, MaxPendingNodes: 1, MaxPendingBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := resourceLimited.UnmarshalRunBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("run state over receiver limits = %v", err)
	}
	pending, err := New("pending")
	if err != nil {
		t.Fatal(err)
	}
	pending.pending[Position{ReplicaID: "missing", WallTime: 1}] = node{parent: Position{ReplicaID: "parent", WallTime: 1}, rune: 'x'}
	if _, err := pending.SnapshotRunCurrentState(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("pending run snapshot = %v", err)
	}
	if _, err := pending.MarshalRunBinary(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("pending run marshal = %v", err)
	}
	if err := value.Merge(nil); !errors.Is(err, ErrNilText) {
		t.Fatalf("merge nil = %v", err)
	}
	if err := value.Merge(value); err != nil {
		t.Fatalf("merge self = %v", err)
	}
	if _, _, err := value.MarshalBinaryWithClockState(); err != nil {
		t.Fatalf("marshal clock state = %v", err)
	}
	if _, err := value.Snapshot(nil); err != nil {
		t.Fatalf("explicit snapshot = %v", err)
	}
}
