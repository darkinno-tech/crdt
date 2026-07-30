package text

import "testing"

// BenchmarkRGARunObfuscatedState measures the complete debug-export path on a
// large linear document. It is a controlled local codec measurement, not a
// claim about support-upload or network latency.
func BenchmarkRGARunObfuscatedState(b *testing.B) {
	value := benchmarkRGALinearDocument(b, benchmarkRGARunDocumentRunes)
	for _, export := range []struct {
		name    string
		marshal func() ([]byte, error)
	}{
		{name: "normal", marshal: value.MarshalRunBinary},
		{name: "obfuscated", marshal: value.MarshalObfuscatedRunBinary},
	} {
		b.Run(export.name, func(b *testing.B) {
			encoded, err := export.marshal()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := export.marshal(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
