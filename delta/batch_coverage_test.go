package delta

import (
	"errors"
	"testing"
)

func TestCoalescerRejectsInvalidMergedFramesWithoutMutation(t *testing.T) {
	item := mustEncodedDelta(t, "source", 1)
	coalescer, err := NewCoalescer(2, len(item)*2, func(_, _ []byte) ([]byte, error) {
		return []byte("not a frame"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(item); err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(item); err == nil {
		t.Fatal("invalid merge output accepted")
	}
	if items, bytes := coalescer.Len(); items != 1 || bytes != len(item) {
		t.Fatalf("invalid merge changed queue to %d items, %d bytes", items, bytes)
	}

	wrongFrame := mustEncodedORSetDelta(t)
	typed, err := NewCoalescer(2, len(item)*2, func(_, _ []byte) ([]byte, error) {
		return wrongFrame, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := typed.Add(item); err != nil {
		t.Fatal(err)
	}
	if err := typed.Add(item); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("wrong type merge error = %v", err)
	}
}

func TestCoalescerEnforcesByteBudgetBeforeAppending(t *testing.T) {
	item := mustEncodedDelta(t, "source", 1)
	tooSmall, err := NewCoalescer(1, len(item)-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tooSmall.Add(item); !errors.Is(err, ErrLimit) {
		t.Fatalf("single-item byte budget error = %v", err)
	}
	coalescer, err := NewCoalescer(2, len(item)+1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(item); err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(item); !errors.Is(err, ErrLimit) {
		t.Fatalf("byte budget error = %v", err)
	}
	if items, bytes := coalescer.Len(); items != 1 || bytes != len(item) {
		t.Fatalf("failed append changed queue to %d items, %d bytes", items, bytes)
	}
}
