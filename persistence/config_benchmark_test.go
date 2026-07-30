package persistence

import (
	"testing"

	configuration "github.com/DarkInno/crdt/config"
)

func BenchmarkConfigFromLoader(b *testing.B) {
	loader, err := configuration.New(configuration.NewMap(map[string]string{
		KeyMaxRecordBytes:      "8192",
		KeyMaxStateBytes:       "4096",
		KeyMaxFrontierEntries:  "32",
		KeyMaxReplicaIDBytes:   "128",
		KeyMaxOutboxBytes:      "256",
		KeyMaxNameBytes:        "64",
		KeyMaxStoreBytes:       "32768",
		KeyFormatCompatibility: formatCompatibilityCurrentAndPrevious,
	}))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := ConfigFrom(loader, validateTestORSet); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileConfigFromLoader(b *testing.B) {
	loader, err := configuration.New(configuration.NewMap(map[string]string{
		KeyMaxRecordBytes:      "8192",
		KeyMaxStateBytes:       "4096",
		KeyMaxFrontierEntries:  "32",
		KeyMaxReplicaIDBytes:   "128",
		KeyMaxOutboxBytes:      "256",
		KeyMaxNameBytes:        "64",
		KeyMaxStoreBytes:       "32768",
		KeyFormatCompatibility: formatCompatibilityCurrentAndPrevious,
	}))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := FileConfigFrom(loader, validateTestORSet); err != nil {
			b.Fatal(err)
		}
	}
}
