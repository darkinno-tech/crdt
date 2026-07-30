package awareness

import "testing"

func FuzzUnmarshalUpdate(f *testing.F) {
	seed, err := (Update{Actor: "seed", Clock: 1, State: []byte(`{"cursor":1}`)}).MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{protocolVersion, 0, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = UnmarshalUpdate(data, DefaultOptions())
	})
}
