//go:build unix

package artifactfs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkBlob(b *testing.B) {
	const size = 64 << 10
	for _, operation := range []string{"PublishFresh64KiB", "PublishDuplicate64KiB", "ReadVerified64KiB"} {
		b.Run(operation, func(b *testing.B) {
			s, src, dir := fixture(b, size)
			data := strings.Repeat("x", size)
			must(b, src.WriteFile("input", []byte(data), 0600))
			blob, err := s.Publish(src, "input")
			must(b, err)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch operation {
				case "PublishFresh64KiB":
					b.StopTimer() // Exclude fixture removal; every timed publish is new.
					must(b, os.Remove(filepath.Join(dir, blob.Hash)))
					b.StartTimer()
					_, err = s.Publish(src, "input")
				case "PublishDuplicate64KiB":
					_, err = s.Publish(src, "input")
				case "ReadVerified64KiB":
					var reader io.ReadCloser
					reader, err = s.Open(blob.Hash)
					must(b, err)
					_, err = io.Copy(io.Discard, reader)
					must(b, reader.Close())
				}
				must(b, err)
			}
		})
	}
}
