package lww

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
)

func TestSetConvergesAndRetainsDeleteMetadata(t *testing.T) {
	left, err := NewSet[string]("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSet[string]("right")
	if err != nil {
		t.Fatal(err)
	}
	if err := left.Add("note"); err != nil {
		t.Fatal(err)
	}
	if err := right.Remove("note"); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	if left.Contains("note") != right.Contains("note") {
		t.Fatalf("replicas diverged")
	}
	state := left.State()
	if state.ElementCount+state.TombstoneCount != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestMapCopiesValuesAndConverges(t *testing.T) {
	left, err := NewMap("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewMap("right")
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("one")
	if err := left.Set("title", input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	if got, ok := left.Get("title"); !ok || !bytes.Equal(got, []byte("one")) {
		t.Fatalf("Get() = %q, %v", got, ok)
	}
	if err := right.Set("title", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	leftValue, leftOK := left.Get("title")
	rightValue, rightOK := right.Get("title")
	if leftOK != rightOK || !bytes.Equal(leftValue, rightValue) {
		t.Fatalf("replicas diverged: %q/%v %q/%v", leftValue, leftOK, rightValue, rightOK)
	}
	if err := left.Set(" ", []byte("bad")); err != ErrInvalidKey {
		t.Fatalf("Set(empty key) = %v", err)
	}
}

func TestLocalWritesCannotReplaceHigherKnownTag(t *testing.T) {
	set, err := NewSet[string]("local")
	if err != nil {
		t.Fatal(err)
	}
	highest := crdt.Tag{ReplicaID: "remote", WallTime: math.MaxUint64}
	set.entries["x"] = setEntry[string]{tag: highest, present: false}
	if err := set.Add("x"); err != nil {
		t.Fatal(err)
	}
	if set.Contains("x") {
		t.Fatal("older local tag replaced higher known remove")
	}

	value, err := NewMap("local-map")
	if err != nil {
		t.Fatal(err)
	}
	value.entries["x"] = mapEntry{tag: highest, present: true, value: []byte("remote")}
	if err := value.Set("x", []byte("local")); err != nil {
		t.Fatal(err)
	}
	if got, ok := value.Get("x"); !ok || !bytes.Equal(got, []byte("remote")) {
		t.Fatalf("Get() = %q, %v", got, ok)
	}
}

func TestMapMergeDoesNotAliasFutureSourceWrites(t *testing.T) {
	source, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMap("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Set("key", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(source); err != nil {
		t.Fatal(err)
	}
	if err := source.Set("key", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got, ok := target.Get("key"); !ok || !bytes.Equal(got, []byte("first")) {
		t.Fatalf("target Get() = %q, %v", got, ok)
	}
}

func TestMapMetadataIntrospectionAndValidation(t *testing.T) {
	value, err := NewMap("map")
	if err != nil {
		t.Fatal(err)
	}
	change, err := value.SetWithDelta("b", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if !value.HasEntry("a") || !value.HasEntry("b") || value.HasEntry("missing") || value.EntryCount() != 2 {
		t.Fatalf("entry metadata = %#v, %d", value.EntryKeys(), value.EntryCount())
	}
	if got := value.EntryKeys(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("EntryKeys() = %#v", got)
	}
	if got := change.Keys(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("delta Keys() = %#v", got)
	}
	if err := change.ValidateValues(func(key string, bytes []byte) error {
		if key != "b" || string(bytes) != "value" {
			t.Fatalf("delta callback = %q, %q", key, bytes)
		}
		bytes[0] = 'X'
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := value.Get("b"); !ok || string(got) != "value" {
		t.Fatalf("callback aliased map value = %q, %v", got, ok)
	}
	validatorErr := errors.New("reject")
	if err := value.ValidateValues(func(string, []byte) error { return validatorErr }); !errors.Is(err, validatorErr) {
		t.Fatalf("Map.ValidateValues() = %v", err)
	}
	if err := change.ValidateValues(nil); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("delta nil validator = %v", err)
	}
	var nilMap *Map
	if nilMap.HasEntry("x") || nilMap.EntryCount() != 0 || nilMap.EntryKeys() != nil {
		t.Fatal("nil map metadata accessors")
	}
	if err := nilMap.ValidateValues(func(string, []byte) error { return nil }); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map validator = %v", err)
	}
}

func TestLWWErrorPathsAndIntrospection(t *testing.T) {
	if _, err := NewSetFromClock[string](clock.State{}); err != ErrInvalidReplicaID {
		t.Fatalf("NewSetFromClock() = %v", err)
	}
	if _, err := NewMapFromClock(clock.State{}); err != ErrInvalidReplicaID {
		t.Fatalf("NewMapFromClock() = %v", err)
	}
	var nilSet *Set[string]
	if err := nilSet.Add("x"); err != ErrNilSet {
		t.Fatalf("nil Add() = %v", err)
	}
	if nilSet.Contains("x") || nilSet.Elements() != nil || nilSet.ClockState() != (clock.State{}) {
		t.Fatal("nil set accessors")
	}
	if state := nilSet.State(); state.Type != "lww-set" {
		t.Fatalf("nil State() = %+v", state)
	}

	set, err := NewSet[string]("set")
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Add("b"); err != nil {
		t.Fatal(err)
	}
	if err := set.Add("a"); err != nil {
		t.Fatal(err)
	}
	if err := set.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if got := set.Elements(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("Elements() = %#v", got)
	}
	if state := set.State(); state.ElementCount != 1 || state.TombstoneCount != 1 || set.ClockState().ReplicaID != "set" {
		t.Fatalf("set state = %+v", state)
	}
	if err := set.Merge(set); err != nil {
		t.Fatal(err)
	}
	if err := set.Merge(nil); err != ErrNilSet {
		t.Fatalf("Merge(nil) = %v", err)
	}

	var nilMap *Map
	if err := nilMap.Set("x", nil); err != ErrNilMap {
		t.Fatalf("nil Set() = %v", err)
	}
	if _, ok := nilMap.Get("x"); ok || nilMap.Keys() != nil || nilMap.ClockState() != (clock.State{}) {
		t.Fatal("nil map accessors")
	}
	if state := nilMap.State(); state.Type != "lww-map" {
		t.Fatalf("nil State() = %+v", state)
	}
	value, err := NewMap("map")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Set("b", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := value.Set("a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := value.Delete("b"); err != nil {
		t.Fatal(err)
	}
	if got := value.Keys(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("Keys() = %#v", got)
	}
	if _, ok := value.Get("b"); ok {
		t.Fatal("deleted value is visible")
	}
	if state := value.State(); state.ElementCount != 1 || state.TombstoneCount != 1 || value.ClockState().ReplicaID != "map" {
		t.Fatalf("map state = %+v", state)
	}
	if err := value.Merge(value); err != nil {
		t.Fatal(err)
	}
	if err := value.Merge(nil); err != ErrNilMap {
		t.Fatalf("Merge(nil) = %v", err)
	}
}

func TestLWWConflictingEqualTagsLeaveReceiverUnchanged(t *testing.T) {
	tag := crdt.Tag{ReplicaID: "remote", WallTime: 1}
	left, err := NewSet[string]("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSet[string]("right")
	if err != nil {
		t.Fatal(err)
	}
	left.entries["x"] = setEntry[string]{tag: tag, present: true}
	right.entries["x"] = setEntry[string]{tag: tag, present: false}
	if err := left.Merge(right); err != ErrTagConflict || !left.Contains("x") {
		t.Fatalf("set Merge() = %v", err)
	}

	leftMap, err := NewMap("left-map")
	if err != nil {
		t.Fatal(err)
	}
	rightMap, err := NewMap("right-map")
	if err != nil {
		t.Fatal(err)
	}
	leftMap.entries["x"] = mapEntry{tag: tag, present: true, value: []byte("left")}
	rightMap.entries["x"] = mapEntry{tag: tag, present: true, value: []byte("right")}
	if err := leftMap.Merge(rightMap); err != ErrTagConflict {
		t.Fatalf("map Merge() = %v", err)
	}
	if got, _ := leftMap.Get("x"); !bytes.Equal(got, []byte("left")) {
		t.Fatalf("receiver changed to %q", got)
	}
}
