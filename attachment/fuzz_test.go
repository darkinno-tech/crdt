package attachment

import "testing"

func FuzzUnmarshalDelta(f *testing.F) {
	source, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	change, err := source.Put("image", testReference("seed", "image/png", 1))
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := change.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("not a frame"))
	f.Fuzz(func(t *testing.T, data []byte) {
		change, err := UnmarshalDelta(data)
		if err != nil {
			return
		}
		target := mustRegister(t, "target")
		if err := target.ApplyDelta(change); err != nil {
			t.Fatalf("accepted delta did not apply: %v", err)
		}
	})
}
