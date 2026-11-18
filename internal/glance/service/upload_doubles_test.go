package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

const uploadImageID = "00000000-0000-4000-8000-000000000001"
const oldUploadAttempt = "00000000-0000-4000-8000-000000000002"

var uploadPayload = []byte("binary fixture bytes")

func uploadPtr[T any](v T) *T { return &v }
func uploadInfo(b []byte) store.Info {
	m, h := md5.Sum(b), sha512.Sum512(b)
	return store.Info{Size: int64(len(b)), Checksum: hex.EncodeToString(m[:]), SHA512: hex.EncodeToString(h[:])}
}

type uploadEvents struct {
	mu    sync.Mutex
	calls []string
	tx    int
	t     *testing.T
}

func (e *uploadEvents) add(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(name) > 3 && name[:3] == "fs." && e.tx != 0 {
		e.t.Error("filesystem operation while SQL transaction active")
	}
	e.calls = append(e.calls, name)
}
func (e *uploadEvents) transaction(delta int) { e.mu.Lock(); e.tx += delta; e.mu.Unlock() }
func (e *uploadEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.calls...)
}
func (e *uploadEvents) reset() { e.mu.Lock(); e.calls = nil; e.mu.Unlock() }

type uploadSQLFault struct {
	err     error
	applied bool
}

// This double models atomic SQL transitions, never filesystem side effects.
// Faults distinguish a lost acknowledgement from an unapplied transaction.
type uploadSQLDouble struct {
	mu            sync.Mutex
	log           *uploadEvents
	image         model.Image
	op            *model.DataOperation
	faults        map[string]uploadSQLFault
	before, after func(string)
	reads         int
	failReadAt    map[int]bool
	inventory     []model.DataOperation
	otherStates   map[string]model.DataState
}

func (d *uploadSQLDouble) run(ctx context.Context, name string, f func() error) error {
	d.log.add("sql." + name)
	if d.before != nil {
		d.before(name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	d.log.transaction(1)
	fault := d.faults[name]
	var err error
	if fault.err != nil && !fault.applied {
		err = fault.err
	} else {
		err = f()
		if err == nil {
			err = fault.err
		}
	}
	d.log.transaction(-1)
	d.mu.Unlock()
	if d.after != nil {
		d.after(name)
	}
	return err
}
func cloneUploadOp(o *model.DataOperation) *model.DataOperation {
	if o == nil {
		return nil
	}
	c := *o
	return &c
}
func (d *uploadSQLDouble) snapshot() model.DataState {
	return model.DataState{ImageID: d.image.ID, Status: d.image.Status, Deleted: d.image.Status == "deleted", Size: d.image.Size, VirtualSize: d.image.VirtualSize, Checksum: d.image.Checksum, HashAlgorithm: d.image.HashAlgorithm, HashValue: d.image.HashValue, Operation: cloneUploadOp(d.op)}
}

// Separate catalog reader, with no write API or filesystem dependency.
type uploadCatalogDouble struct {
	sql *uploadSQLDouble
	err error
}

func (c uploadCatalogDouble) Get(ctx context.Context, id string) (model.Image, error) {
	c.sql.log.add("catalog.Get")
	if ctx.Err() != nil {
		return model.Image{}, ctx.Err()
	}
	if c.err != nil {
		return model.Image{}, c.err
	}
	c.sql.mu.Lock()
	defer c.sql.mu.Unlock()
	if id != c.sql.image.ID || c.sql.image.Status == "deleted" {
		return model.Image{}, repository.ErrNotFound
	}
	return c.sql.image, nil
}
func (d *uploadSQLDouble) DataStateSnapshot(ctx context.Context, id string) (model.DataState, error) {
	return d.DataState(ctx, id) // same "State" event: tests observe reads, not locks
}
func (d *uploadSQLDouble) DataState(ctx context.Context, id string) (out model.DataState, err error) {
	err = d.run(ctx, "State", func() error {
		d.reads++
		if d.failReadAt[d.reads] {
			return errors.New("private SQL diagnostic")
		}
		if id != d.image.ID {
			s, ok := d.otherStates[id]
			if !ok {
				return repository.ErrNotFound
			}
			out = s
			out.Operation = cloneUploadOp(s.Operation)
			return nil
		}
		out = d.snapshot()
		return nil
	})
	return
}
func (d *uploadSQLDouble) ListDataOperations(ctx context.Context, after string, limit int) (out []model.DataOperation, err error) {
	err = d.run(ctx, "List", func() error {
		all := append([]model.DataOperation{}, d.inventory...)
		if d.inventory == nil && d.op != nil {
			all = append(all, *d.op)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ImageID < all[j].ImageID })
		out = []model.DataOperation{}
		for _, o := range all {
			if o.ImageID > after && len(out) < limit {
				out = append(out, o)
			}
		}
		return nil
	})
	return
}
func (d *uploadSQLDouble) ReserveData(ctx context.Context, id, attempt string, expected *int64, now time.Time) (out model.DataOperation, err error) {
	err = d.run(ctx, "Reserve", func() error {
		if id != d.image.ID || d.image.Status != "queued" || d.op != nil && d.op.Phase != model.DataCleaned {
			return repository.ErrConflict
		}
		d.image.Status = "saving"
		out = model.DataOperation{ImageID: id, AttemptID: attempt, Phase: model.DataReserved, ExpectedSize: expected, CreatedAt: now, UpdatedAt: now}
		d.op = cloneUploadOp(&out)
		return nil
	})
	return
}
func (d *uploadSQLDouble) check(id, attempt string, phase model.DataPhase) error {
	if id != d.image.ID || d.image.Status != "saving" || d.op == nil || d.op.AttemptID != attempt || d.op.Phase != phase {
		return repository.ErrConflict
	}
	return nil
}
func (d *uploadSQLDouble) StageData(ctx context.Context, id, attempt string, c model.DataContent, now time.Time) error {
	return d.run(ctx, "Stage", func() error {
		if err := d.check(id, attempt, model.DataReserved); err != nil {
			return err
		}
		if err := c.Validate(d.op.ExpectedSize); err != nil {
			return err
		}
		d.op.Phase = model.DataStaged
		d.op.Size = uploadPtr(c.Size)
		d.op.VirtualSize = c.VirtualSize
		d.op.Checksum = uploadPtr(c.Checksum)
		d.op.SHA512 = uploadPtr(c.SHA512)
		d.op.UpdatedAt = now
		return nil
	})
}
func (d *uploadSQLDouble) PublishData(ctx context.Context, id, attempt string, now time.Time) error {
	return d.run(ctx, "Publish", func() error {
		if err := d.check(id, attempt, model.DataStaged); err != nil {
			return err
		}
		d.op.Phase = model.DataPublished
		d.op.UpdatedAt = now
		return nil
	})
}
func (d *uploadSQLDouble) CommitData(ctx context.Context, id, attempt string, now time.Time) error {
	return d.run(ctx, "Commit", func() error {
		if err := d.check(id, attempt, model.DataPublished); err != nil {
			return err
		}
		d.op.Phase = model.DataCommitted
		d.op.UpdatedAt = now
		d.image.Status = "active"
		d.image.Size = d.op.Size
		d.image.VirtualSize = d.op.VirtualSize
		d.image.Checksum = d.op.Checksum
		d.image.HashAlgorithm = uploadPtr("sha512")
		d.image.HashValue = d.op.SHA512
		return nil
	})
}
func (d *uploadSQLDouble) BeginDataCleanup(ctx context.Context, id, attempt, target string, code *string, now time.Time) error {
	return d.run(ctx, "BeginCleanup", func() error {
		if d.op == nil || d.image.Status != "saving" || d.op.AttemptID != attempt || d.op.Phase == model.DataCommitted || target != "queued" {
			return repository.ErrConflict
		}
		d.op.Phase = model.DataCleanup
		d.op.CleanupTarget = uploadPtr(target)
		d.op.LastErrorCode = code
		d.op.UpdatedAt = now
		return nil
	})
}
func (d *uploadSQLDouble) CompleteDataCleanup(ctx context.Context, id, attempt string, now time.Time) error {
	return d.run(ctx, "CompleteCleanup", func() error {
		if err := d.check(id, attempt, model.DataCleanup); err != nil {
			return err
		}
		if d.op.CleanupTarget == nil || *d.op.CleanupTarget != "queued" {
			return repository.ErrConflict
		}
		d.op.Phase = model.DataCleaned
		d.image.Status = "queued"
		d.image.Size = nil
		d.image.VirtualSize = nil
		d.image.Checksum = nil
		d.image.HashAlgorithm = nil
		d.image.HashValue = nil
		return nil
	})
}

type uploadStoreFault struct {
	err     error
	applied bool
}

// Independent store: measures actual fixture bytes and knows no SQL phase/state.
type uploadStoreDouble struct {
	mu            sync.Mutex
	log           *uploadEvents
	faults        map[string]uploadStoreFault
	part, ready   bool
	staged, blob  []byte
	lastKey       store.Key
	before, after func(string, store.Key)
}

func (s *uploadStoreDouble) event(name string, k store.Key) {
	s.log.add("fs." + name)
	if s.before != nil {
		s.before(name, k)
	}
}
func (s *uploadStoreDouble) Stage(ctx context.Context, k store.Key, src io.Reader, limit int64, expected *int64) (store.Info, error) {
	s.event("Stage", k)
	s.mu.Lock()
	s.part = true
	s.lastKey = k
	s.mu.Unlock()
	if f := s.faults["Stage"]; f.err != nil && !f.applied {
		return store.Info{}, f.err
	}
	var data bytes.Buffer
	buf := make([]byte, 7)
	for {
		if err := ctx.Err(); err != nil {
			return store.Info{}, err
		}
		n, err := src.Read(buf)
		if ctx.Err() != nil {
			return store.Info{}, ctx.Err()
		}
		if int64(data.Len()+n) > limit {
			return store.Info{}, store.ErrTooLarge
		}
		data.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return store.Info{}, err
		}
	}
	if expected != nil && int64(data.Len()) != *expected {
		return store.Info{}, store.ErrSizeMismatch
	}
	s.mu.Lock()
	s.staged = append([]byte{}, data.Bytes()...)
	s.ready = true
	s.part = false
	s.mu.Unlock()
	if s.after != nil {
		s.after("Stage", k)
	}
	if f := s.faults["Stage"]; f.err != nil {
		return store.Info{}, f.err
	}
	return uploadInfo(data.Bytes()), nil
}
func (s *uploadStoreDouble) Publish(ctx context.Context, k store.Key, expected store.Info) error {
	s.event("Publish", k)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f := s.faults["Publish"]
	if f.err != nil && !f.applied {
		return f.err
	}
	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		return store.ErrIncomplete
	}
	if uploadInfo(s.staged) != expected {
		s.mu.Unlock()
		return store.ErrIntegrity
	}
	s.blob = append([]byte{}, s.staged...)
	s.ready = false
	s.mu.Unlock()
	if s.after != nil {
		s.after("Publish", k)
	}
	return f.err
}

type uploadReadCloser struct {
	*bytes.Reader
	log *uploadEvents
}

func (r uploadReadCloser) Close() error { r.log.add("fs.Close"); return nil }
func (s *uploadStoreDouble) Open(ctx context.Context, k store.Key) (store.ReadSeekCloser, store.Info, error) {
	s.event("Open", k)
	if ctx.Err() != nil {
		return nil, store.Info{}, ctx.Err()
	}
	if f := s.faults["Open"]; f.err != nil {
		return nil, store.Info{}, f.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blob == nil {
		return nil, store.Info{}, store.ErrNotFound
	}
	b := append([]byte{}, s.blob...)
	return uploadReadCloser{bytes.NewReader(b), s.log}, uploadInfo(b), nil
}
func (s *uploadStoreDouble) OpenStream(ctx context.Context, k store.Key) (store.ReadSeekCloser, int64, error) {
	s.event("OpenStream", k)
	if ctx.Err() != nil {
		return nil, 0, ctx.Err()
	}
	if f := s.faults["OpenStream"]; f.err != nil {
		return nil, 0, f.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blob == nil {
		return nil, 0, store.ErrNotFound
	}
	b := append([]byte{}, s.blob...)
	return uploadReadCloser{bytes.NewReader(b), s.log}, int64(len(b)), nil
}
func (s *uploadStoreDouble) Delete(ctx context.Context, k store.Key) error {
	s.event("Delete", k)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f := s.faults["Delete"]
	if f.err != nil && !f.applied {
		return f.err
	}
	s.mu.Lock()
	s.part = false
	s.ready = false
	s.staged = nil
	s.blob = nil
	s.mu.Unlock()
	if s.after != nil {
		s.after("Delete", k)
	}
	return f.err
}
func uploadFixture(t *testing.T) (*UploadCoordinator, *uploadSQLDouble, *uploadStoreDouble, *uploadEvents) {
	t.Helper()
	log := &uploadEvents{t: t}
	image, err := model.New(model.CreateInput{ID: uploadPtr(uploadImageID), DiskFormat: uploadPtr("raw"), ContainerFormat: uploadPtr("bare")}, "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sql := &uploadSQLDouble{log: log, image: image, faults: map[string]uploadSQLFault{}, failReadAt: map[int]bool{}, otherStates: map[string]model.DataState{}}
	fs := &uploadStoreDouble{log: log, faults: map[string]uploadStoreFault{}}
	u, err := newUploadCoordinator(uploadCatalogDouble{sql: sql}, sql, fs, UploadLimits{MaxBytes: 1024, MaxConcurrent: 2, ResolutionTimeout: time.Second, RecoveryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.RecoverUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	log.reset()
	return u, sql, fs, log
}
func uploadSeed(d *uploadSQLDouble, phase model.DataPhase) {
	info := uploadInfo(uploadPayload)
	d.image.Status = "saving"
	d.op = &model.DataOperation{ImageID: uploadImageID, AttemptID: oldUploadAttempt, Phase: phase, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if phase != model.DataReserved {
		d.op.Size = uploadPtr(info.Size)
		d.op.VirtualSize = uploadPtr(info.Size)
		d.op.Checksum = uploadPtr(info.Checksum)
		d.op.SHA512 = uploadPtr(info.SHA512)
	}
	if phase == model.DataCleanup || phase == model.DataCleaned {
		d.op.CleanupTarget = uploadPtr("queued")
	}
	if phase == model.DataCleaned {
		d.image.Status = "queued"
	}
	if phase == model.DataCommitted {
		d.image.Status = "active"
		d.image.Size = d.op.Size
		d.image.VirtualSize = d.op.VirtualSize
		d.image.Checksum = d.op.Checksum
		d.image.HashAlgorithm = uploadPtr("sha512")
		d.image.HashValue = d.op.SHA512
	}
}
