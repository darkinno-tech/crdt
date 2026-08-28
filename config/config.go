// Package config provides explicit, layered configuration lookup for host
// applications. It intentionally does not install process-wide settings or
// make library constructors read environment variables implicitly.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/darkinno-tech/crdt"
)

var (
	// ErrInvalidSource reports a nil or unusable configuration source.
	ErrInvalidSource = errors.New("crdt config: invalid source")
	// ErrInvalidKey reports a key outside the portable upper-snake-case form.
	ErrInvalidKey = errors.New("crdt config: invalid key")
	// ErrRequired reports an absent or empty required setting.
	ErrRequired = errors.New("crdt config: required value is missing")
	// ErrInvalidValue reports an invalid or out-of-range setting. Error text
	// deliberately contains its key but never the setting value.
	ErrInvalidValue = errors.New("crdt config: invalid value")
)

// Source looks up one named setting. A Source must be safe for concurrent use.
// Higher-priority sources appear first when constructing a Loader.
type Source interface {
	Lookup(key string) (string, bool)
}

// Map is an immutable copy of one caller-owned settings map.
type Map struct {
	values map[string]string
}

// NewMap copies values so later mutations by the caller cannot change a
// Loader's effective configuration.
func NewMap(values map[string]string) Map {
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return Map{values: copy}
}

// Lookup implements Source.
func (source Map) Lookup(key string) (string, bool) {
	value, ok := source.values[key]
	return value, ok
}

// Environment is an environment-variable Source. Prefix is prepended to
// every lookup key, allowing one process to host multiple independent apps.
type Environment struct {
	prefix string
}

// NewEnvironment creates a portable environment source. Prefix may be empty
// or use the same upper-snake-case form as a key, including a trailing
// underscore (for example, "CRDT_").
func NewEnvironment(prefix string) (Environment, error) {
	if prefix != "" && !validPrefix(prefix) {
		return Environment{}, invalid(crdt.ErrorCodeInvalidConfig, "config.new_environment", ErrInvalidKey)
	}
	return Environment{prefix: prefix}, nil
}

// Lookup implements Source.
func (source Environment) Lookup(key string) (string, bool) {
	return os.LookupEnv(source.prefix + key)
}

// Loader resolves settings from an ordered, immutable source list. It is safe
// for concurrent lookups when its Sources are safe for concurrent use.
type Loader struct {
	sources []Source
}

// New creates a Loader. The first source that contains a key wins, so a
// typical application passes explicit deployment values before Environment
// and compiled defaults.
func New(sources ...Source) (Loader, error) {
	if len(sources) == 0 {
		return Loader{}, invalid(crdt.ErrorCodeInvalidConfig, "config.new", ErrInvalidSource)
	}
	result := Loader{sources: make([]Source, len(sources))}
	for index, source := range sources {
		if source == nil {
			return Loader{}, invalid(crdt.ErrorCodeInvalidConfig, "config.new", ErrInvalidSource)
		}
		result.sources[index] = source
	}
	return result, nil
}

// String returns key or fallback when no source defines it.
func (loader Loader) String(key, fallback string) (string, error) {
	value, ok, err := loader.lookup(key)
	if err != nil || !ok {
		return fallback, err
	}
	return value, nil
}

// RequiredString returns a non-empty, whitespace-trimmed setting.
func (loader Loader) RequiredString(key string) (string, error) {
	value, ok, err := loader.lookup(key)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return "", invalid(crdt.ErrorCodeInvalidConfig, "config.required_string."+key, ErrRequired)
	}
	return value, nil
}

// Bool parses a strictly formatted Boolean setting or returns fallback when
// the setting is absent. Accepted values are those accepted by strconv.ParseBool.
func (loader Loader) Bool(key string, fallback bool) (bool, error) {
	value, ok, err := loader.lookup(key)
	if err != nil || !ok {
		return fallback, err
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, invalid(crdt.ErrorCodeInvalidConfig, "config.bool."+key, ErrInvalidValue)
	}
	return parsed, nil
}

// Int parses a setting in the inclusive range [min, max], or returns fallback
// when absent. Supplying min greater than max is a programmer configuration
// error and is rejected even when key is absent.
func (loader Loader) Int(key string, fallback, min, max int) (int, error) {
	if min > max || fallback < min || fallback > max {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.int."+key, ErrInvalidValue)
	}
	value, ok, err := loader.lookup(key)
	if err != nil || !ok {
		return fallback, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min || parsed > max {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.int."+key, ErrInvalidValue)
	}
	return parsed, nil
}

// Duration parses a positive duration setting or returns fallback when absent.
// The fallback must also be positive so zero can never silently disable a
// timeout in host transport configuration.
func (loader Loader) Duration(key string, fallback time.Duration) (time.Duration, error) {
	if fallback <= 0 {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.duration."+key, ErrInvalidValue)
	}
	value, ok, err := loader.lookup(key)
	if err != nil || !ok {
		return fallback, err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.duration."+key, ErrInvalidValue)
	}
	return parsed, nil
}

func (loader Loader) lookup(key string) (string, bool, error) {
	if !validKey(key) {
		return "", false, invalid(crdt.ErrorCodeInvalidConfig, "config.lookup", ErrInvalidKey)
	}
	for _, source := range loader.sources {
		if value, ok := source.Lookup(key); ok {
			return value, true, nil
		}
	}
	return "", false, nil
}

func validKey(key string) bool {
	if key == "" || key[len(key)-1] == '_' || key[0] < 'A' || key[0] > 'Z' {
		return false
	}
	for index := 1; index < len(key); index++ {
		character := key[index]
		if character != '_' && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validPrefix(prefix string) bool {
	if !strings.HasSuffix(prefix, "_") || prefix[0] == '_' {
		return false
	}
	return validKey(strings.TrimSuffix(prefix, "_"))
}

func invalid(code crdt.ErrorCode, operation string, cause error) error {
	return crdt.WrapError(code, operation, cause)
}
