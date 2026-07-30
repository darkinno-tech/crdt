package lww

import (
	"errors"
	"testing"
)

type panicBoundaryCodec struct {
	panicID        bool
	panicMarshal   bool
	panicUnmarshal bool
}

func (c panicBoundaryCodec) ID() string {
	if c.panicID {
		panic("ID")
	}
	return "example.com/lww-panic-codec/v1"
}

func (c panicBoundaryCodec) Marshal(value string) ([]byte, error) {
	if c.panicMarshal {
		panic("Marshal")
	}
	return []byte(value), nil
}

func (c panicBoundaryCodec) Unmarshal(data []byte) (string, error) {
	if c.panicUnmarshal {
		panic("Unmarshal")
	}
	return string(data), nil
}

func TestSetCodecPanicsReturnInvalidCodec(t *testing.T) {
	value, err := NewSet[string]("writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.AddWithDelta("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.MarshalBinary(panicBoundaryCodec{panicID: true}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("MarshalBinary ID panic error = %v, want %v", err, ErrInvalidCodec)
	}
	if _, err := value.MarshalBinary(panicBoundaryCodec{panicMarshal: true}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("MarshalBinary Marshal panic error = %v, want %v", err, ErrInvalidCodec)
	}

	encoded, err := value.MarshalBinary(panicBoundaryCodec{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewSet[string]("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(encoded, panicBoundaryCodec{panicUnmarshal: true}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("UnmarshalBinary() error = %v, want %v", err, ErrInvalidCodec)
	}
}
