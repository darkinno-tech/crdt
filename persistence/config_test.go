package persistence

import (
	"errors"
	"testing"
	"time"

	configuration "github.com/DarkInno/crdt/config"
)

func TestConfigFromLoaderBuildsNormalizedConfiguration(t *testing.T) {
	loader := persistenceLoader(t, map[string]string{
		KeyMaxRecordBytes:      "8192",
		KeyMaxStateBytes:       "4096",
		KeyMaxFrontierEntries:  "32",
		KeyMaxReplicaIDBytes:   "128",
		KeyMaxOutboxBytes:      "256",
		KeyMaxNameBytes:        "64",
		KeyOpenTimeout:         "250ms",
		KeyFormatVersion:       "2",
		KeyFormatCompatibility: formatCompatibilityCurrentAndPrevious,
		KeyMigrateOnLoad:       "true",
		KeyMaxStoreBytes:       "32768",
	})
	migrations := []Migration{{FromVersion: RecordFormatV1}}
	config, err := ConfigFrom(loader, validateTestORSet, migrations...)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxRecordBytes != 8192 || config.MaxStateBytes != 4096 || config.OpenTimeout != 250*time.Millisecond ||
		config.Format.Version != RecordFormatV2 || config.Format.Compatibility != CompatibilityCurrentAndPrevious || !config.Format.MigrateOnLoad {
		t.Fatalf("ConfigFrom() = %+v", config)
	}
	migrations[0].FromVersion = RecordFormatV2
	if migration, ok := config.Format.migration(RecordFormatV1); !ok || migration.FromVersion != RecordFormatV1 {
		t.Fatalf("migrations were not copied: %+v", config.Format.Migrations)
	}
	file, err := FileConfigFrom(loader, validateTestORSet)
	if err != nil {
		t.Fatal(err)
	}
	if file.MaxStoreBytes != 32768 || file.MaxRecordBytes != 8192 {
		t.Fatalf("FileConfigFrom() = %+v", file)
	}
}

func TestConfigFromLoaderRejectsUnsafeOrIncompleteSettings(t *testing.T) {
	complete := map[string]string{
		KeyMaxRecordBytes:     "8192",
		KeyMaxStateBytes:      "4096",
		KeyMaxFrontierEntries: "32",
		KeyMaxReplicaIDBytes:  "128",
		KeyMaxOutboxBytes:     "256",
		KeyMaxNameBytes:       "64",
		KeyMaxStoreBytes:      "32768",
	}
	if _, err := ConfigFrom(persistenceLoader(t, complete), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil validator error = %v", err)
	}
	for _, setting := range []struct {
		key   string
		value string
	}{
		{KeyMaxRecordBytes, "0"},
		{KeyMaxStateBytes, "8193"},
		{KeyMaxFrontierEntries, "0"},
		{KeyMaxReplicaIDBytes, "0"},
		{KeyMaxOutboxBytes, "-1"},
		{KeyOpenTimeout, "0s"},
		{KeyFormatVersion, "3"},
		{KeyFormatCompatibility, "unknown"},
		{KeyMigrateOnLoad, "sometimes"},
	} {
		settings := cloneSettings(complete)
		settings[setting.key] = setting.value
		if _, err := ConfigFrom(persistenceLoader(t, settings), validateTestORSet); err == nil {
			t.Fatalf("ConfigFrom(%s=%q) accepted unsafe setting", setting.key, setting.value)
		}
	}
	missing := cloneSettings(complete)
	delete(missing, KeyMaxNameBytes)
	if _, err := ConfigFrom(persistenceLoader(t, missing), validateTestORSet); err == nil {
		t.Fatal("ConfigFrom() accepted missing required limit")
	}
	v1 := cloneSettings(complete)
	v1[KeyFormatVersion] = "1"
	if _, err := ConfigFrom(persistenceLoader(t, v1), validateTestORSet); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("v1 with default compatibility error = %v, want %v", err, ErrInvalidConfig)
	}
	if _, err := FileConfigFrom(persistenceLoader(t, missing), validateTestORSet); err == nil {
		t.Fatal("FileConfigFrom() accepted missing required setting")
	}
	missing = cloneSettings(complete)
	delete(missing, KeyMaxStoreBytes)
	if _, err := FileConfigFrom(persistenceLoader(t, missing), validateTestORSet); err == nil {
		t.Fatal("FileConfigFrom() accepted missing total-file limit")
	}
	current := cloneSettings(complete)
	current[KeyFormatCompatibility] = formatCompatibilityCurrent
	config, err := ConfigFrom(persistenceLoader(t, current), validateTestORSet)
	if err != nil || config.Format.Compatibility != CompatibilityCurrentOnly {
		t.Fatalf("current-only compatibility config=%+v err=%v", config, err)
	}
}

func persistenceLoader(t *testing.T, settings map[string]string) configuration.Loader {
	t.Helper()
	loader, err := configuration.New(configuration.NewMap(settings))
	if err != nil {
		t.Fatal(err)
	}
	return loader
}

func cloneSettings(settings map[string]string) map[string]string {
	cloned := make(map[string]string, len(settings))
	for key, value := range settings {
		cloned[key] = value
	}
	return cloned
}
