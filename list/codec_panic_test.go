package list

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
	return "example.com/list-panic-codec/v1"
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

func TestElementCodecPanicsReturnInvalidCodec(t *testing.T) {
	if _, err := New("id", panicBoundaryCodec{panicID: true}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("New() error = %v, want %v", err, ErrInvalidCodec)
	}
	writer, err := New("writer", panicBoundaryCodec{panicMarshal: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Insert(0, []string{"item"}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("Insert() error = %v, want %v", err, ErrInvalidCodec)
	}

	source, err := New("source", panicBoundaryCodec{})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, []string{"item"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := New("reader", panicBoundaryCodec{panicUnmarshal: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ApplyDelta(delta); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("ApplyDelta() error = %v, want %v", err, ErrInvalidCodec)
	}
}
