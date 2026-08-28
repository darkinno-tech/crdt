package config

import (
	"strconv"

	"github.com/darkinno-tech/crdt"
)

// RequiredInt parses a required integer in the inclusive range [min, max].
// It rejects an invalid range before reading sources and never includes the
// configured value in an error.
func (loader Loader) RequiredInt(key string, min, max int) (int, error) {
	if min > max {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.required_int."+key, ErrInvalidValue)
	}
	value, err := loader.RequiredString(key)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min || parsed > max {
		return 0, invalid(crdt.ErrorCodeInvalidConfig, "config.required_int."+key, ErrInvalidValue)
	}
	return parsed, nil
}

// Enum returns a configured value only when it is one of values. The fallback
// and accepted values are validated first so an absent setting cannot mask a
// programmer configuration error.
func (loader Loader) Enum(key, fallback string, values ...string) (string, error) {
	if !contains(values, fallback) {
		return "", invalid(crdt.ErrorCodeInvalidConfig, "config.enum."+key, ErrInvalidValue)
	}
	value, ok, err := loader.lookup(key)
	if err != nil || !ok {
		return fallback, err
	}
	if !contains(values, value) {
		return "", invalid(crdt.ErrorCodeInvalidConfig, "config.enum."+key, ErrInvalidValue)
	}
	return value, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
