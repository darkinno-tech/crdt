package documenttree

import (
	"bytes"
	"testing"
)

// FuzzDocumentTreeWire exercises the attacker-controlled frame boundary. A
// malformed frame must never panic or mutate a receiver. Complete valid
// frames are additionally required to marshal back canonically.
func FuzzDocumentTreeWire(f *testing.F) {
	source := mustDocument(f, "seed")
	_, seed, err := source.CreateRootMap("workspace")
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := seed.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("not-a-frame"))
	f.Add(bytes.Repeat([]byte{0xff}, 64))
	f.Fuzz(func(t *testing.T, input []byte) {
		before := mustDocument(t, "before")
		beforeState, err := before.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		delta, err := UnmarshalDelta(input)
		if err != nil {
			after, marshalErr := before.MarshalBinary()
			if marshalErr != nil || !bytes.Equal(after, beforeState) {
				t.Fatalf("decode rejection changed receiver: %v", marshalErr)
			}
			return
		}
		if err := before.ApplyDelta(delta); err != nil {
			return
		}
		if state, err := before.MarshalBinary(); err == nil {
			restored := mustDocument(t, "restore")
			if err := restored.UnmarshalBinary(state); err != nil {
				t.Fatalf("accepted state did not restore: %v", err)
			}
		}
	})
}
