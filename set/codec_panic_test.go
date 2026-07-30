package set

import (
	"errors"
	"testing"
)

type panicCodec struct {
	panicID        bool
	panicMarshal   bool
	panicUnmarshal bool
}

func (c panicCodec) ID() string {
	if c.panicID {
		panic("ID")
	}
	return "example.com/panic-codec/v1"
}

func (c panicCodec) Marshal(value string) ([]byte, error) {
	if c.panicMarshal {
		panic("Marshal")
	}
	return []byte(value), nil
}

func (c panicCodec) Unmarshal(data []byte) (string, error) {
	if c.panicUnmarshal {
		panic("Unmarshal")
	}
	return string(data), nil
}

func TestElementCodecPanicsReturnInvalidCodec(t *testing.T) {
	if _, err := NewGSet("id", panicCodec{panicID: true}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("NewGSet() error = %v, want %v", err, ErrInvalidCodec)
	}

	value, err := NewGSet("writer", panicCodec{panicMarshal: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.MarshalBinary(); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("MarshalBinary() error = %v, want %v", err, ErrInvalidCodec)
	}

	source, err := NewGSet("source", panicCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Add("item"); err != nil {
		t.Fatal(err)
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewGSet("target", panicCodec{panicUnmarshal: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(encoded); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("UnmarshalBinary() error = %v, want %v", err, ErrInvalidCodec)
	}
}
