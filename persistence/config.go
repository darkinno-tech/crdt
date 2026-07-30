package persistence

import (
	"time"

	configuration "github.com/DarkInno/crdt/config"
	"github.com/DarkInno/crdt/snapshot"
)

const (
	// KeyMaxRecordBytes is the required maximum encoded checkpoint record size.
	KeyMaxRecordBytes = "PERSISTENCE_MAX_RECORD_BYTES"
	// KeyMaxStateBytes is the required maximum canonical CRDT state size.
	KeyMaxStateBytes = "PERSISTENCE_MAX_STATE_BYTES"
	// KeyMaxFrontierEntries is the required maximum snapshot frontier size.
	KeyMaxFrontierEntries = "PERSISTENCE_MAX_FRONTIER_ENTRIES"
	// KeyMaxReplicaIDBytes is the required maximum replica identifier size.
	KeyMaxReplicaIDBytes = "PERSISTENCE_MAX_REPLICA_ID_BYTES"
	// KeyMaxOutboxBytes is the required maximum opaque outbox size.
	KeyMaxOutboxBytes = "PERSISTENCE_MAX_OUTBOX_BYTES"
	// KeyMaxNameBytes is the required maximum checkpoint name size.
	KeyMaxNameBytes = "PERSISTENCE_MAX_NAME_BYTES"
	// KeyOpenTimeout is an optional positive bbolt lock timeout.
	KeyOpenTimeout = "PERSISTENCE_OPEN_TIMEOUT"
	// KeyFormatVersion is an optional local checkpoint record format version.
	KeyFormatVersion = "PERSISTENCE_FORMAT_VERSION"
	// KeyFormatCompatibility is an optional local record compatibility policy.
	KeyFormatCompatibility = "PERSISTENCE_FORMAT_COMPATIBILITY"
	// KeyMigrateOnLoad enables optional transactional migration of legacy local
	// records. Migration functions themselves remain code-only.
	KeyMigrateOnLoad = "PERSISTENCE_MIGRATE_ON_LOAD"
	// KeyMaxStoreBytes is the required maximum complete FileStore size.
	KeyMaxStoreBytes = "PERSISTENCE_MAX_STORE_BYTES"

	formatCompatibilityCurrent            = "current"
	formatCompatibilityCurrentAndPrevious = "current-and-previous"
)

// ConfigFrom resolves one normalized persistence Config from an explicit
// configuration Loader. Capacity limits remain required because their safe
// values depend on the application's documents and retry policy. validator
// and migrations are code-owned contracts, not untrusted configuration data.
func ConfigFrom(loader configuration.Loader, validator snapshot.StateValidator, migrations ...Migration) (Config, error) {
	if validator == nil {
		return Config{}, ErrInvalidConfig
	}
	maximum := maxInt()
	maxRecordBytes, err := loader.RequiredInt(KeyMaxRecordBytes, 1, maximum)
	if err != nil {
		return Config{}, err
	}
	maxStateBytes, err := loader.RequiredInt(KeyMaxStateBytes, 1, maxRecordBytes)
	if err != nil {
		return Config{}, err
	}
	maxFrontierEntries, err := loader.RequiredInt(KeyMaxFrontierEntries, 1, maximum)
	if err != nil {
		return Config{}, err
	}
	maxReplicaIDBytes, err := loader.RequiredInt(KeyMaxReplicaIDBytes, 1, maximum)
	if err != nil {
		return Config{}, err
	}
	maxOutboxBytes, err := loader.RequiredInt(KeyMaxOutboxBytes, 0, maximum)
	if err != nil {
		return Config{}, err
	}
	maxNameBytes, err := loader.RequiredInt(KeyMaxNameBytes, 1, maximum)
	if err != nil {
		return Config{}, err
	}
	openTimeout, err := loader.Duration(KeyOpenTimeout, 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	formatVersion, err := loader.Int(KeyFormatVersion, int(CurrentRecordFormat), int(RecordFormatV1), int(RecordFormatV2))
	if err != nil {
		return Config{}, err
	}
	var recordVersion byte
	switch formatVersion {
	case int(RecordFormatV1):
		recordVersion = RecordFormatV1
	case int(RecordFormatV2):
		recordVersion = RecordFormatV2
	default:
		return Config{}, ErrInvalidConfig
	}
	compatibilityValue, err := loader.Enum(
		KeyFormatCompatibility,
		formatCompatibilityCurrentAndPrevious,
		formatCompatibilityCurrent,
		formatCompatibilityCurrentAndPrevious,
	)
	if err != nil {
		return Config{}, err
	}
	migrateOnLoad, err := loader.Bool(KeyMigrateOnLoad, false)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Format: FormatConfig{
			Version:       recordVersion,
			Compatibility: compatibilityFromValue(compatibilityValue),
			MigrateOnLoad: migrateOnLoad,
			Migrations:    append([]Migration(nil), migrations...),
		},
		MaxRecordBytes:     maxRecordBytes,
		MaxStateBytes:      maxStateBytes,
		MaxFrontierEntries: maxFrontierEntries,
		MaxReplicaIDBytes:  maxReplicaIDBytes,
		MaxOutboxBytes:     maxOutboxBytes,
		MaxNameBytes:       maxNameBytes,
		OpenTimeout:        openTimeout,
		Validate:           validator,
	}
	return config.normalized()
}

// FileConfigFrom resolves a FileStore configuration from an explicit Loader.
// It retains the same required record bounds as ConfigFrom and adds the
// required complete-file budget.
func FileConfigFrom(loader configuration.Loader, validator snapshot.StateValidator, migrations ...Migration) (FileConfig, error) {
	config, err := ConfigFrom(loader, validator, migrations...)
	if err != nil {
		return FileConfig{}, err
	}
	maxStoreBytes, err := loader.RequiredInt(KeyMaxStoreBytes, 1, maxInt())
	if err != nil {
		return FileConfig{}, err
	}
	result := FileConfig{Config: config, MaxStoreBytes: maxStoreBytes}
	return result.normalized()
}

func compatibilityFromValue(value string) Compatibility {
	if value == formatCompatibilityCurrent {
		return CompatibilityCurrentOnly
	}
	return CompatibilityCurrentAndPrevious
}

func maxInt() int { return int(^uint(0) >> 1) }
