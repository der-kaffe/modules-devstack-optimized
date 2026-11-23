package format

import (
	"bytes"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const benchChunk = 1 << 20

func benchData() []byte { return bytes.Repeat([]byte("glance-go-inspect"), (64<<20)/17) }

// The upload path: 64 MiB of raw data fed in 1 MiB chunks. ASCII data
// also looks like a text VMDK descriptor; binary data is a disk.
func BenchmarkInspectWrapper64MiB(b *testing.B) {
	b.Run("text", func(b *testing.B) { benchInspect(b, benchData()) })
	b.Run("binary", func(b *testing.B) {
		data := make([]byte, 64<<20)
		_, _ = rand.NewChaCha8([32]byte{1}).Read(data)
		benchInspect(b, data)
	})
}

func benchInspect(b *testing.B, data []byte) {
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		w := NewWrapper()
		for off := 0; off < len(data); off += benchChunk {
			_, _ = w.Write(data[off:min(off+benchChunk, len(data))])
		}
		w.Finish()
		if _, err := w.Check("raw", true, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// image_conversion's detection of a 64 MiB qcow2 and raw file.
func BenchmarkDetectFile64MiB(b *testing.B) {
	dir := b.TempDir()
	raw := filepath.Join(dir, "raw")
	if err := os.WriteFile(raw, benchData(), 0o600); err != nil {
		b.Fatal(err)
	}
	files := map[string]string{"raw": raw}
	if _, err := exec.LookPath("qemu-img"); err == nil {
		q := filepath.Join(dir, "qcow2")
		if exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", raw, q).Run() == nil {
			files["qcow2"] = q
		}
	}
	for name, path := range files {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got, err := DetectFile(path, nil)
				if err != nil || got != name {
					b.Fatal(got, err)
				}
			}
		})
	}
}
