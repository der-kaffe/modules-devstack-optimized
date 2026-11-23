//go:build linux

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBackendsCopyAndDelete(t *testing.T) {
	s, official := sharedFixture(t)
	fast := filepath.Join(filepath.Dir(official), "fast")
	if err := os.Mkdir(fast, 0755); err != nil {
		t.Fatal(err)
	}
	if err := s.AddBackend("fast", fast); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := key()
	info := publish(t, s, k, []byte("multi"))
	got, err := s.Copy(ctx, k, "fast")
	if err != nil || got != info {
		t.Fatal("copy", got, info, err)
	}
	if _, err = s.Copy(ctx, k, "fast"); !errors.Is(err, ErrExists) {
		t.Fatal("second copy must not overwrite", err)
	}
	if _, err = s.Copy(ctx, k, "missing"); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("unknown backend", err)
	}
	fk := k
	fk.Backend = "fast"
	f, measured, err := s.Open(ctx, fk)
	if err != nil || measured != info {
		t.Fatal("open copy", err)
	}
	_ = f.Close()
	// Only the default copy goes, then Delete with no backend removes all.
	if err = s.DeleteDefault(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(official, k.ImageID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default copy kept", err)
	}
	if _, err = os.Stat(filepath.Join(fast, k.ImageID)); err != nil {
		t.Fatal("fast copy removed", err)
	}
	if err = s.Delete(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(fast, k.ImageID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete must remove every store's copy", err)
	}
	// A group-writable backend directory is refused.
	loose := filepath.Join(filepath.Dir(official), "loose")
	_ = os.Mkdir(loose, 0777)
	_ = os.Chmod(loose, 0777)
	if err = s.AddBackend("loose", loose); !errors.Is(err, ErrUnsafe) {
		t.Fatal("unsafe backend accepted", err)
	}
}
