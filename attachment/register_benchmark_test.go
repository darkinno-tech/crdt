package attachment

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

func BenchmarkRegisterApplyMetadataDelta(b *testing.B) {
	source, err := New("source")
	if err != nil {
		b.Fatal(err)
	}
	change, err := source.Put("video", testReference("benchmark-video", "video/mp4", 64<<20))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(32) // The replicated content hash, not the 64 MiB media object.
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := New("target")
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRegisterMarshalThousandReferences(b *testing.B) {
	value, err := New("writer")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 1_000; index++ {
		if _, err := value.Put(fmt.Sprintf("asset-%04d", index), testReference(fmt.Sprintf("asset-%d", index), "image/webp", 128<<10)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReferenceVerifyOneMiB(b *testing.B) {
	content := bytes.Repeat([]byte("m"), 1<<20)
	ref := Reference{
		ObjectID:  "object-benchmark-verify",
		MediaType: "video/mp4",
		Size:      uint64(len(content)),
		Digest:    sha256.Sum256(content),
	}
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := ref.Verify(bytes.NewReader(content)); err != nil {
			b.Fatal(err)
		}
	}
}
