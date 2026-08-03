package counter

import (
	"math/big"
	"testing"
)

func TestGCounterApplyDeltaChangedClassifiesSubsumedDelivery(t *testing.T) {
	source := mustNewGCounter(t, "source")
	target := mustNewGCounter(t, "target")
	delta, err := source.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := target.ApplyDeltaChanged(delta); err != nil || !changed {
		t.Fatalf("first ApplyDeltaChanged = %t, %v, want changed", changed, err)
	}
	if changed, err := target.ApplyDeltaChanged(delta); err != nil || changed {
		t.Fatalf("duplicate ApplyDeltaChanged = %t, %v, want unchanged", changed, err)
	}
}

func TestPNCounterApplyDeltaChangedClassifiesBothComponents(t *testing.T) {
	source := mustNewPNCounter(t, "source")
	target := mustNewPNCounter(t, "target")
	positive, err := source.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	negative, err := source.Decrement(3)
	if err != nil {
		t.Fatal(err)
	}
	for name, delta := range map[string]PNCounterDelta{"positive": positive, "negative": negative} {
		if changed, err := target.ApplyDeltaChanged(delta); err != nil || !changed {
			t.Fatalf("%s ApplyDeltaChanged = %t, %v, want changed", name, changed, err)
		}
		if changed, err := target.ApplyDeltaChanged(delta); err != nil || changed {
			t.Fatalf("%s duplicate ApplyDeltaChanged = %t, %v, want unchanged", name, changed, err)
		}
	}
	assertPNValue(t, target, big.NewInt(4))
}
