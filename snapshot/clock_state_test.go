package snapshot_test

import (
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	"github.com/im10furry/crdt/set"
	"github.com/im10furry/crdt/snapshot"
)

type snapshotStringCodec struct{}

func (snapshotStringCodec) ID() string                           { return "example.com/string/v1" }
func (snapshotStringCodec) Marshal(value string) ([]byte, error) { return []byte(value), nil }
func (snapshotStringCodec) Unmarshal(data []byte) (string, error) {
	return string(data), nil
}

func TestORSetSnapshotCarriesClockStateThroughRecoveryPlan(t *testing.T) {
	codec := snapshotStringCodec{}
	source, err := set.NewORSetFromClock(clock.State{ReplicaID: "replica", WallTime: 1 << 63, Logical: 2}, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Add("item"); err != nil {
		t.Fatal(err)
	}
	state, clockState, err := source.MarshalBinaryWithClockState()
	if err != nil {
		t.Fatal(err)
	}
	frontier := map[string]crdt.Tag{"replica": {ReplicaID: "replica", WallTime: clockState.WallTime, Logical: clockState.Logical}}
	saved, err := snapshot.NewWithClockState(state, frontier, clockState)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := snapshot.NewRecoveryPlan(saved, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := plan.Snapshot.ClockState()
	if !ok || got != clockState {
		t.Fatalf("recovered clock state = %#v, %v; want %#v, true", got, ok, clockState)
	}
}
