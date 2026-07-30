package richtext

import "testing"

func FuzzUnmarshal(f *testing.F) {
	document, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	insert, err := document.InsertWithAttributes(0, "seed", Attributes{"bold": "true"})
	if err != nil {
		f.Fatal(err)
	}
	delta, err := insert.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	state, err := document.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(delta)
	f.Add(state)
	f.Add([]byte("not a CRDT frame"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if decoded, err := UnmarshalDelta(data); err == nil {
			target := mustDocument(t, "target")
			if err := target.ApplyDelta(decoded); err != nil {
				t.Fatalf("decoded delta did not apply: %v", err)
			}
		}
		target := mustDocument(t, "state-target")
		_ = target.UnmarshalBinary(data)
	})
}
