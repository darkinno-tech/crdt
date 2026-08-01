package text

import (
	"strings"
	"testing"

	frame "github.com/DarkInno/crdt/encoding"
)

// BenchmarkRGADeltaWireProtocols compares the complete bounded encode/decode
// path used by the diagnostic probe for one linear editor paste. It is a local
// codec measurement, not a network, storage, or production-latency claim.
func BenchmarkRGADeltaWireProtocols(b *testing.B) {
	const runes = 4_096
	source, err := New("writer")
	if err != nil {
		b.Fatal(err)
	}
	delta, err := source.Insert(0, strings.Repeat("λ", runes))
	if err != nil {
		b.Fatal(err)
	}
	limits := frame.DefaultLimits()
	for _, protocol := range []struct {
		name      string
		marshal   func(Delta) ([]byte, error)
		unmarshal func([]byte) (Delta, error)
	}{
		{
			name: "v1",
			marshal: func(value Delta) ([]byte, error) {
				return value.MarshalBinaryWithLimits(limits)
			},
			unmarshal: func(data []byte) (Delta, error) {
				return UnmarshalRGADeltaWithLimits(data, limits)
			},
		},
		{
			name: "run-v2",
			marshal: func(value Delta) ([]byte, error) {
				return value.MarshalRunBinaryWithLimits(limits)
			},
			unmarshal: func(data []byte) (Delta, error) {
				return UnmarshalRGARunDeltaWithLimits(data, limits)
			},
		},
		{
			name: "run-v2-outer-v2-convert",
			marshal: func(value Delta) ([]byte, error) {
				encoded, err := value.MarshalRunBinaryWithLimits(limits)
				if err != nil {
					return nil, err
				}
				return frame.ConvertFrameV1ToV2(encoded, limits)
			},
			unmarshal: func(data []byte) (Delta, error) {
				return UnmarshalRGARunDeltaWithLimits(data, limits)
			},
		},
		{
			name: "run-v2-outer-v2-direct",
			marshal: func(value Delta) ([]byte, error) {
				return value.MarshalRunFrameV2WithLimits(limits)
			},
			unmarshal: func(data []byte) (Delta, error) {
				return UnmarshalRGARunDeltaWithLimits(data, limits)
			},
		},
	} {
		b.Run(protocol.name, func(b *testing.B) {
			encoded, err := protocol.marshal(delta)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				data, err := protocol.marshal(delta)
				if err != nil {
					b.Fatal(err)
				}
				decoded, err := protocol.unmarshal(data)
				if err != nil {
					b.Fatal(err)
				}
				if len(decoded.nodes) != runes {
					b.Fatalf("decoded nodes = %d, want %d", len(decoded.nodes), runes)
				}
			}
		})
	}
}
