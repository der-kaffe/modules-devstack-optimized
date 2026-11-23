//go:build linux

package store

import (
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
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
	// backends are the other enabled_backends' directories, by name.
	backends map[string]*os.File
	keysMu   sync.Mutex
	keys     map[string]*keyLock
	staged   map[string]stagedIdentity // guarded by keysMu
	ops      fileOps
}

// dir is the objects directory of a backend ("" or unknown name: default).
func (s *Filesystem) dir(backend string) (*os.File, error) {
	if backend == "" {
		return s.objects, nil
	}
	if d, ok := s.backends[backend]; ok {
		return d, nil
	}
	return nil, ErrInvalidKey
}

// AddBackend opens another enabled_backends directory with the same
// checks as the default one (owner, no group/other write, same device).
func (s *Filesystem) AddBackend(name, path string) error {
	dir, err := openDirectory(path)
	if err != nil {
		return err
	}
	var stat syscall.Stat_t
	if syscall.Fstat(int(dir.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Mode&0022 != 0 {
		_ = dir.Close()
		return ErrUnsafe
	}
	if s.backends == nil {
		s.backends = map[string]*os.File{}
	}
	s.backends[name] = dir
	return nil
}

// final is the published name for a staged key name (<image>-<attempt>).
// final is the published name for a key name (<image>-<attempt>): the image
// ID, as glance_store names files in filesystem_store_datadir.
func (s *Filesystem) final(name string) string { return name[:36] }

// checkObject verifies a published file. The official store creates 0644
// files, so flat mode also accepts group/other read bits (never write/exec).
func (s *Filesystem) checkObject(f *os.File) error {
	var stat, anchor syscall.Stat_t
	if syscall.Fstat(int(f.Fd()), &stat) != nil || syscall.Fstat(int(s.root.Fd()), &anchor) != nil {
		return ErrUnsafe
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&07133 != 0 || stat.Mode&0600 != 0600 || stat.Dev != anchor.Dev {
		return ErrUnsafe
	}
	return nil
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

// openPrivateRoot opens only an explicitly initialized root with the expected
// scope marker and takes a non-blocking process lock. No SQL or service starts.
func openPrivateRoot(rootPath, scope string) (*Filesystem, error) {
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

// NewSharedFilesystem opens an initialized private root (lock, marker and
// staging) whose published objects live in the official Glance store
// directory objectsPath, named <image-id> as glance_store does. That
// directory must be owned by this user, not group/other writable, and on the
// root's filesystem so publication stays an atomic hard link.
func NewSharedFilesystem(rootPath, scope, objectsPath string) (*Filesystem, error) {
	s, err := openPrivateRoot(rootPath, scope)
	if err != nil {
		return nil, err
	}
	dir, err := openDirectory(objectsPath)
	if err == nil {
		var stat, anchor syscall.Stat_t
		if syscall.Fstat(int(dir.Fd()), &stat) != nil || syscall.Fstat(int(s.root.Fd()), &anchor) != nil ||
			stat.Uid != uint32(os.Geteuid()) || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Mode&0022 != 0 || stat.Dev != anchor.Dev {
			_ = dir.Close()
			err = ErrUnsafe
		}
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	_ = s.objects.Close()
	s.objects = dir
	return s, nil
}

// openDirectory walks an absolute clean path with O_NOFOLLOW at every step.
func openDirectory(name string) (*os.File, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" {
		return nil, ErrUnsafe
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, opError("open directory anchor", err)
	}
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if e != nil {
			return nil, opError("open directory component", e)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), name), nil
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
	for _, f := range s.backends {
		_ = f.Close()
	}
	for _, f := range []*os.File{s.objects, s.staging, s.lock, s.root} {
		if f != nil {
			result = errors.Join(result, f.Close())
		}
	}
	return result
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
	dir, err := s.dir(key.Backend)
	if err != nil {
		return Info{}, err
	}
	return s.stageDirect(ctx, dir, name, src, limit, declared)
}

// stage runs one per-chunk step (MD5, SHA-512 or the file write) on its own
// goroutine. measure feeds every chunk to all stages, so the two digests and
// the write proceed in parallel, while the next network read fills the other
// buffer. A buffer is reused only after every stage finished with it, and
// all goroutines exit before measure returns.
type stage struct {
	in   chan []byte
	done chan error
}

func newStage(run func([]byte) error) *stage {
	s := &stage{in: make(chan []byte), done: make(chan error)}
	go func() {
		for chunk := range s.in {
			s.done <- run(chunk)
		}
	}()
	return s
}

type pipeline struct {
	stages  []*stage
	pending bool
}

func (p *pipeline) submit(chunk []byte) {
	for _, s := range p.stages {
		s.in <- chunk
	}
	p.pending = true
}

// wait joins the chunk in flight and returns the first stage error.
func (p *pipeline) wait() error {
	if !p.pending {
		return nil
	}
	var first error
	for _, s := range p.stages {
		if err := <-s.done; err != nil && first == nil {
			first = err
		}
	}
	p.pending = false
	return first
}

func (p *pipeline) stop() {
	_ = p.wait()
	for _, s := range p.stages {
		close(s.in)
	}
}

func measure(ctx context.Context, src io.Reader, write func([]byte) (int, error), limit int64, expected *int64) (Info, error) {
	md, sh := md5.New(), sha512.New()
	stages := []*stage{
		newStage(func(b []byte) error { _, _ = md.Write(b); return nil }),
		newStage(func(b []byte) error { _, _ = sh.Write(b); return nil }),
	}
	if write != nil {
		stages = append(stages, newStage(func(b []byte) error {
			n, err := write(b)
			if err == nil && n != len(b) {
				err = io.ErrShortWrite
			}
			return err
		}))
	}
	p := &pipeline{stages: stages}
	defer p.stop()
	// Two buffers: the read of chunk n+1 overlaps the stages of chunk n.
	bufs := [2][]byte{make([]byte, BufferSize), make([]byte, BufferSize)}
	var size int64
	emptyReads, turn := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}
		remaining := limit - size
		if expected != nil && *expected-size < remaining {
			remaining = *expected - size
		}
		buf := bufs[turn]
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
			// The previous chunk (other buffer) must be fully hashed and
			// written before this one is handed to the stages.
			if err := p.wait(); err != nil {
				return Info{}, err
			}
			p.submit(chunk[:n])
			size += int64(n)
			turn ^= 1
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
	if err := p.wait(); err != nil {
		return Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if expected != nil && size != *expected {
		return Info{}, ErrSizeMismatch
	}
	return Info{Size: size, Checksum: hex.EncodeToString(md.Sum(nil)), SHA512: hex.EncodeToString(sh.Sum(nil))}, nil
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
	dir, err := s.dir(key.Backend)
	if err != nil {
		return err
	}
	return s.publishDirect(ctx, dir, name, expected)
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
	objects, err := s.dir(key.Backend)
	if err != nil {
		return nil, Info{}, err
	}
	f, err := openEntry(objects, s.final(name), syscall.O_RDONLY, 0)
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
	if err = s.checkObject(f); err != nil {
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
	objects, err := s.dir(key.Backend)
	if err != nil {
		return nil, 0, err
	}
	f, err := openEntry(objects, s.final(name), syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, opError("open content", err)
	}
	if err = s.checkObject(f); err != nil {
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
	// As glance_store's filesystem driver: unlink <image-id>, no directory
	// fsync. Under heavy writeback an ext4 directory fsync waits for the whole
	// journal and took longer than the request deadline (DELETE 409/503).
	if key.Backend != "" { // one store's copy
		dir, e := s.dir(key.Backend)
		if e != nil {
			return e
		}
		if e = s.ops.unlink(dir, s.final(name)); e != nil && !errors.Is(e, syscall.ENOENT) {
			return opError("delete", e)
		}
		return nil
	}
	for _, dir := range s.backends { // the image's copies in other stores
		if e := s.ops.unlink(dir, s.final(name)); e != nil && !errors.Is(e, syscall.ENOENT) {
			return opError("delete", e)
		}
	}
	if err = s.ops.unlink(s.objects, s.final(name)); err != nil && !errors.Is(err, syscall.ENOENT) {
		return opError("delete", err)
	}
	return nil
}

func validHash(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// stageDirect writes the upload straight to <image-id> in the official
// directory, as glance_store's filesystem driver does: no staging copy, no
// fsync, and a failed write removes the partial file. The caller holds the key.
func (s *Filesystem) stageDirect(ctx context.Context, objects *os.File, name string, src io.Reader, limit int64, declared *int64) (info Info, result error) {
	final := s.final(name)
	f, err := openEntry(objects, final, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0644)
	if errors.Is(err, syscall.EEXIST) {
		return Info{}, ErrExists
	}
	if err != nil {
		return Info{}, opError("create image file", err)
	}
	ok := false
	defer func() {
		if e := f.Close(); e != nil && result == nil {
			result, info = opError("close image file", e), Info{}
		}
		if !ok || result != nil {
			_ = s.ops.unlink(objects, final)
		}
	}()
	if err = s.checkObject(f); err != nil {
		return Info{}, err
	}
	info, err = measure(ctx, src, func(b []byte) (int, error) { return s.ops.write(f, b) }, limit, declared)
	if err != nil {
		return Info{}, opError("write image file", err)
	}
	if err = ctx.Err(); err != nil {
		return Info{}, err
	}
	if id, known := fileIdentity(f, info); known {
		s.rememberStaged(name, id)
	}
	ok = true
	return info, nil
}

// publishDirect confirms the directly written file is still the staged one;
// its final name already exists, so there is nothing to link.
func (s *Filesystem) publishDirect(ctx context.Context, objects *os.File, name string, expected Info) error {
	f, err := openEntry(objects, s.final(name), syscall.O_RDONLY, 0)
	if errors.Is(err, syscall.ENOENT) {
		return ErrIncomplete
	}
	if err != nil {
		return opError("open image file", err)
	}
	defer f.Close()
	if err = s.checkObject(f); err != nil {
		return err
	}
	if !s.unchangedSinceStage(name, f, expected) {
		actual, err := measure(ctx, f, nil, expected.Size, &expected.Size)
		if err != nil || actual != expected {
			return ErrIntegrity
		}
	}
	s.forgetStaged(name)
	return nil
}

// Copy writes the image file of from (its backend) to backend to, as
// copy-image: the destination is created exclusively, the bytes are hashed
// on the way and a failed copy leaves no destination file.
func (s *Filesystem) Copy(ctx context.Context, from Key, to string) (Info, error) {
	name, leave, err := s.enter(ctx, from)
	if err != nil {
		return Info{}, err
	}
	defer leave()
	src, err := s.dir(from.Backend)
	if err != nil {
		return Info{}, err
	}
	dst, err := s.dir(to)
	if err != nil {
		return Info{}, err
	}
	in, err := openEntry(src, s.final(name), syscall.O_RDONLY, 0)
	if err != nil {
		return Info{}, opError("copy", err)
	}
	defer in.Close()
	out, err := openEntry(dst, s.final(name), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return Info{}, ErrExists
		}
		return Info{}, opError("copy", err)
	}
	// The upload's pipeline: write, MD5 and SHA-512 in parallel with the
	// next read; the source must keep its size while copied.
	var size int64
	if st, e := in.Stat(); e == nil {
		size = st.Size()
	}
	info, err := measure(ctx, in, func(b []byte) (int, error) { return s.ops.write(out, b) }, size, &size)
	if e := out.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = s.ops.unlink(dst, s.final(name))
		return Info{}, opError("copy", err)
	}
	return info, nil
}

// DeleteDefault removes only the default store's copy of an image (the
// other stores keep theirs), for DELETE /v2/stores/{default}/{image}.
func (s *Filesystem) DeleteDefault(ctx context.Context, key Key) error {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return err
	}
	defer leave()
	if err = s.ops.unlink(s.objects, s.final(name)); err != nil && !errors.Is(err, syscall.ENOENT) {
		return opError("delete", err)
	}
	return nil
}
