//go:build linux

package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// copy-image between two file stores, 64 MiB.
func BenchmarkCopy64MiB(b *testing.B) {
	base := b.TempDir()
	root, official, fast, scope := filepath.Join(base, "root"), filepath.Join(base, "images"), filepath.Join(base, "fast"), uuid.NewString()
	for _, d := range []string{root, official, fast} {
		if err := os.Mkdir(d, 0o700); err != nil {
			b.Fatal(err)
		}
	}
	if err := Initialize(root, scope); err != nil {
		b.Fatal(err)
	}
	s, err := NewSharedFilesystem(root, scope, official)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if err = s.AddBackend("fast", fast); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	data := bytes.Repeat([]byte("glance-go-copy-"), (64<<20)/15)
	k := Key{ImageID: uuid.NewString(), AttemptID: uuid.NewString()}
	info, err := s.Stage(ctx, k, bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		b.Fatal(err)
	}
	if err = s.Publish(ctx, k, info); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		got, err := s.Copy(ctx, k, "fast")
		if err != nil || got != info {
			b.Fatal(got, err)
		}
		b.StopTimer()
		_ = os.Remove(filepath.Join(fast, k.ImageID))
		b.StartTimer()
	}
}
