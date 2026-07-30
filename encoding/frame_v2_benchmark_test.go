package encoding

import (
	"strings"
	"testing"
)

// BenchmarkFrameUpdateFormats compares the complete bounded transport path for
// a realistic repeated-editor payload. It is a codec measurement, not a claim
// about network or storage latency.
func BenchmarkFrameUpdateFormats(b *testing.B) {
	payload := []byte(strings.Repeat("author=writer; selection=paragraph; text=collaborative update\n", 2_048))
	for _, format := range []struct {
		name    string
		marshal func(Frame) ([]byte, error)
	}{
		{name: "v1", marshal: MarshalFrame},
		{name: "v2", marshal: MarshalFrameV2},
	} {
		b.Run(format.name, func(b *testing.B) {
			encoded, err := format.marshal(Frame{TypeID: 1, CodecID: "example.com/editor/v1", Payload: payload})
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				data, err := format.marshal(Frame{TypeID: 1, CodecID: "example.com/editor/v1", Payload: payload})
				if err != nil {
					b.Fatal(err)
				}
				decoded, err := UnmarshalFrame(data, DefaultLimits())
				if err != nil || len(decoded.Payload) != len(payload) {
					b.Fatalf("decode = %d bytes, %v", len(decoded.Payload), err)
				}
			}
		})
	}
}
