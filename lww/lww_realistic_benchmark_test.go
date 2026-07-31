package lww

import (
	"reflect"
	"testing"
)

// BenchmarkMapThreeReplicaFramedDeliveryAndRecovery measures the public LWW
// replication path, rather than an in-memory merge: it includes canonical
// frame encoding, duplicate and out-of-order delivery, decoding, snapshot
// creation, same-ID recovery, and a second duplicate delivery pass.
func BenchmarkMapThreeReplicaFramedDeliveryAndRecovery(b *testing.B) {
	changes := benchmarkThreeReplicaMapChanges(b)
	wantKeys := []string{"cover", "status"}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		alice, err := NewMap("alice")
		if err != nil {
			b.Fatal(err)
		}
		bob, err := NewMap("bob")
		if err != nil {
			b.Fatal(err)
		}
		carol, err := NewMap("carol")
		if err != nil {
			b.Fatal(err)
		}
		for targetIndex, target := range []*Map{alice, bob, carol} {
			deliverMapChanges(b, target, changes, int64(20260731+index+targetIndex))
		}

		saved, err := bob.SnapshotCurrentState()
		if err != nil {
			b.Fatal(err)
		}
		recovered, err := NewMapFromSnapshot(saved)
		if err != nil {
			b.Fatal(err)
		}
		deliverMapChanges(b, recovered, changes, int64(20260801+index))
		if keys := recovered.Keys(); !reflect.DeepEqual(keys, wantKeys) {
			b.Fatalf("recovered keys = %#v, want %#v", keys, wantKeys)
		}
	}
}

func benchmarkThreeReplicaMapChanges(b testing.TB) []MapDelta {
	b.Helper()
	alice, err := NewMap("alice")
	if err != nil {
		b.Fatal(err)
	}
	bob, err := NewMap("bob")
	if err != nil {
		b.Fatal(err)
	}
	carol, err := NewMap("carol")
	if err != nil {
		b.Fatal(err)
	}
	title, err := alice.SetWithDelta("title", []byte("draft"))
	if err != nil {
		b.Fatal(err)
	}
	if err := bob.ApplyDelta(title); err != nil {
		b.Fatal(err)
	}
	status, err := bob.SetWithDelta("status", []byte("review"))
	if err != nil {
		b.Fatal(err)
	}
	cover, err := carol.SetWithDelta("cover", []byte("object-42"))
	if err != nil {
		b.Fatal(err)
	}
	removeTitle, err := alice.DeleteWithDelta("title")
	if err != nil {
		b.Fatal(err)
	}
	return []MapDelta{title, status, cover, removeTitle}
}
