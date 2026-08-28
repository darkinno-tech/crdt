package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt"
)

func TestLoaderUsesExplicitSourceBeforeEnvironment(t *testing.T) {
	t.Setenv("CRDT_LIMIT", "32")
	environment, err := NewEnvironment("CRDT_")
	if err != nil {
		t.Fatal(err)
	}
	loader, err := New(NewMap(map[string]string{"LIMIT": "16"}), environment)
	if err != nil {
		t.Fatal(err)
	}
	limit, err := loader.Int("LIMIT", 8, 1, 64)
	if err != nil || limit != 16 {
		t.Fatalf("Int() = %d, %v; want 16, nil", limit, err)
	}

	loader, err = New(environment)
	if err != nil {
		t.Fatal(err)
	}
	limit, err = loader.Int("LIMIT", 8, 1, 64)
	if err != nil || limit != 32 {
		t.Fatalf("environment Int() = %d, %v; want 32, nil", limit, err)
	}
}

func TestLoaderParsesAndValidatesWithoutLeakingValues(t *testing.T) {
	loader, err := New(NewMap(map[string]string{
		"ENABLED": "true",
		"TIMEOUT": "150ms",
		"SECRET":  "not-a-number",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if enabled, err := loader.Bool("ENABLED", false); err != nil || !enabled {
		t.Fatalf("Bool() = %v, %v; want true, nil", enabled, err)
	}
	if timeout, err := loader.Duration("TIMEOUT", time.Second); err != nil || timeout != 150*time.Millisecond {
		t.Fatalf("Duration() = %v, %v; want 150ms, nil", timeout, err)
	}
	_, err = loader.Int("SECRET", 1, 1, 16)
	if !errors.Is(err, ErrInvalidValue) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("Int invalid error = %v", err)
	}
	if got := err.Error(); got == "" || strings.Contains(got, "not-a-number") {
		t.Fatalf("invalid value leaked into error %q", got)
	}
}

func TestLoaderRejectsInvalidKeysAndRequiredValues(t *testing.T) {
	loader, err := New(NewMap(map[string]string{"BLANK": "  "}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.RequiredString("BLANK"); !errors.Is(err, ErrRequired) {
		t.Fatalf("RequiredString() error = %v", err)
	}
	if _, err := loader.String("bad-key", "fallback"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key error = %v", err)
	}
	if _, err := NewEnvironment("bad-"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid prefix error = %v", err)
	}
}

func TestNewMapCopiesInput(t *testing.T) {
	values := map[string]string{"MODE": "safe"}
	loader, err := New(NewMap(values))
	if err != nil {
		t.Fatal(err)
	}
	values["MODE"] = "unsafe"
	value, err := loader.String("MODE", "")
	if err != nil || value != "safe" {
		t.Fatalf("String() = %q, %v; want immutable copy", value, err)
	}
}

func TestLoaderCoversAbsentAndUnsafeTypedSettings(t *testing.T) {
	if _, err := New(); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("New() error = %v", err)
	}
	var missing Source
	if _, err := New(missing); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("New(nil source) error = %v", err)
	}
	loader, err := New(NewMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.RequiredString("MISSING"); !errors.Is(err, ErrRequired) {
		t.Fatalf("missing required error = %v", err)
	}
	if value, err := loader.Bool("MISSING", true); err != nil || !value {
		t.Fatalf("missing Bool() = %v, %v", value, err)
	}
	if value, err := loader.Int("MISSING", 4, 1, 8); err != nil || value != 4 {
		t.Fatalf("missing Int() = %d, %v", value, err)
	}
	if value, err := loader.Duration("MISSING", time.Second); err != nil || value != time.Second {
		t.Fatalf("missing Duration() = %v, %v", value, err)
	}
	if _, err := loader.Int("MISSING", 0, 1, 8); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsafe int fallback error = %v", err)
	}
	if _, err := loader.Int("MISSING", 4, 8, 1); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("inverted int range error = %v", err)
	}
	if _, err := loader.Duration("MISSING", 0); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsafe duration fallback error = %v", err)
	}

	unsafe, err := New(NewMap(map[string]string{"BOOL": "not-bool", "INT": "0", "DURATION": "0s"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unsafe.Bool("BOOL", false); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid Bool error = %v", err)
	}
	if _, err := unsafe.Int("INT", 1, 1, 2); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid Int error = %v", err)
	}
	if _, err := unsafe.Duration("DURATION", time.Second); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid Duration error = %v", err)
	}
	for _, key := range []string{"_LEADING", "TRAILING_", "1DIGIT", "UNICODE_Ä"} {
		if _, err := loader.String(key, ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("invalid key %q error = %v", key, err)
		}
	}
}

func TestLoaderRequiredIntAndEnum(t *testing.T) {
	loader, err := New(NewMap(map[string]string{
		"LIMIT": "16",
		"MODE":  "safe",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if value, err := loader.RequiredInt("LIMIT", 1, 32); err != nil || value != 16 {
		t.Fatalf("RequiredInt() = %d, %v", value, err)
	}
	if value, err := loader.Enum("MODE", "safe", "safe", "strict"); err != nil || value != "safe" {
		t.Fatalf("Enum() = %q, %v", value, err)
	}
	if _, err := loader.RequiredInt("MISSING", 1, 32); !errors.Is(err, ErrRequired) {
		t.Fatalf("missing RequiredInt() error = %v", err)
	}
	if _, err := loader.RequiredInt("LIMIT", 32, 1); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid RequiredInt range error = %v", err)
	}
	if _, err := loader.Enum("MODE", "unsafe", "safe", "strict"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsafe enum fallback error = %v", err)
	}
	unsafe, err := New(NewMap(map[string]string{"MODE": "unsafe", "LIMIT": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unsafe.Enum("MODE", "safe", "safe", "strict"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid enum error = %v", err)
	}
	if _, err := unsafe.RequiredInt("LIMIT", 1, 32); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid required int error = %v", err)
	}
}
