package redis

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	"github.com/darkinno-tech/crdt/durable"
	"github.com/darkinno-tech/crdt/replica"
	"github.com/alicebob/miniredis/v2"
	redisclient "github.com/redis/go-redis/v9"
)

func TestStoreAppendsRetriesConflictsAndReplays(t *testing.T) {
	store, cleanup := testStore(t, 32, 1<<20)
	defer cleanup()
	manifest := testManifest(t)
	first := testChange(t, manifest, "alice", 1, 2)
	appended, err := store.Append(manifest.GroupID, first)
	if err != nil || appended.Duplicate || appended.Event.Sequence != 1 {
		t.Fatalf("append = %+v, %v", appended, err)
	}
	retry, err := store.Append(manifest.GroupID, first)
	if err != nil || !retry.Duplicate || retry.Event.Sequence != 1 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 1, 9)); !errors.Is(err, durable.ErrConflictingDot) {
		t.Fatalf("conflict = %v, want %v", err, durable.ErrConflictingDot)
	}
	second := testChange(t, manifest, "bob", 1, 3)
	if result, err := store.Append(manifest.GroupID, second); err != nil || result.Event.Sequence != 2 {
		t.Fatalf("append second = %+v, %v", result, err)
	}
	events, highWater, err := store.Replay(manifest.GroupID, 0, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 2 || len(events) != 2 || events[0].Change.Dot != first.Dot || events[1].Change.Dot != second.Dot {
		t.Fatalf("replay = high=%d events=%+v err=%v", highWater, events, err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrReplayUnavailable) {
		t.Fatalf("partial replay = %v, want %v", err, durable.ErrReplayUnavailable)
	}
}

func TestStoreRejectsOverflowCorruptionAndClosedUse(t *testing.T) {
	store, cleanup := testStore(t, 1, 1<<20)
	defer cleanup()
	manifest := testManifest(t)
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 2, 2)); !errors.Is(err, durable.ErrStoreFull) {
		t.Fatalf("overflow = %v, want %v", err, durable.ErrStoreFull)
	}
	if _, _, err := store.Replay(manifest.GroupID, 2, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrReplayUnavailable) {
		t.Fatalf("future cursor = %v, want %v", err, durable.ErrReplayUnavailable)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 3, 3)); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("closed append = %v, want %v", err, durable.ErrClosed)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("closed replay = %v, want %v", err, durable.ErrClosed)
	}
}

func TestStoreConcurrentAppendsKeepContiguousReplay(t *testing.T) {
	store, cleanup := testStore(t, 64, 1<<20)
	defer cleanup()
	manifest := testManifest(t)
	var group sync.WaitGroup
	errors := make(chan error, 32)
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, err := store.Append(manifest.GroupID, testChange(t, manifest, "actor-"+string(rune('a'+index)), 1, uint64(index+1)))
			errors <- err
		}(index)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, highWater, err := store.Replay(manifest.GroupID, 0, 32, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 32 || len(events) != 32 {
		t.Fatalf("concurrent replay = high=%d len=%d err=%v", highWater, len(events), err)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) {
			t.Fatalf("event %d sequence = %d", index, event.Sequence)
		}
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	client := redisclient.NewClient(&redisclient.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = client.Close() }()
	for _, config := range []Config{{}, {Prefix: " ", MaxEvents: 1, MaxBytes: 1}, {Prefix: "test", MaxEvents: maxLuaExactInteger + 1, MaxBytes: 1}, {Prefix: "test", MaxEvents: 1, MaxBytes: maxLuaExactInteger + 1}} {
		if _, err := New(client, config); !errors.Is(err, durable.ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", config, err)
		}
	}
	valid, err := New(client, Config{Prefix: "test", MaxEvents: 1, MaxBytes: 1, Timeout: -1})
	if valid != nil || !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("negative timeout New = %v, %v", valid, err)
	}
}

func TestStoreFailsClosedOnCorruptMetadataAndEnvelope(t *testing.T) {
	store, cleanup := testStore(t, 8, 1<<20)
	defer cleanup()
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	keys := store.keys(manifest.GroupID)
	ctx := context.Background()
	if err := store.client.HSet(ctx, keys[0], "group_id", "other", "high_water", "0", "event_count", "0", "used_bytes", "0").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(manifest.GroupID, change); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("wrong group append = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("wrong group replay = %v", err)
	}

	store, cleanup = testStore(t, 8, 1<<20)
	defer cleanup()
	keys = store.keys(manifest.GroupID)
	if err := store.client.HSet(ctx, keys[0], "group_id", manifest.GroupID, "high_water", "1", "event_count", "1", "used_bytes", "1").Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.client.HSet(ctx, keys[2], "1", []byte{0}).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("bad envelope replay = %v", err)
	}
}

func TestStoreRejectsExactSequenceLimitAndInvalidReplay(t *testing.T) {
	store, cleanup := testStore(t, 8, 1<<20)
	defer cleanup()
	manifest := testManifest(t)
	keys := store.keys(manifest.GroupID)
	ctx := context.Background()
	if err := store.client.HSet(ctx, keys[0], "group_id", manifest.GroupID, "high_water", strconv.FormatUint(maxLuaExactInteger, 10), "event_count", "0", "used_bytes", "0").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, ErrSequenceRange) {
		t.Fatalf("sequence limit = %v, want %v", err, ErrSequenceRange)
	}
	if _, _, err := store.Replay("unused", 1, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, durable.ErrReplayUnavailable) {
		t.Fatalf("empty future replay = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 0, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid replay = %v", err)
	}
	if _, err := store.Append(" ", testChange(t, manifest, "bob", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid group append = %v", err)
	}
}

func TestHelpersRejectMalformedValues(t *testing.T) {
	if !allNil([]interface{}{nil, nil}) || allNil([]interface{}{nil, "value"}) {
		t.Fatal("allNil mismatch")
	}
	if got := stringValue(int64(12)); got != "12" {
		t.Fatalf("int value = %q", got)
	}
	if got := stringValue(12); got != "12" {
		t.Fatalf("int value = %q", got)
	}
	if got := stringValue([]byte("bytes")); got != "bytes" || stringValue(struct{}{}) != "" {
		t.Fatal("string conversion mismatch")
	}
	if _, ok := uintValue("bad"); ok {
		t.Fatal("invalid uint accepted")
	}
	for _, value := range []interface{}{nil, []interface{}{int64(1)}, []interface{}{"bad", "1"}, []interface{}{int64(1), "bad"}} {
		if _, _, err := scriptResult(value); err == nil {
			t.Fatalf("script result %T accepted", value)
		}
	}
}

func TestStoreCoversEmptyAndInputBoundaries(t *testing.T) {
	store, cleanup := testStore(t, 8, 128)
	defer cleanup()
	manifest := testManifest(t)
	if _, high, err := store.Replay(manifest.GroupID, 0, 8, 128, manifest, crdt.ProtocolPolicy{}, 128, 128); err != nil || high != 0 {
		t.Fatalf("empty replay = high=%d err=%v", high, err)
	}
	if _, err := store.Append(manifest.GroupID, replica.Change{}); err == nil {
		t.Fatal("invalid change append succeeded")
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if events, high, err := store.Replay(manifest.GroupID, 1, 8, 128, manifest, crdt.ProtocolPolicy{}, 128, 128); err != nil || high != 1 || len(events) != 0 {
		t.Fatalf("caught-up replay = high=%d events=%d err=%v", high, len(events), err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("second close = %v", err)
	}
}

func testStore(t testing.TB, maxEvents, maxBytes uint64) (*Store, func()) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redisclient.NewClient(&redisclient.Options{Addr: server.Addr()})
	store, err := New(client, Config{Prefix: "crdt-test", MaxEvents: maxEvents, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	return store, func() { _ = client.Close() }
}

func testManifest(t testing.TB) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("redis-counter", "example.com/redis-counter/v1", 1, replica.Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testChange(t testing.TB, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := state.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return change
}
