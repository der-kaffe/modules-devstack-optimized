//go:build linux

package store

import (
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const markerName = ".glance-go-store"

// fileOps is an instance-local fault-injection seam. Production always uses
// actual writes, fsyncs, descriptor-relative links and unlinks.
type fileOps struct {
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	link   func(*os.File, *os.File, string) error
	unlink func(*os.File, string) error
}

func realOps() fileOps {
	return fileOps{
		write: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		sync:  func(f *os.File) error { return f.Sync() },
		link:  linkDescriptor,
		unlink: func(dir *os.File, name string) error {
			return syscall.Unlinkat(int(dir.Fd()), name)
		},
	}
}

type keyLock struct {
	gate chan struct{}
	refs int
}

// Filesystem is Linux-only. Close waits for running operations, but not for
// descriptors already returned by Open. Never replace a live root/lock file.
type Filesystem struct {
	life    sync.RWMutex
	closed  bool
	root    *os.File
	lock    *os.File
	staging *os.File
	objects *os.File
	keysMu  sync.Mutex
	keys    map[string]*keyLock
	staged  map[string]stagedIdentity // guarded by keysMu
	ops     fileOps
}

// stagedIdentity proves a .ready is the very file this instance's Stage just
// measured: same device, inode, size and change time. ctime cannot be set by
// unprivileged code, and any write, truncate, chmod, link or rename changes it.
// Official Glance hashes once while writing and never re-reads; Publish skips
// its re-read only when this proof holds, and otherwise verifies fully.
type stagedIdentity struct {
	dev, ino            uint64
	size                int64
	ctimeSec, ctimeNsec int64
	info                Info
}

func fileIdentity(f *os.File, info Info) (stagedIdentity, bool) {
	stat, err := f.Stat()
	if err != nil {
		return stagedIdentity{}, false
	}
	st, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return stagedIdentity{}, false
	}
	return stagedIdentity{dev: uint64(st.Dev), ino: st.Ino, size: st.Size, ctimeSec: st.Ctim.Sec, ctimeNsec: st.Ctim.Nsec, info: info}, true
}

func (s *Filesystem) rememberStaged(name string, id stagedIdentity) {
	s.keysMu.Lock()
	if s.staged == nil {
		s.staged = map[string]stagedIdentity{}
	}
	s.staged[name] = id
	s.keysMu.Unlock()
}

// unchangedSinceStage reports whether f still is the measured .ready for this
// receipt. It consumes the record: a second Publish attempt verifies fully.
func (s *Filesystem) unchangedSinceStage(name string, f *os.File, expected Info) bool {
	s.keysMu.Lock()
	want, ok := s.staged[name]
	delete(s.staged, name)
	s.keysMu.Unlock()
	if !ok || want.info != expected {
		return false
	}
	got, ok := fileIdentity(f, expected)
	return ok && got == want
}

func (s *Filesystem) forgetStaged(name string) {
	s.keysMu.Lock()
	delete(s.staged, name)
	s.keysMu.Unlock()
}

var _ Store = (*Filesystem)(nil)

func marker(scope string) string { return "glance-go-filesystem-v1\n" + scope + "\n" }

// Initialize explicitly prepares an EXISTING EMPTY private root. scope is a
// caller-managed UUID (future service binds it to its own database). It never
// adopts populated directories, repairs an existing root or changes permissions.
// Initialization errors can leave partial setup for human review.
func Initialize(rootPath, scope string) error {
	id, err := canonicalID(scope)
	if err != nil {
		return ErrUnsafe
	}
	root, err := openRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := root.Readdirnames(-1)
	if err != nil || len(entries) != 0 {
		return ErrUnsafe
	}
	lock, err := openEntry(root, ".lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		return opError("initialize lock", err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrBusy
	}
	for _, name := range []string{"staging", "objects"} {
		if err = syscall.Mkdirat(int(root.Fd()), name, 0700); err != nil {
			return opError("initialize directory", err)
		}
		dir, err := openEntry(root, name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		err = checkEntry(dir, true, root)
		if err == nil {
			err = dir.Sync()
		}
		closeErr := dir.Close()
		if err != nil || closeErr != nil {
			return opError("initialize directory sync", errors.Join(err, closeErr))
		}
	}
	f, err := openEntry(root, markerName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, marker(id))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close(), lock.Sync(), root.Sync())
	return opError("initialize sync", err)
}

// NewFilesystem opens only an explicitly initialized root with the expected
// scope marker and takes a non-blocking process lock. No SQL or service starts.
func NewFilesystem(rootPath, scope string) (*Filesystem, error) {
	id, err := canonicalID(scope)
	if err != nil {
		return nil, ErrUnsafe
	}
	root, err := openRoot(rootPath)
	if err != nil {
		return nil, err
	}
	s := &Filesystem{root: root, keys: map[string]*keyLock{}, ops: realOps()}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()
	s.lock, err = openEntry(root, ".lock", syscall.O_RDWR, 0)
	if err != nil {
		return nil, opError("open lock", err)
	}
	if err = checkEntry(s.lock, false, root); err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, ErrBusy
	}
	f, err := openEntry(root, markerName, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, opError("open marker", err)
	}
	err = checkEntry(f, false, root)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(f, 129)) // small marker, never image data
	}
	err = errors.Join(err, f.Close())
	if err != nil || string(data) != marker(id) {
		return nil, ErrUnsafe
	}
	for _, item := range []struct {
		name string
		dst  **os.File
	}{{"staging", &s.staging}, {"objects", &s.objects}} {
		*item.dst, err = openEntry(root, item.name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, opError("open directory", err)
		}
		if err = checkEntry(*item.dst, true, root); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

// openRoot walks every component with O_NOFOLLOW, pins descriptors and checks
// the opened inode. It never does Lstat(path) followed by Open(path).
func openRoot(name string) (*os.File, error) {
	const pythonStore = "/opt/stack/data/glance/images"
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" || name == pythonStore || strings.HasPrefix(name, pythonStore+"/") {
		return nil, ErrUnsafe
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, opError("open root anchor", err)
	}
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if e != nil {
			return nil, opError("open root component", e)
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), "root")
	if err = checkEntry(f, true, nil); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openEntry(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func checkEntry(f *os.File, directory bool, root *os.File) error {
	var stat, anchor syscall.Stat_t
	if syscall.Fstat(int(f.Fd()), &stat) != nil {
		return ErrUnsafe
	}
	mode := uint32(0600)
	kind := uint32(syscall.S_IFREG)
	if directory {
		mode, kind = 0700, syscall.S_IFDIR
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&syscall.S_IFMT != kind || stat.Mode&07777 != mode {
		return ErrUnsafe
	}
	if root != nil && (syscall.Fstat(int(root.Fd()), &anchor) != nil || stat.Dev != anchor.Dev) {
		return ErrUnsafe
	}
	return nil
}

// linkDescriptor uses Linux linkat through the kernel's /proc/self/fd handle,
// AT_SYMLINK_FOLLOW applies ONLY to that trusted descriptor reference. The source
// inode was opened with O_NOFOLLOW and fstat-verified; its pathname is not reopened.
// linkat atomically creates a hard link or fails EEXIST, never replaces a target.
// Unlike AT_EMPTY_PATH this needs no CAP_DAC_READ_SEARCH. Missing procfs/hard-link
// support fails closed; there is no unsafe check-then-rename fallback.
func linkDescriptor(src, dir *os.File, name string) error {
	source, err := syscall.BytePtrFromString(fmt.Sprintf("/proc/self/fd/%d", src.Fd()))
	if err != nil {
		return err
	}
	dest, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	const atSymlinkFollow = 0x400
	// AT_FDCWD is -100 represented as a machine word.
	atFDCWD := ^uintptr(99)
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, atFDCWD, uintptr(unsafe.Pointer(source)), dir.Fd(), uintptr(unsafe.Pointer(dest)), atSymlinkFollow, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func opError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		err = errors.Join(ErrStoreFull, err)
	case errors.Is(err, syscall.EEXIST):
		err = errors.Join(ErrExists, err)
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		err = errors.Join(ErrUnsafe, err)
	}
	return fmt.Errorf("store %s: %w", op, err)
}

func (s *Filesystem) enter(ctx context.Context, key Key) (string, func(), error) {
	name, err := key.name()
	if err != nil {
		return "", nil, err
	}
	s.life.RLock()
	if s.closed {
		s.life.RUnlock()
		return "", nil, ErrClosed
	}
	s.keysMu.Lock()
	k := s.keys[name]
	if k == nil {
		k = &keyLock{gate: make(chan struct{}, 1)}
		k.gate <- struct{}{}
		s.keys[name] = k
	}
	k.refs++
	s.keysMu.Unlock()
	drop := func() {
		s.keysMu.Lock()
		k.refs--
		if k.refs == 0 {
			delete(s.keys, name)
		}
		s.keysMu.Unlock()
		s.life.RUnlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return "", nil, ctx.Err()
	case <-k.gate:
	}
	return name, func() { k.gate <- struct{}{}; drop() }, nil
}

func (s *Filesystem) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var result error
	for _, f := range []*os.File{s.objects, s.staging, s.lock, s.root} {
		if f != nil {
			result = errors.Join(result, f.Close())
		}
	}
	return result
}

func (s *Filesystem) noFinal(name string) error {
	f, err := openEntry(s.objects, name+".blob", syscall.O_RDONLY, 0)
	if err == nil {
		_ = f.Close()
		return ErrExists
	}
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return opError("check final", err)
}

func (s *Filesystem) Stage(ctx context.Context, key Key, src io.Reader, limit int64, expected *int64) (info Info, result error) {
	if src == nil || limit < 0 || (expected != nil && *expected < 0) {
		return Info{}, ErrInvalidSize
	}
	var declared *int64
	if expected != nil {
		n := *expected
		declared = &n
		if n > limit {
			return Info{}, ErrTooLarge
		}
	}
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return Info{}, err
	}
	defer leave()
	if err = ctx.Err(); err != nil {
		return Info{}, err
	}
	if err = s.noFinal(name); err != nil {
		return Info{}, err
	}
	ready, err := openEntry(s.staging, name+".ready", syscall.O_RDONLY, 0)
	if err == nil {
		_ = ready.Close()
		return Info{}, ErrExists
	}
	if !errors.Is(err, syscall.ENOENT) {
		return Info{}, opError("check staged", err)
	}
	f, err := openEntry(s.staging, name+".part", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		return Info{}, opError("create stage", err)
	}
	defer func() {
		if e := f.Close(); e != nil {
			result = errors.Join(result, opError("close stage", e))
			info = Info{}
		}
	}()
	if err = checkEntry(f, false, s.root); err != nil {
		return Info{}, err
	}
	info, err = measure(ctx, src, func(b []byte) (int, error) { return s.ops.write(f, b) }, limit, declared)
	if err != nil {
		return Info{}, opError("stage stream", err)
	}
	if err = s.ops.sync(f); err != nil {
		return Info{}, opError("sync stage", err)
	}
	if err = ctx.Err(); err != nil {
		return Info{}, err
	}
	if err = s.ops.link(f, s.staging, name+".ready"); err != nil {
		return Info{}, opError("complete stage", err)
	}
	if err = s.ops.sync(s.staging); err != nil {
		return Info{}, opError("sync complete stage", err)
	}
	if err = s.ops.unlink(s.staging, name+".part"); err != nil {
		return Info{}, opError("unlink part", err)
	}
	if err = s.ops.sync(s.staging); err != nil {
		return Info{}, opError("sync stage cleanup", err)
	}
	// After the last link/unlink: those change ctime too.
	if id, ok := fileIdentity(f, info); ok {
		s.rememberStaged(name, id)
	}
	return info, nil
}

// md5Worker hashes MD5 on its own goroutine while the caller computes SHA-512
// and writes the same chunk (the two digests are independent). It never reads
// the source; each chunk is joined before its buffer is reused, and the
// goroutine exits before measure returns, so nothing is left behind.
type md5Worker struct {
	in   chan []byte
	done chan struct{}
	h    hash.Hash
}

func newMD5Worker() *md5Worker {
	w := &md5Worker{in: make(chan []byte), done: make(chan struct{}), h: md5.New()}
	go func() {
		for chunk := range w.in {
			_, _ = w.h.Write(chunk)
			w.done <- struct{}{}
		}
		close(w.done)
	}()
	return w
}

func (w *md5Worker) submit(chunk []byte) { w.in <- chunk }
func (w *md5Worker) wait()               { <-w.done }

// stop ends the goroutine; it must not be called while a chunk is pending.
func (w *md5Worker) stop() {
	close(w.in)
	<-w.done
}

func measure(ctx context.Context, src io.Reader, write func([]byte) (int, error), limit int64, expected *int64) (Info, error) {
	h := sha512.New()
	m := newMD5Worker()
	defer m.stop()
	buf := make([]byte, BufferSize)
	var size int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}
		remaining := limit - size
		if expected != nil && *expected-size < remaining {
			remaining = *expected - size
		}
		chunk := buf
		if remaining < int64(len(buf)) {
			chunk = buf[:remaining+1] // one extra byte, without overflowing int64
		}
		n, readErr := src.Read(chunk)
		if n < 0 || n > len(chunk) {
			return Info{}, errors.New("invalid reader count")
		}
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}
		if int64(n) > limit-size {
			return Info{}, ErrTooLarge
		}
		if expected != nil && int64(n) > *expected-size {
			return Info{}, ErrSizeMismatch
		}
		if n > 0 {
			emptyReads = 0
			data := chunk[:n]
			m.submit(data)
			var writeErr error
			if write != nil {
				written, err := write(data)
				if err == nil && written != n {
					err = io.ErrShortWrite
				}
				writeErr = err
			}
			_, _ = h.Write(data)
			m.wait() // buf is reused by the next Read
			if writeErr != nil {
				return Info{}, writeErr
			}
			size += int64(n)
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return Info{}, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return Info{}, readErr
			}
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if expected != nil && size != *expected {
		return Info{}, ErrSizeMismatch
	}
	return Info{Size: size, Checksum: hex.EncodeToString(m.h.Sum(nil)), SHA512: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *Filesystem) Publish(ctx context.Context, key Key, expected Info) error {
	if expected.Size < 0 || !validHash(expected.Checksum, 32) || !validHash(expected.SHA512, 128) {
		return ErrIntegrity
	}
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return err
	}
	defer leave()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.noFinal(name); err != nil {
		return err
	}
	f, err := openEntry(s.staging, name+".ready", syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return ErrIncomplete
	}
	if err != nil {
		return opError("open ready", err)
	}
	defer f.Close()
	if err = checkEntry(f, false, s.root); err != nil {
		return err
	}
	if !s.unchangedSinceStage(name, f, expected) {
		actual, err := measure(ctx, f, nil, expected.Size, &expected.Size)
		if err != nil {
			return errors.Join(ErrIntegrity, opError("verify ready", err))
		}
		if actual != expected {
			return ErrIntegrity
		}
	}
	if err = s.ops.sync(f); err != nil {
		return opError("sync publish file", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.ops.link(f, s.objects, name+".blob"); err != nil {
		return opError("publish", err)
	}
	// Destination is durable BEFORE removing either staging name. Any error
	// preserves enough evidence for caller-driven outcome resolution/recovery.
	if err = s.ops.sync(s.objects); err != nil {
		return opError("sync published directory", err)
	}
	for _, suffix := range []string{".ready", ".part"} {
		if err = s.ops.unlink(s.staging, name+suffix); err != nil && !errors.Is(err, syscall.ENOENT) {
			return opError("remove published stage", err)
		}
	}
	return opError("sync published cleanup", s.ops.sync(s.staging))
}

func (s *Filesystem) Open(ctx context.Context, key Key) (ReadSeekCloser, Info, error) {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return nil, Info{}, err
	}
	defer leave()
	if err = ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	f, err := openEntry(s.objects, name+".blob", syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, Info{}, ErrNotFound
	}
	if err != nil {
		return nil, Info{}, opError("open content", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if err = checkEntry(f, false, s.root); err != nil {
		return nil, Info{}, err
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, Info{}, opError("stat content", err)
	}
	size := stat.Size()
	info, err := measure(ctx, f, nil, size, &size)
	if err != nil {
		return nil, Info{}, errors.Join(ErrIntegrity, opError("measure content", err))
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, Info{}, opError("rewind content", err)
	}
	ok = true
	return f, info, nil
}

// OpenStream opens published content for serving without measuring it.
func (s *Filesystem) OpenStream(ctx context.Context, key Key) (ReadSeekCloser, int64, error) {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	defer leave()
	if err = ctx.Err(); err != nil {
		return nil, 0, err
	}
	f, err := openEntry(s.objects, name+".blob", syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, opError("open content", err)
	}
	if err = checkEntry(f, false, s.root); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, opError("stat content", err)
	}
	return f, stat.Size(), nil
}

func (s *Filesystem) Delete(ctx context.Context, key Key) error {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return err
	}
	defer leave()
	if err = ctx.Err(); err != nil {
		return err
	}
	s.forgetStaged(name)
	var result error
	for _, item := range []struct {
		dir    *os.File
		suffix string
	}{{s.objects, ".blob"}, {s.staging, ".ready"}, {s.staging, ".part"}} {
		if err = ctx.Err(); err != nil {
			result = errors.Join(result, err)
			break
		}
		if err = s.ops.unlink(item.dir, name+item.suffix); err != nil && !errors.Is(err, syscall.ENOENT) {
			result = errors.Join(result, opError("delete", err))
		}
	}
	// Even an ENOENT retry must sync: a previous unlink may have succeeded while
	// its directory sync failed. Never enumerate or remove any unrelated name.
	return errors.Join(result, opError("sync deleted objects", s.ops.sync(s.objects)), opError("sync deleted stages", s.ops.sync(s.staging)))
}

func validHash(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
