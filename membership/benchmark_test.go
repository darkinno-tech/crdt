package membership

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkVerifyView(b *testing.B) {
	for _, memberCount := range []int{3, 16, 64} {
		b.Run(fmt.Sprintf("members_%d", memberCount), func(b *testing.B) {
			ids := make([]string, memberCount)
			for index := range ids {
				ids[index] = fmt.Sprintf("member-%03d", index)
			}
			setup := newFixture(b, 1, ids...)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := VerifyView(setup.view, setup.authorityPublic); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGossipHeartbeatAndObserve(b *testing.B) {
	setup := newFixture(b, 1, "api", "mobile", "warehouse")
	api, err := NewGossip(setup.view, "api", setup.members["api"], time.Minute)
	if err != nil {
		b.Fatal(err)
	}
	mobile, err := NewGossip(setup.view, "mobile", setup.members["mobile"], time.Minute)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Unix(1, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		message, err := mobile.Heartbeat()
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := api.Observe(message, now.Add(time.Duration(index))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGCBridgeFinalReceipt(b *testing.B) {
	for _, tombstoneCount := range []int{32, 256} {
		b.Run(fmt.Sprintf("tombstones_%d", tombstoneCount), func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				setup := newFixture(b, 1, "api", "mobile", "warehouse")
				manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
				if err != nil {
					b.Fatal(err)
				}
				bridge, err := NewGCBridge(manager)
				if err != nil {
					b.Fatal(err)
				}
				target := mustORSet(b, "api")
				for tagIndex := 0; tagIndex < tombstoneCount; tagIndex++ {
					value := fmt.Sprintf("order-%d", tagIndex)
					if _, err := target.Add(value); err != nil {
						b.Fatal(err)
					}
					if _, err := target.Remove(value); err != nil {
						b.Fatal(err)
					}
				}
				tags, err := SortedTags(target.TombstoneTags())
				if err != nil {
					b.Fatal(err)
				}
				for receiptIndex, memberID := range []string{"api", "mobile"} {
					receipt := signedReceipt(b, setup, setup.view, memberID, uint64(receiptIndex+1), tags)
					if _, err := bridge.Apply(receipt, target); err != nil {
						b.Fatal(err)
					}
				}
				receipt := signedReceipt(b, setup, setup.view, "warehouse", 3, tags)
				b.StartTimer()
				removed, err := bridge.Apply(receipt, target)
				b.StopTimer()
				if err != nil || removed != tombstoneCount {
					b.Fatalf("final receipt = %d, %v", removed, err)
				}
			}
		})
	}
}
