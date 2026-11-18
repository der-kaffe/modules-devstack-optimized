package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/google/uuid"
)

// Before/after microbenchmarks on the developer host (not campaign results).
// Old download path: Open (full MD5+SHA-512 before the first byte) + copy.
// New: OpenStream + copy (net/http adds sendfile on a real socket).

func benchStore(b *testing.B, size int) (*Filesystem, Key, Info) {
	b.Helper()
	root := b.TempDir()
	s, err := newBenchFilesystem(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	k := Key{ImageID: "3f0b7c9e-1d2a-4b5c-8d6e-7f8091a2b3c4", AttemptID: "1f0b7c9e-1d2a-4b5c-8d6e-7f8091a2b3c4"}
	data := bytes.Repeat([]byte("glance-go-bench-"), size/16)
	n := int64(len(data))
	i, err := s.Stage(context.Background(), k, bytes.NewReader(data), n, &n)
	if err != nil {
		b.Fatal(err)
	}
	if err = s.Publish(context.Background(), k, i); err != nil {
		b.Fatal(err)
	}
	return s, k, i
}

func BenchmarkDownload64MiBVerifiedOpen(b *testing.B) {
	s, k, i := benchStore(b, 64<<20)
	b.SetBytes(i.Size)
	for b.Loop() {
		f, _, err := s.Open(context.Background(), k)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, f)
		_ = f.Close()
	}
}

func BenchmarkDownload64MiBOpenStream(b *testing.B) {
	s, k, i := benchStore(b, 64<<20)
	b.SetBytes(i.Size)
	for b.Loop() {
		f, _, err := s.OpenStream(context.Background(), k)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, f)
		_ = f.Close()
	}
}

func benchmarkUpload(b *testing.B, reread bool) {
	root := b.TempDir()
	s, err := newBenchFilesystem(root)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	data := bytes.Repeat([]byte("glance-go-bench-"), (64<<20)/16)
	n := int64(len(data))
	b.SetBytes(n)
	for b.Loop() {
		k := key()
		i, err := s.Stage(context.Background(), k, bytes.NewReader(data), n, &n)
		if err != nil {
			b.Fatal(err)
		}
		if reread { // old behaviour: no record, Publish re-reads and re-hashes
			name, _ := k.name()
			s.forgetStaged(name)
		}
		if err = s.Publish(context.Background(), k, i); err != nil {
			b.Fatal(err)
		}
		_ = s.Delete(context.Background(), k)
	}
}

func BenchmarkUpload64MiBPublishRereads(b *testing.B)  { benchmarkUpload(b, true) }
func BenchmarkUpload64MiBPublishFastPath(b *testing.B) { benchmarkUpload(b, false) }

func newBenchFilesystem(root string) (*Filesystem, error) {
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	scope := uuid.NewString()
	if err := Initialize(root, scope); err != nil {
		return nil, err
	}
	return NewFilesystem(root, scope)
}
