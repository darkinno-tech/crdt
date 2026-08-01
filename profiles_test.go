package crdt

import (
	"reflect"
	"testing"
)

func TestReplicationProfilesAreCompleteCanonicalAndDefensive(t *testing.T) {
	profiles := ReplicationProfiles()
	registrations := RegisteredFrameTypes()
	if len(profiles) != len(registrations) {
		t.Fatalf("profile count = %d, want registration count %d", len(profiles), len(registrations))
	}

	seenIDs := make(map[string]bool, len(profiles))
	seenFrames := make(map[FrameType]bool, len(profiles))
	for _, profile := range profiles {
		if profile.ID == "" || profile.Title == "" || profile.Summary == "" || profile.ConflictRule == "" ||
			len(profile.RecommendedFor) == 0 || len(profile.NotFor) == 0 || len(profile.HostRequirements) == 0 {
			t.Fatalf("incomplete profile: %#v", profile)
		}
		if seenIDs[profile.ID] {
			t.Fatalf("duplicate profile ID %q", profile.ID)
		}
		seenIDs[profile.ID] = true
		if seenFrames[profile.FrameType] {
			t.Fatalf("duplicate frame profile %#v", profile.FrameType)
		}
		seenFrames[profile.FrameType] = true

		registered, ok := FrameTypeForState(profile.FrameType.StateID)
		if !ok || registered != profile.FrameType {
			t.Fatalf("profile %q frame type = %#v, registered = %#v, %v", profile.ID, profile.FrameType, registered, ok)
		}
		lookedUp, ok := ReplicationProfileFor(profile.ID)
		if !ok || !reflect.DeepEqual(lookedUp, profile) {
			t.Fatalf("ReplicationProfileFor(%q) = %#v, %v", profile.ID, lookedUp, ok)
		}
	}

	profiles[0].RecommendedFor[0] = "mutated"
	profiles[0].NotFor[0] = "mutated"
	profiles[0].HostRequirements[0] = "mutated"
	if fresh, ok := ReplicationProfileFor("counter/grow-only"); !ok || fresh.RecommendedFor[0] == "mutated" || fresh.NotFor[0] == "mutated" || fresh.HostRequirements[0] == "mutated" {
		t.Fatalf("profiles exposed mutable metadata: %#v, %v", fresh, ok)
	}
}

func TestReplicationProfileForRequiresExactKnownID(t *testing.T) {
	for _, id := range []string{"", "COUNTER/GROW-ONLY", " counter/grow-only", "counter/grow-only "} {
		if profile, ok := ReplicationProfileFor(id); ok || profile.ID != "" || profile.Title != "" || profile.FrameType != (FrameType{}) {
			t.Fatalf("ReplicationProfileFor(%q) = %#v, %v", id, profile, ok)
		}
	}
}

func BenchmarkReplicationProfileFor(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		profile, ok := ReplicationProfileFor("text/run-v2")
		if !ok || profile.FrameType != DefaultRGAFrameType() {
			b.Fatal("missing run-v2 profile")
		}
	}
}
