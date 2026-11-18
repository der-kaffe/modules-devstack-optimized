//go:build linux && rbd

// Ceph RBD backend. Built only with the "rbd" tag (cgo, librados/librbd); the
// default binary keeps the dependency-free filesystem store. Same contract as
// Filesystem: <image>-<attempt>.part → .ready (verified receipt) → .blob, plus a
// protected snapshot "snap" so Nova (images_type=rbd) can clone copy-on-write.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"
	"github.com/ceph/go-ceph/rbd"
	"github.com/google/uuid"
)

const (
	rbdMarkerObject = "glance-go-store.scope"
	rbdLockName     = "glance-go-store"
	rbdSnapshot     = "snap"
	rbdOrder        = 22       // 4 MiB RADOS objects, librbd's default
	rbdGrowth       = 64 << 20 // resize step while the length is unknown
)

var rbdName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RBDConfig names a cluster config file, a cephx user and an existing pool.
// It is operator policy, never request input.
type RBDConfig struct {
	ConfFile, User, Pool string
}

func (c RBDConfig) open() (*rados.Conn, *rados.IOContext, error) {
	if c.ConfFile == "" || !rbdName.MatchString(c.User) || !rbdName.MatchString(c.Pool) {
		return nil, nil, ErrUnsafe
	}
	conn, err := rados.NewConnWithUser(c.User)
	if err != nil {
		return nil, nil, rbdError("connect", err)
	}
	if err = conn.ReadConfigFile(c.ConfFile); err == nil {
		err = conn.Connect()
	}
	if err != nil {
		conn.Shutdown()
		return nil, nil, rbdError("connect", err)
	}
	ioctx, err := conn.OpenIOContext(c.Pool)
	if err != nil {
		conn.Shutdown()
		return nil, nil, rbdError("open pool", err)
	}
	return conn, ioctx, nil
}

func rbdMarker(scope string) string { return "glance-go-rbd-v1\n" + scope + "\n" }

// InitializeRBD binds an empty pool (no marker yet) to the scope. It never
// adopts or overwrites another store's marker.
func InitializeRBD(c RBDConfig, scope string) error {
	id, err := canonicalID(scope)
	if err != nil {
		return ErrUnsafe
	}
	conn, ioctx, err := c.open()
	if err != nil {
		return err
	}
	defer conn.Shutdown()
	defer ioctx.Destroy()
	if err = ioctx.Create(rbdMarkerObject, rados.CreateExclusive); err != nil {
		return rbdError("initialize marker", err)
	}
	return rbdError("initialize marker", ioctx.WriteFull(rbdMarkerObject, []byte(rbdMarker(id))))
}

type RBD struct {
	conn   *rados.Conn
	ioctx  *rados.IOContext
	cookie string
	life   sync.RWMutex
	closed bool
	keysMu sync.Mutex
	keys   map[string]*keyLock
	staged map[string]rbdStaged // guarded by keysMu
}

// rbdStaged records what this instance's Stage measured. As official Glance's
// rbd driver, Publish does not re-read data written by this process; it
// re-reads only if the image id, size or receipt differ (or no record exists).
// Changing an RBD image in between requires the cluster credentials.
type rbdStaged struct {
	id   string
	size int64
	info Info
}

// NewRBD opens an initialized pool with the expected scope and takes the
// store's exclusive RADOS lock (another Glance Go instance gets ErrBusy).
func NewRBD(c RBDConfig, scope string) (*RBD, error) {
	id, err := canonicalID(scope)
	if err != nil {
		return nil, ErrUnsafe
	}
	conn, ioctx, err := c.open()
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*RBD, error) { ioctx.Destroy(); conn.Shutdown(); return nil, e }
	want := rbdMarker(id)
	buf := make([]byte, len(want)+1)
	n, err := ioctx.Read(rbdMarkerObject, buf, 0)
	if err != nil || string(buf[:n]) != want {
		return fail(errors.Join(ErrUnsafe, rbdError("verify marker", err)))
	}
	cookie := uuid.NewString()
	ret, err := ioctx.LockExclusive(rbdMarkerObject, rbdLockName, cookie, "glance-go", 0, nil)
	if err != nil {
		return fail(rbdError("lock store", err))
	}
	if ret != 0 {
		return fail(ErrBusy)
	}
	return &RBD{conn: conn, ioctx: ioctx, cookie: cookie, keys: map[string]*keyLock{}}, nil
}

func (s *RBD) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_, err := s.ioctx.Unlock(rbdMarkerObject, rbdLockName, s.cookie)
	s.ioctx.Destroy()
	s.conn.Shutdown()
	return rbdError("unlock store", err)
}

// enter serializes operations per key, like Filesystem.enter.
func (s *RBD) enter(ctx context.Context, key Key) (string, func(), error) {
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

// noFinal refuses to stage or publish over already published content.
func (s *RBD) noFinal(name string) error {
	img, err := rbd.OpenImageReadOnly(s.ioctx, name+".blob", rbd.NoSnapshot)
	if errors.Is(err, rbd.ErrNotFound) {
		return nil
	}
	if err != nil {
		return rbdError("check final", err)
	}
	return errors.Join(ErrExists, rbdError("check final", img.Close()))
}

func (s *RBD) Stage(ctx context.Context, key Key, src io.Reader, limit int64, expected *int64) (Info, error) {
	if limit < 0 || (expected != nil && *expected < 0) {
		return Info{}, ErrInvalidSize
	}
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return Info{}, err
	}
	defer leave()
	if err = s.noFinal(name); err != nil {
		return Info{}, err
	}
	var capacity int64
	if expected != nil {
		capacity = *expected
	}
	// Exclusive create: an earlier attempt's .part is evidence, not reused.
	if _, err = rbd.Create(s.ioctx, name+".part", uint64(capacity), rbdOrder); err != nil {
		return Info{}, rbdError("create staging", err)
	}
	img, err := rbd.OpenImage(s.ioctx, name+".part", rbd.NoSnapshot)
	if err != nil {
		return Info{}, rbdError("open staging", err)
	}
	var offset int64
	write := func(p []byte) (int, error) {
		if offset+int64(len(p)) > capacity {
			grown := max(offset+int64(len(p)), capacity+rbdGrowth)
			if err := img.Resize(uint64(grown)); err != nil {
				return 0, rbdError("grow staging", err)
			}
			capacity = grown
		}
		n, err := img.WriteAt(p, offset)
		offset += int64(n)
		return n, rbdError("write staging", err)
	}
	info, err := measure(ctx, src, write, limit, expected)
	if err == nil && capacity != info.Size {
		err = rbdError("trim staging", img.Resize(uint64(info.Size)))
	}
	if err == nil {
		err = rbdError("flush staging", img.Flush())
	}
	stagedID := func() (string, bool) { return "", false }
	if err == nil {
		if id, idErr := img.GetId(); idErr == nil {
			stagedID = func() (string, bool) { return id, true }
		}
	}
	// On failure the .part stays for explicit Delete/recovery, as on disk.
	if closeErr := rbdError("close staging", img.Close()); err == nil {
		err = closeErr
	}
	if err != nil {
		return Info{}, err
	}
	if err = rbd.GetImage(s.ioctx, name+".part").Rename(name + ".ready"); err != nil {
		return Info{}, rbdError("mark ready", err)
	}
	if id, ok := stagedID(); ok {
		s.keysMu.Lock()
		if s.staged == nil {
			s.staged = map[string]rbdStaged{}
		}
		s.staged[name] = rbdStaged{id: id, size: info.Size, info: info}
		s.keysMu.Unlock()
	}
	return info, nil
}

func (s *RBD) Publish(ctx context.Context, key Key, expected Info) error {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return err
	}
	defer leave()
	if err = s.noFinal(name); err != nil {
		return err
	}
	img, err := rbd.OpenImage(s.ioctx, name+".ready", rbd.NoSnapshot)
	if errors.Is(err, rbd.ErrNotFound) {
		return ErrIncomplete
	}
	if err != nil {
		return rbdError("open ready", err)
	}
	size := expected.Size
	s.keysMu.Lock()
	want, recorded := s.staged[name]
	delete(s.staged, name)
	s.keysMu.Unlock()
	unchanged := false
	if recorded && want.info == expected {
		id, idErr := img.GetId()
		raw, sizeErr := img.GetSize()
		unchanged = idErr == nil && sizeErr == nil && id == want.id && int64(raw) == want.size
	}
	if !unchanged {
		var info Info
		reader, rerr := newRBDReader(img)
		err = rerr
		if err == nil {
			info, err = measure(ctx, reader, nil, size, &size)
		}
		if err == nil && info != expected {
			err = ErrIntegrity
		}
	}
	if err == nil {
		// Snapshot before the name becomes final: a published .blob always has
		// the protected point-in-time Nova clones from.
		var snap *rbd.Snapshot
		if snap, err = img.CreateSnapshot(rbdSnapshot); err == nil {
			err = snap.Protect()
		}
		err = rbdError("snapshot", err)
	}
	if closeErr := rbdError("close ready", img.Close()); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// rbd_rename refuses an existing destination: never replaces published content.
	return rbdError("publish", rbd.GetImage(s.ioctx, name+".ready").Rename(name+".blob"))
}

// rbdReader reads an image up to its size. librbd answers EINVAL (not EOF) to
// a read that starts exactly at the end, which happens whenever the size is a
// multiple of the caller's buffer.
type rbdReader struct {
	img       *rbd.Image
	size, off int64
}

func newRBDReader(img *rbd.Image) (*rbdReader, error) {
	size, err := img.GetSize()
	if err != nil {
		return nil, rbdError("size content", err)
	}
	return &rbdReader{img: img, size: int64(size)}, nil
}

func (r *rbdReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if rest := r.size - r.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := r.img.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (r *rbdReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.off = offset
	return offset, nil
}

func (r *rbdReader) Close() error { return r.img.Close() }

func (s *RBD) Open(ctx context.Context, key Key) (ReadSeekCloser, Info, error) {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return nil, Info{}, err
	}
	defer leave()
	img, err := rbd.OpenImageReadOnly(s.ioctx, name+".blob", rbdSnapshot)
	if errors.Is(err, rbd.ErrNotFound) {
		return nil, Info{}, ErrNotFound
	}
	if err != nil {
		return nil, Info{}, rbdError("open content", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = img.Close()
		}
	}()
	reader, err := newRBDReader(img)
	if err != nil {
		return nil, Info{}, err
	}
	size := reader.size
	// Same contract as Filesystem.Open: freshly measured hashes. See
	// docs/glance/ceph-rbd-projection.md for the cost of this choice.
	info, err := measure(ctx, reader, nil, size, &size)
	if err != nil {
		return nil, Info{}, errors.Join(ErrIntegrity, rbdError("measure content", err))
	}
	if _, err = reader.Seek(0, io.SeekStart); err != nil {
		return nil, Info{}, err
	}
	ok = true
	return reader, info, nil
}

// OpenStream opens .blob@snap for serving without measuring it.
func (s *RBD) OpenStream(ctx context.Context, key Key) (ReadSeekCloser, int64, error) {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	defer leave()
	img, err := rbd.OpenImageReadOnly(s.ioctx, name+".blob", rbdSnapshot)
	if errors.Is(err, rbd.ErrNotFound) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, rbdError("open content", err)
	}
	reader, err := newRBDReader(img)
	if err != nil {
		_ = img.Close()
		return nil, 0, err
	}
	return reader, reader.size, nil
}

// Delete removes only this key's names. Protected snapshots with clones
// (images Nova is using) make it fail with ErrInUse and keep the content.
func (s *RBD) Delete(ctx context.Context, key Key) error {
	name, leave, err := s.enter(ctx, key)
	if err != nil {
		return err
	}
	defer leave()
	s.keysMu.Lock()
	delete(s.staged, name)
	s.keysMu.Unlock()
	for _, suffix := range []string{".part", ".ready", ".blob"} {
		if err := s.remove(name + suffix); err != nil {
			return err
		}
	}
	return nil
}

func (s *RBD) remove(name string) error {
	img, err := rbd.OpenImage(s.ioctx, name, rbd.NoSnapshot)
	if errors.Is(err, rbd.ErrNotFound) {
		return nil
	}
	if err != nil {
		return rbdError("open "+name, err)
	}
	snaps, err := img.GetSnapshotNames()
	for _, info := range snaps {
		if err != nil {
			break
		}
		snap := img.GetSnapshot(info.Name)
		var protected bool
		if protected, err = snap.IsProtected(); err == nil && protected {
			if err = snap.Unprotect(); errno(err) == syscall.EBUSY {
				err = ErrInUse
				break
			}
		}
		if err == nil {
			err = snap.Remove()
		}
	}
	if closeErr := img.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = rbd.RemoveImage(s.ioctx, name)
		if errors.Is(err, rbd.ErrNotFound) {
			err = nil
		}
	}
	if errors.Is(err, ErrInUse) {
		return err
	}
	return rbdError("delete "+name, err)
}

func errno(err error) syscall.Errno {
	var coded interface{ ErrorCode() int }
	if errors.As(err, &coded) {
		code := coded.ErrorCode()
		if code < 0 {
			code = -code
		}
		return syscall.Errno(code)
	}
	return 0
}

// rbdError classifies Ceph errnos into the store's sentinels, like opError.
func rbdError(op string, err error) error {
	if err == nil {
		return nil
	}
	switch errno(err) {
	case syscall.ENOSPC, syscall.EDQUOT:
		err = errors.Join(ErrStoreFull, err)
	case syscall.EEXIST:
		err = errors.Join(ErrExists, err)
	case syscall.EBUSY:
		err = errors.Join(ErrBusy, err)
	}
	return fmt.Errorf("store %s: %w", op, err)
}

// CountRBDImages reports how many RBD images the pool holds; operators and the
// integration test use it to prove a pool is new/empty or fully cleaned.
func CountRBDImages(c RBDConfig) (int, error) {
	conn, ioctx, err := c.open()
	if err != nil {
		return 0, err
	}
	defer conn.Shutdown()
	defer ioctx.Destroy()
	names, err := rbd.GetImageNames(ioctx)
	return len(names), rbdError("list images", err)
}
