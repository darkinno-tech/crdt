package documenttree

import (
	"bytes"
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestDocumentTreeV1FramesRequireExplicitOfflineMigration(t *testing.T) {
	source := mustDocument(t, "source")
	root, rootDelta, err := source.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	board, _, err := root.CreateMap("board")
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := board.CreateArray("items")
	if err != nil {
		t.Fatal(err)
	}
	item, _, err := items.InsertMap(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := item.Set("title", []byte("replicated in the parent frame")); err != nil {
		t.Fatal(err)
	}

	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacyState := withTypeID(t, state, legacyDocumentTreeStateTypeID)
	target := mustDocument(t, "target")
	if err := target.UnmarshalBinary(legacyState); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("v2 receiver accepted v1 state = %v", err)
	}
	migratedState, err := MigrateV1State(legacyState, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := frame.UnmarshalFrame(migratedState, frame.DefaultLimits())
	if err != nil || decoded.TypeID != crdt.TypeIDDocumentTreeState {
		t.Fatalf("migrated state frame = %#v, %v", decoded, err)
	}
	if err := target.UnmarshalBinary(migratedState); err != nil {
		t.Fatal(err)
	}
	got, err := target.MarshalBinary()
	if err != nil || !bytes.Equal(got, state) {
		t.Fatalf("migrated state changed nested tree = %v\n got: %x\nwant: %x", err, got, state)
	}

	v2Delta, err := rootDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacyDelta := withTypeID(t, v2Delta, legacyDocumentTreeDeltaTypeID)
	if _, err := UnmarshalDelta(legacyDelta); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("v2 receiver accepted v1 delta = %v", err)
	}
	migratedDelta, err := MigrateV1Delta(legacyDelta, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	change, err := UnmarshalDelta(migratedDelta)
	if err != nil {
		t.Fatal(err)
	}
	other := mustDocument(t, "other")
	if err := other.ApplyDelta(change); err != nil {
		t.Fatal(err)
	}
	if _, ok := other.RootMap("workspace"); !ok {
		t.Fatal("migrated delta did not retain the root declaration")
	}
}

func TestDocumentTreeV1MigrationRejectsFormerLazyValue(t *testing.T) {
	source := mustDocument(t, "source")
	root, _, err := source.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Set("value", []byte("x")); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacy := withTypeID(t, state, legacyDocumentTreeStateTypeID)
	decoded, err := frame.UnmarshalFrame(legacy, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), decoded.Payload...)
	valueOffset := bytes.LastIndex(payload, []byte{byte(ValueBytes), 1, 'x'})
	if valueOffset < 0 {
		t.Fatalf("scalar value not found in canonical payload: %x", payload)
	}
	payload[valueOffset] = 3 // document-tree-v1's removed lazy-reference kind.
	legacyLazy, err := frame.MarshalFrame(frame.Frame{TypeID: legacyDocumentTreeStateTypeID, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateV1State(legacyLazy, DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("migration accepted a former lazy value = %v", err)
	}
}

func TestDocumentTreeV1MigrationRejectsInvalidInputBeforeConversion(t *testing.T) {
	if _, err := MigrateV1State([]byte("not-a-frame"), DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid legacy state = %v", err)
	}
	if _, err := MigrateV1Delta([]byte("not-a-frame"), DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid legacy delta = %v", err)
	}

	source := mustDocument(t, "source")
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateV1State(withTypeID(t, state, legacyDocumentTreeStateTypeID), Options{}, frame.DefaultLimits()); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid migration options = %v", err)
	}
}

func withTypeID(t testing.TB, data []byte, typeID uint64) []byte {
	t.Helper()
	decoded, err := frame.UnmarshalFrame(data, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: typeID, CodecID: decoded.CodecID, Payload: decoded.Payload})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
