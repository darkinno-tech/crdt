package persistence

import (
	"testing"

	configuration "github.com/darkinno-tech/crdt/config"
)

func FuzzConfigFromLoader(f *testing.F) {
	f.Add("8192", "4096", "32", "current-and-previous", "false")
	f.Add("0", "999999", "not-a-number", "unknown", "sometimes")
	f.Fuzz(func(t *testing.T, record, state, frontier, compatibility, migrate string) {
		loader, err := configuration.New(configuration.NewMap(map[string]string{
			KeyMaxRecordBytes:      record,
			KeyMaxStateBytes:       state,
			KeyMaxFrontierEntries:  frontier,
			KeyMaxReplicaIDBytes:   "128",
			KeyMaxOutboxBytes:      "256",
			KeyMaxNameBytes:        "64",
			KeyMaxStoreBytes:       "32768",
			KeyFormatCompatibility: compatibility,
			KeyMigrateOnLoad:       migrate,
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ConfigFrom(loader, validateTestORSet)
		_, _ = FileConfigFrom(loader, validateTestORSet)
	})
}
