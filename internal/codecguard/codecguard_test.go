package codecguard

import (
	"errors"
	"testing"
)

func TestCallsReturnValues(t *testing.T) {
	id, err := ID(func() string { return "example.com/value/v1" })
	if err != nil || id != "example.com/value/v1" {
		t.Fatalf("ID() = %q, %v", id, err)
	}
	encoded, err := Marshal(func() ([]byte, error) { return []byte("value"), nil })
	if err != nil || string(encoded) != "value" {
		t.Fatalf("Marshal() = %q, %v", encoded, err)
	}
	decoded, err := Unmarshal(func() (string, error) { return "value", nil })
	if err != nil || decoded != "value" {
		t.Fatalf("Unmarshal() = %q, %v", decoded, err)
	}
}

func TestCallsRecoverPanics(t *testing.T) {
	if _, err := ID(func() string { panic("ID") }); !errors.Is(err, ErrPanic) {
		t.Fatalf("ID() error = %v, want %v", err, ErrPanic)
	}
	if _, err := Marshal(func() ([]byte, error) { panic("Marshal") }); !errors.Is(err, ErrPanic) {
		t.Fatalf("Marshal() error = %v, want %v", err, ErrPanic)
	}
	if _, err := Unmarshal(func() (string, error) { panic("Unmarshal") }); !errors.Is(err, ErrPanic) {
		t.Fatalf("Unmarshal() error = %v, want %v", err, ErrPanic)
	}
}
