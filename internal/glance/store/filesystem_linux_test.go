//go:build linux

package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests exercise actual local temporary filesystem operations and Linux
// linkat/fsync. Injected I/O failures are in faults_linux_test.go, not evidence
// of an actual full disk, process crash, MySQL or power-loss durability.
func fixture(t *testing.T) (*Filesystem, string, string) {
	t.Helper()
	root, scope := t.TempDir(), uuid.NewString()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(root, scope); err != nil {
		t.Fatal("initialize", err)
	}
	s, err := NewFilesystem(root, scope)
	if err != nil {
		t.Fatal("open store", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, root, scope
}

func key() Key { return Key{ImageID: uuid.NewString(), AttemptID: uuid.NewString()} }
func digest(b []byte) Info {
	m, h := md5.Sum(b), sha512.Sum512(b)
	return Info{Size: int64(len(b)), Checksum: hex.EncodeToString(m[:]), SHA512: hex.EncodeToString(h[:])}
}
func pathFor(root, dir string, k Key, suffix string) string {
	n, err := k.name()
	if err != nil {
		panic(err)
	}
	return filepath.Join(root, dir, n+suffix)
}
func publish(t *testing.T, s *Filesystem, k Key, b []byte) Info {
	t.Helper()
	i, err := s.Stage(context.Background(), k, bytes.NewReader(b), int64(len(b)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(context.Background(), k, i); err != nil {
		t.Fatal(err)
	}
	return i
}

type sizedReader struct {
	io.Reader
	maxRead int
}

func (r *sizedReader) Read(b []byte) (int, error) {
	if len(b) > r.maxRead {
		r.maxRead = len(b)
	}
	return r.Reader.Read(b)
}

func TestFilesystemStreamingRoundTripAndReopen(t *testing.T) {
	s, root, scope := fixture(t)
	k := key()
	b := bytes.Repeat([]byte("Glance-Go-\x00\xff"), BufferSize/3)
	reader := &sizedReader{Reader: bytes.NewReader(b)}
	expected := int64(len(b))
	i, err := s.Stage(context.Background(), k, reader, expected, &expected)
	if err != nil || i != digest(b) || reader.maxRead > BufferSize || len(b) <= BufferSize {
		t.Fatal("streaming/hash", i, err, reader.maxRead)
	}
	if _, _, err = s.Open(context.Background(), k); !errors.Is(err, ErrNotFound) {
		t.Fatal("staged bytes visible as published", err)
	}
	// A completed stage remains publishable across store instances.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFilesystem(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err = s2.Publish(context.Background(), k, i); err != nil {
		t.Fatal(err)
	}
	if err = s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := NewFilesystem(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	f, got, err := s3.Open(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f) // test-only; production Open returns a streaming FD
	if err != nil || !bytes.Equal(data, b) || got != i {
		t.Fatal("reopened content differs", got, err)
	}
	if _, err = f.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	segment := make([]byte, 15)
	if _, err = io.ReadFull(f, segment); err != nil || !bytes.Equal(segment, b[10:25]) {
		t.Fatal("seekable streaming descriptor", err)
	}
}

func TestFilesystemSizeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		limit    int64
		expected *int64
		want     error
	}{
		{"exact", "abc", 3, nil, nil},
		{"exceeded", "abcd", 3, nil, ErrTooLarge},
		{"zero", "", 0, nil, nil},
		{"zero exceeded", "x", 0, nil, ErrTooLarge},
		{"declared exact", "abc", 4, number(3), nil},
		{"declared short", "ab", 4, number(3), ErrSizeMismatch},
		{"declared long", "abcd", 5, number(3), ErrSizeMismatch},
		{"declared zero", "x", 4, number(0), ErrSizeMismatch},
		{"declaration over limit", "", 2, number(3), ErrTooLarge},
		{"invalid limit", "", -1, nil, ErrInvalidSize},
		{"invalid declaration", "", 4, number(-1), ErrInvalidSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, root, _ := fixture(t)
			k := key()
			i, err := s.Stage(context.Background(), k, strings.NewReader(tc.body), tc.limit, tc.expected)
			if !errors.Is(err, tc.want) {
				t.Fatal(err, "want", tc.want)
			}
			if err == nil {
				if i != digest([]byte(tc.body)) {
					t.Fatal(i)
				}
			} else {
				if _, e := os.Stat(pathFor(root, "staging", k, ".ready")); !os.IsNotExist(e) {
					t.Fatal("failure marked complete", e)
				}
			}
			if err = s.Delete(context.Background(), k); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func number(n int64) *int64 { return &n }

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

func TestFilesystemInterruptedAndCancelledStreams(t *testing.T) {
	for _, tc := range []string{"interrupted", "cancelled before", "cancelled during", "no progress", "bad reader"} {
		t.Run(tc, func(t *testing.T) {
			s, _, _ := fixture(t)
			k := key()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(io.ErrUnexpectedEOF)
			var r io.Reader = readerFunc(func(b []byte) (int, error) { return copy(b, "partial"), io.ErrUnexpectedEOF })
			switch tc {
			case "cancelled before":
				cancel()
				want = context.Canceled
			case "cancelled during":
				r = readerFunc(func(b []byte) (int, error) { cancel(); return copy(b, "partial"), nil })
				want = context.Canceled
			case "no progress":
				r = readerFunc(func([]byte) (int, error) { return 0, nil })
				want = io.ErrNoProgress
			case "bad reader":
				r = readerFunc(func(b []byte) (int, error) { return len(b) + 1, nil })
				want = nil // any error, no panic
			}
			_, err := s.Stage(ctx, k, r, 100, nil)
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal(err)
			}
			if err = s.Publish(context.Background(), k, digest([]byte("partial"))); !errors.Is(err, ErrIncomplete) {
				t.Fatal("partial upload published", err)
			}
			if err = s.Delete(context.Background(), k); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFilesystemReceiptsAndNoOverwrite(t *testing.T) {
	s, root, _ := fixture(t)
	k := key()
	i, err := s.Stage(context.Background(), k, strings.NewReader("first"), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []Info{{Size: -1}, {Size: 5}, digest([]byte("other"))} {
		if err = s.Publish(context.Background(), k, receipt); !errors.Is(err, ErrIntegrity) {
			t.Fatal(err)
		}
	}
	if _, err = s.Stage(context.Background(), k, strings.NewReader("overwrite"), 20, nil); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if err = os.WriteFile(pathFor(root, "staging", k, ".ready"), []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(context.Background(), k, i); !errors.Is(err, ErrIntegrity) {
		t.Fatal("corrupted stage published", err)
	}
	if err = s.Delete(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	i = publish(t, s, k, []byte("first"))
	if err = s.Publish(context.Background(), k, i); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if _, err = s.Stage(context.Background(), k, strings.NewReader("second"), 10, nil); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	f, got, err := s.Open(context.Background(), k)
	if err != nil || got != i {
		t.Fatal(got, err)
	}
	f.Close()
}

func TestFilesystemInitializationPermissionsAndLock(t *testing.T) {
	s, root, scope := fixture(t)
	if _, err := NewFilesystem(root, scope); !errors.Is(err, ErrBusy) {
		t.Fatal("second writer allowed", err)
	}
	k := key()
	publish(t, s, k, []byte("content"))
	for name, mode := range map[string]os.FileMode{".": 0700, "staging": 0700, "objects": 0700, ".lock": 0600, markerName: 0600, filepath.Join("objects", mustName(k)+".blob"): 0600} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal(name, info, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystem(root, uuid.NewString()); !errors.Is(err, ErrUnsafe) {
		t.Fatal("scope mismatch accepted", err)
	}
	if err := Initialize(root, scope); !errors.Is(err, ErrUnsafe) {
		t.Fatal("populated root adopted", err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystem(root, scope); !errors.Is(err, ErrUnsafe) {
		t.Fatal("non-private root", err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pathFor(root, "objects", k, ".blob"), 0644); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFilesystem(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err = s2.Open(context.Background(), k); !errors.Is(err, ErrUnsafe) {
		t.Fatal("insecure blob", err)
	}
}

func mustName(k Key) string { n, _ := k.name(); return n }

func TestFilesystemRootAndEntrySymlinks(t *testing.T) {
	for _, tc := range []string{"root", "parent", "staging", "part", "ready", "blob"} {
		t.Run(tc, func(t *testing.T) {
			s, root, scope := fixture(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "untouched")
			if err := os.WriteFile(target, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			k := key()
			var err error
			switch tc {
			case "root", "parent":
				link := filepath.Join(t.TempDir(), "root-link")
				if err = os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				name := link
				if tc == "parent" {
					if err = os.Mkdir(filepath.Join(root, "child"), 0700); err != nil {
						t.Fatal(err)
					}
					name = filepath.Join(link, "child")
				}
				_, err = NewFilesystem(name, scope)
			case "staging":
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(filepath.Join(root, "staging")); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(outside, filepath.Join(root, "staging")); err != nil {
					t.Fatal(err)
				}
				_, err = NewFilesystem(root, scope)
			case "part", "ready":
				if err = os.Symlink(target, pathFor(root, "staging", k, "."+tc)); err != nil {
					t.Fatal(err)
				}
				_, err = s.Stage(context.Background(), k, strings.NewReader("evil"), 10, nil)
				if tc == "ready" {
					if e := s.Publish(context.Background(), k, digest([]byte("private"))); e == nil {
						t.Fatal("followed ready symlink")
					}
				}
			case "blob":
				if err = os.Symlink(target, pathFor(root, "objects", k, ".blob")); err != nil {
					t.Fatal(err)
				}
				_, _, err = s.Open(context.Background(), k)
				if e := s.Delete(context.Background(), k); e != nil {
					t.Fatal(e)
				}
			}
			if err == nil {
				t.Fatal("symlink accepted")
			}
			data, e := os.ReadFile(target)
			if e != nil || string(data) != "private" {
				t.Fatal("outside target touched", e)
			}
		})
	}
}

func TestFilesystemInvalidKeysTraversalAndClosed(t *testing.T) {
	s, _, _ := fixture(t)
	for _, invalid := range []string{"", "../escape", "/tmp/escape", uuid.NewString() + "/../x", strings.Repeat("a", 32), "00000000-0000-4000-8000-00000000000z"} {
		for _, k := range []Key{{ImageID: invalid, AttemptID: uuid.NewString()}, {ImageID: uuid.NewString(), AttemptID: invalid}} {
			if _, err := s.Stage(context.Background(), k, strings.NewReader("x"), 1, nil); !errors.Is(err, ErrInvalidKey) {
				t.Fatal(err)
			}
			if err := s.Publish(context.Background(), k, digest(nil)); !errors.Is(err, ErrInvalidKey) {
				t.Fatal(err)
			}
			if _, _, err := s.Open(context.Background(), k); !errors.Is(err, ErrInvalidKey) {
				t.Fatal(err)
			}
			if err := s.Delete(context.Background(), k); !errors.Is(err, ErrInvalidKey) {
				t.Fatal(err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), key()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestFilesystemRejectsUninitializedAndForeignRoots(t *testing.T) {
	for _, tc := range []string{"uninitialized", "populated", "public", "scope"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			scope := uuid.NewString()
			var err error
			switch tc {
			case "uninitialized":
				_, err = NewFilesystem(root, scope)
			case "populated":
				if err = os.WriteFile(filepath.Join(root, "foreign-data"), []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				err = Initialize(root, scope)
			case "public":
				if err = os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
				err = Initialize(root, scope)
			case "scope":
				err = Initialize(root, "../invalid")
			}
			if err == nil {
				t.Fatal("unsafe root accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc == "populated" {
				want = 1
			}
			if len(entries) != want {
				t.Fatal("foreign root modified", entries)
			}
		})
	}
	// Rejected lexically, before any access to the original location.
	if _, err := openRoot("/opt/stack/data/glance/images"); !errors.Is(err, ErrUnsafe) {
		t.Fatal(err)
	}
}

func TestFilesystemReopensPartialAttemptForExplicitCleanup(t *testing.T) {
	s, root, scope := fixture(t)
	k := key()
	r := readerFunc(func(b []byte) (int, error) { return copy(b, "partial"), io.ErrUnexpectedEOF })
	if _, err := s.Stage(context.Background(), k, r, 20, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFilesystem(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err = s2.Publish(context.Background(), k, digest([]byte("partial"))); !errors.Is(err, ErrIncomplete) {
		t.Fatal("incomplete attempt published after reopen", err)
	}
	if _, err = s2.Stage(context.Background(), k, strings.NewReader("overwrite"), 20, nil); !errors.Is(err, ErrExists) {
		t.Fatal("partial attempt overwritten", err)
	}
	if err = s2.Delete(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	if _, err = s2.Stage(context.Background(), k, strings.NewReader("explicit retry"), 20, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemCancellationPreservesPublishedBytes(t *testing.T) {
	s, _, _ := fixture(t)
	k := key()
	i := publish(t, s, k, []byte("keep"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Open(ctx, k); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, k, i); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, k); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f, got, err := s.Open(context.Background(), k)
	if err != nil || got != i {
		t.Fatal("cancelled operation changed bytes", got, err)
	}
	f.Close()
}

func TestFilesystemDeleteOnlyAuthorizedKeyAndKeepsOpenDescriptor(t *testing.T) {
	s, root, _ := fixture(t)
	k, other := key(), key()
	publish(t, s, k, []byte("still readable"))
	publish(t, s, other, []byte("other"))
	unknown := filepath.Join(root, "objects", "unknown-file")
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.Open(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for n := 0; n < 2; n++ {
		if err = s.Delete(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.Open(context.Background(), k); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil || string(b) != "still readable" {
		t.Fatal("open descriptor invalidated", err)
	}
	for _, name := range []string{unknown, pathFor(root, "objects", other, ".blob")} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal("unrelated entry removed", err)
		}
	}
}

func TestFilesystemConcurrentKeysAndCancelableWait(t *testing.T) {
	s, _, _ := fixture(t)
	k := key()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	r := readerFunc(func(b []byte) (int, error) {
		close(started)
		<-release
		return copy(b, "first"), io.EOF
	})
	go func() { _, err := s.Stage(context.Background(), k, r, 10, nil); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- s.Delete(ctx, k) }()
	select {
	case err := <-waited:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key wait ignored cancellation")
	}
	// A different key can complete while the first reader is blocked.
	publish(t, s, key(), []byte("parallel"))
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	i := digest([]byte("first"))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Publish(context.Background(), k, i) }()
	}
	wg.Wait()
	close(errs)
	var success, exists int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrExists) {
			exists++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || exists != 1 {
		t.Fatal(success, exists)
	}
}

func TestLinuxPublicationIsAtomicAndUsesPinnedInode(t *testing.T) {
	s, root, _ := fixture(t)
	// Exercise the real primitive directly: EEXIST must preserve the winner.
	f, err := openEntry(s.staging, "source", syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString("original inode"); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "staging", "source"), filepath.Join(root, "staging", "moved")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(root, "staging", "source")); err != nil {
		t.Fatal(err)
	}
	if err = linkDescriptor(f, s.objects, "destination"); err != nil {
		t.Fatal(err)
	}
	if err = linkDescriptor(f, s.objects, "destination"); !errors.Is(err, syscall.EEXIST) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "objects", "destination"))
	if err != nil || string(b) != "original inode" {
		t.Fatal("source pathname re-resolved", err)
	}
}

// Downloads use OpenStream (as glance_store: no re-hash); recovery and deletion
// keep Open, which still detects same-size corruption.
func TestFilesystemOpenStreamServesWithoutHashing(t *testing.T) {
	s, root, _ := fixture(t)
	k := key()
	b := bytes.Repeat([]byte("stream-\x00\xff"), BufferSize/2+3)
	expected := int64(len(b))
	i, err := s.Stage(context.Background(), k, bytes.NewReader(b), expected, &expected)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(context.Background(), k, i); err != nil {
		t.Fatal(err)
	}
	f, size, err := s.OpenStream(context.Background(), k)
	if err != nil || size != expected {
		t.Fatal(size, err)
	}
	if _, isFile := f.(*os.File); !isFile {
		t.Fatal("filesystem stream must be an *os.File (sendfile)")
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || !bytes.Equal(data, b) {
		t.Fatal("streamed content differs", err)
	}
	name, _ := k.name()
	blob, err := os.OpenFile(root+"/objects/"+name+".blob", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blob.WriteAt([]byte("X"), 5); err != nil { // same size, different bytes
		t.Fatal(err)
	}
	_ = blob.Close()
	if f, size, err = s.OpenStream(context.Background(), k); err != nil || size != expected {
		t.Fatal("stream must not hash", err)
	}
	_ = f.Close()
	// Open still measures: its fresh hashes no longer match the receipt, which
	// recovery and deletion compare (store returns facts, callers decide).
	verified, measured, err := s.Open(context.Background(), k)
	if err != nil || measured == i || measured.Size != i.Size {
		t.Fatal("verifying Open must expose the corruption", measured, err)
	}
	_ = verified.Close()
	if _, _, err = s.OpenStream(context.Background(), key()); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err = s.OpenStream(context.Background(), Key{ImageID: "x", AttemptID: "y"}); !errors.Is(err, ErrInvalidKey) {
		t.Fatal(err)
	}
}

// Publish skips its re-read only for the exact file Stage measured (official
// Glance hashes once while writing). In-place same-size edits change ctime.
func TestFilesystemPublishSkipsRereadOnlyForUnchangedStage(t *testing.T) {
	s, root, _ := fixture(t)
	k := key()
	i, err := s.Stage(context.Background(), k, strings.NewReader("measured-bytes"), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := k.name()
	ready, err := os.Open(pathFor(root, "staging", k, ".ready"))
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	if !s.unchangedSinceStage(name, ready, i) {
		t.Fatal("fresh stage must use the fast path")
	}
	if s.unchangedSinceStage(name, ready, i) {
		t.Fatal("record must be consumed by the first attempt")
	}
	k2 := key()
	i2, err := s.Stage(context.Background(), k2, strings.NewReader("measured-bytes"), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	name2, _ := k2.name()
	other := i2
	other.Checksum = strings.Repeat("0", 32)
	f2, err := os.Open(pathFor(root, "staging", k2, ".ready"))
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if s.unchangedSinceStage(name2, f2, other) {
		t.Fatal("a different receipt must verify fully")
	}
	k3 := key()
	i3, err := s.Stage(context.Background(), k3, strings.NewReader("measured-bytes"), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(pathFor(root, "staging", k3, ".ready"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)                   // ctime granularity
	if _, err = w.WriteAt([]byte("X"), 0); err != nil { // same size
		t.Fatal(err)
	}
	_ = w.Close()
	if err = s.Publish(context.Background(), k3, i3); !errors.Is(err, ErrIntegrity) {
		t.Fatal("in-place same-size edit after Stage must be rejected", err)
	}
	if err = s.Publish(context.Background(), k2, i2); err != nil {
		t.Fatal("untouched stage must publish", err)
	}
}
