//go:build linux && rbd

package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"
	"github.com/ceph/go-ceph/rbd"
	"github.com/google/uuid"
)

// Requires a real cluster (infra/ceph-micro): GLANCE_GO_RBD_TEST_CONF names its
// ceph.conf and GLANCE_GO_RBD_TEST_POOL an rbd pool used only by these tests.
func rbdTestConfig(t *testing.T) RBDConfig {
	t.Helper()
	conf := os.Getenv("GLANCE_GO_RBD_TEST_CONF")
	if conf == "" {
		t.Fatal("rbd tag requires GLANCE_GO_RBD_TEST_CONF (see infra/ceph-micro)")
	}
	pool := os.Getenv("GLANCE_GO_RBD_TEST_POOL")
	if pool == "" {
		pool = "glance-go"
	}
	return RBDConfig{ConfFile: conf, User: "admin", Pool: pool}
}

// freshRBD binds the pool to a new scope; earlier markers belong to old tests.
func freshRBD(t *testing.T) (*RBD, RBDConfig, string) {
	t.Helper()
	c := rbdTestConfig(t)
	conn, ioctx, err := c.open()
	if err != nil {
		t.Fatal(err)
	}
	if err = ioctx.Delete(rbdMarkerObject); err != nil && !errors.Is(err, rados.ErrNotFound) {
		t.Fatal(err)
	}
	ioctx.Destroy()
	conn.Shutdown()
	scope := uuid.NewString()
	if err = InitializeRBD(c, scope); err != nil {
		t.Fatal(err)
	}
	s, err := NewRBD(c, scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c, scope
}

func newKey() Key { return Key{ImageID: uuid.NewString(), AttemptID: uuid.NewString()} }

func payload(n int) ([]byte, Info) {
	data := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(data)
	m, h := md5.Sum(data), sha512.Sum512(data)
	return data, Info{Size: int64(n), Checksum: hex.EncodeToString(m[:]), SHA512: hex.EncodeToString(h[:])}
}

func TestRBDRoundTripUnknownLength(t *testing.T) {
	s, _, _ := freshRBD(t)
	ctx, key := context.Background(), newKey()
	data, want := payload(3<<20 + 7) // not a multiple of the object size
	got, err := s.Stage(ctx, key, bytes.NewReader(data), 1<<30, nil)
	if err != nil || got != want {
		t.Fatalf("stage %v %+v", err, got)
	}
	if err = s.Publish(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	r, info, err := s.Open(ctx, key)
	if err != nil || info != want {
		t.Fatalf("open %v %+v", err, info)
	}
	read, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(read, data) {
		t.Fatalf("content differs: %v (%d bytes)", err, len(read))
	}
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err = s.Publish(ctx, key, got); !errors.Is(err, ErrExists) {
		t.Fatalf("republish must not replace: %v", err)
	}
	if _, err = s.Stage(ctx, key, bytes.NewReader(data), 1<<30, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("stage over published: %v", err)
	}
	if err = s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open after delete: %v", err)
	}
	if err = s.Delete(ctx, key); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
}

func TestRBDPublishedImageHasProtectedSnapshot(t *testing.T) {
	s, _, _ := freshRBD(t)
	ctx, key := context.Background(), newKey()
	data, want := payload(1 << 20)
	size := want.Size
	if _, err := s.Stage(ctx, key, bytes.NewReader(data), 1<<30, &size); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, key, want); err != nil {
		t.Fatal(err)
	}
	name, _ := key.name()
	img, err := rbd.OpenImageReadOnly(s.ioctx, name+".blob", rbd.NoSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	protected, err := img.GetSnapshot(rbdSnapshot).IsProtected()
	if err != nil || !protected {
		t.Fatalf("snapshot %q must be protected for COW clones: %v", rbdSnapshot, err)
	}
	if err = s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
}

func TestRBDLimitsAndReceipts(t *testing.T) {
	s, _, _ := freshRBD(t)
	ctx := context.Background()
	data, want := payload(2 << 20)
	small := newKey()
	if _, err := s.Stage(ctx, small, bytes.NewReader(data), 1<<20, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit: %v", err)
	}
	declared, mismatch := want.Size+1, newKey()
	if _, err := s.Stage(ctx, mismatch, bytes.NewReader(data), 1<<30, &declared); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("declared size: %v", err)
	}
	tampered, key := want, newKey()
	tampered.Checksum = "00000000000000000000000000000000"
	if _, err := s.Stage(ctx, key, bytes.NewReader(data), 1<<30, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, key, tampered); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("receipt must match the staged bytes: %v", err)
	}
	if err := s.Publish(ctx, newKey(), want); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("publish without stage: %v", err)
	}
	// Failed attempts leave evidence until an explicit Delete.
	for _, k := range []Key{small, mismatch, key} {
		name, _ := k.name()
		if err := s.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".part", ".ready", ".blob"} {
			if _, err := rbd.OpenImageReadOnly(s.ioctx, name+suffix, rbd.NoSnapshot); !errors.Is(err, rbd.ErrNotFound) {
				t.Fatalf("%s%s left after delete: %v", name, suffix, err)
			}
		}
	}
	if _, err := s.Stage(ctx, Key{ImageID: "x", AttemptID: "y"}, bytes.NewReader(nil), 1, nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("key: %v", err)
	}
}

func TestRBDExclusiveOwnerAndScope(t *testing.T) {
	s, c, scope := freshRBD(t)
	if _, err := NewRBD(c, scope); !errors.Is(err, ErrBusy) {
		t.Fatalf("second owner: %v", err)
	}
	if _, err := NewRBD(c, uuid.NewString()); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("scope mismatch: %v", err)
	}
	if err := InitializeRBD(c, uuid.NewString()); !errors.Is(err, ErrExists) {
		t.Fatalf("re-initialize must not adopt: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stage(context.Background(), newKey(), bytes.NewReader(nil), 1, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	again, err := NewRBD(c, scope)
	if err != nil {
		t.Fatalf("lock released on close: %v", err)
	}
	_ = again.Close()
}

// Nova with images_type=rbd clones the protected snapshot; Glance must not
// delete content that a running instance depends on.
func TestRBDDeleteRefusedWhileCloned(t *testing.T) {
	s, _, _ := freshRBD(t)
	ctx, key := context.Background(), newKey()
	data, want := payload(1 << 20)
	if _, err := s.Stage(ctx, key, bytes.NewReader(data), 1<<30, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, key, want); err != nil {
		t.Fatal(err)
	}
	name, _ := key.name()
	parent, err := rbd.OpenImage(s.ioctx, name+".blob", rbd.NoSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	cloneName := "tt-nova-disk-" + uuid.NewString()[:8]
	if _, err = parent.Clone(rbdSnapshot, s.ioctx, cloneName, rbd.FeatureLayering, rbdOrder); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()
	if err = s.Delete(ctx, key); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete with a clone must be refused: %v", err)
	}
	if _, _, err = s.Open(ctx, key); err != nil {
		t.Fatalf("content must survive a refused delete: %v", err)
	}
	if err = rbd.RemoveImage(s.ioctx, cloneName); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, key); err != nil {
		t.Fatalf("delete after the clone is gone: %v", err)
	}
}

// Largest campaign size plus one byte, unknown length: crosses 4 MiB objects and
// the 64 MiB growth step. Durations are logged as description, not a benchmark
// (memstore OSD on the developer host).
func TestRBDLargeImageAcrossObjectsAndGrowth(t *testing.T) {
	s, _, _ := freshRBD(t)
	ctx, key := context.Background(), newKey()
	data, want := payload(64<<20 + 1)
	started := time.Now()
	got, err := s.Stage(ctx, key, bytes.NewReader(data), 1<<30, nil)
	if err != nil || got != want {
		t.Fatalf("stage %v", err)
	}
	staged := time.Now()
	if err = s.Publish(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	published := time.Now()
	r, info, err := s.Open(ctx, key)
	if err != nil || info != want {
		t.Fatalf("open %v", err)
	}
	opened := time.Now()
	h := sha512.New()
	n, err := io.Copy(h, r)
	_ = r.Close()
	if err != nil || n != want.Size || hex.EncodeToString(h.Sum(nil)) != want.SHA512 {
		t.Fatalf("download differs: %v %d", err, n)
	}
	t.Logf("64 MiB+1: stage %v, publish(verify+snap) %v, open(verify) %v, read %v",
		staged.Sub(started).Round(time.Millisecond), published.Sub(staged).Round(time.Millisecond),
		opened.Sub(published).Round(time.Millisecond), time.Since(opened).Round(time.Millisecond))
	if err = s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
}
