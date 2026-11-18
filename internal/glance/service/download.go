package service

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

var (
	ErrNoImageContent      = errors.New("image has no confirmed content")
	ErrBlobMissing         = errors.New("confirmed image content is missing")
	ErrBlobCorrupt         = errors.New("confirmed image content is corrupt")
	ErrDownloadUnavailable = errors.New("download dependency unavailable")
)

// Download exposes only a verified descriptor and public response metadata.
// The caller owns Body and must close it, including on seek/write/cancel errors.
type Download struct {
	Body     store.ReadSeekCloser
	Size     int64
	Checksum string
}

// MaxUploadBytes is immutable server policy, never derived from a request.
func (u *UploadCoordinator) MaxUploadBytes() int64 { return u.limits.MaxBytes }

func (u *UploadCoordinator) claimDownload() (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.stopping || !u.ready || u.recovering {
		return nil, ErrUploadNotReady
	}
	if len(u.active)+len(u.deleting)+u.readers >= u.limits.MaxConcurrent {
		return nil, ErrUploadBusy
	}
	u.readers++
	return func() { u.mu.Lock(); u.readers--; u.notifyLocked(); u.mu.Unlock() }, nil
}

type downloadBody struct {
	store.ReadSeekCloser
	once    sync.Once
	release func()
	err     error
}

func (b *downloadBody) Close() error {
	b.once.Do(func() { b.err = b.ReadSeekCloser.Close(); b.release() })
	return b.err
}

func (u *UploadCoordinator) downloadImage(ctx context.Context, who auth.Identity, id string) error {
	i, err := u.images.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return notFound(id)
	}
	if err != nil {
		return downloadError(ctx, err)
	}
	if i.ID != id {
		return ErrUploadReconciliation
	}
	if i.Status == "deleted" {
		return notFound(id)
	}
	// Same project/visibility policy as the B20 metadata reader; identity and
	// reader/admin role were checked before any catalog or filesystem access.
	if !who.HasRole("admin") && i.Owner != who.ProjectID && i.Visibility != "public" && i.Visibility != "community" {
		return notFound(id)
	}
	return nil
}

func downloadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrDownloadUnavailable
}

func (u *UploadCoordinator) downloadState(ctx context.Context, id string) (model.DataState, error) {
	s, err := u.transfers.DataStateSnapshot(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return s, notFound(id)
	}
	if err != nil {
		return model.DataState{}, downloadError(ctx, err)
	}
	if s.ImageID != id || s.Deleted != (s.Status == "deleted") || s.Operation != nil && (s.Operation.ImageID != id || s.Operation.Validate() != nil) {
		return model.DataState{}, ErrUploadReconciliation
	}
	if s.Deleted {
		return s, notFound(id)
	}
	return s, nil
}

func absentContent(s model.DataState) bool {
	if !noImageData(s) {
		return false
	}
	o := s.Operation
	switch s.Status {
	case "queued":
		return o == nil || o.Phase == model.DataCleaned && sameText(o.CleanupTarget, "queued")
	case "saving":
		return o != nil && (o.Phase == model.DataReserved || o.Phase == model.DataStaged || o.Phase == model.DataPublished || o.Phase == model.DataCleanup && sameText(o.CleanupTarget, "queued"))
	case "killed":
		return o == nil || o.Phase == model.DataCleaned && sameText(o.CleanupTarget, "killed")
	}
	return false
}

func downloadReceipt(s model.DataState) (store.Key, model.DataContent, error) {
	if absentContent(s) {
		return store.Key{}, model.DataContent{}, ErrNoImageContent
	}
	o := s.Operation
	if o == nil || o.Size == nil || o.Checksum == nil || o.SHA512 == nil {
		return store.Key{}, model.DataContent{}, ErrUploadReconciliation
	}
	c := model.DataContent{Size: *o.Size, VirtualSize: o.VirtualSize, Checksum: *o.Checksum, SHA512: *o.SHA512}
	k := store.Key{ImageID: s.ImageID, AttemptID: o.AttemptID}
	if !committed(s, k, c) || c.Validate(o.ExpectedSize) != nil {
		return k, c, ErrUploadReconciliation
	}
	return k, c, nil
}

// OpenDownload shares the exclusive owner's startup recovery gate and bounded
// transfer capacity. All SQL calls finish before streaming. As official Glance
// (glance_store) does, content is served without re-hashing: integrity was
// established when the upload computed and committed its hashes. The physical
// size must still match the committed receipt. GET never repairs, deletes or
// changes image state.
func (u *UploadCoordinator) OpenDownload(ctx context.Context, who auth.Identity, value string) (*Download, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := who.Check(u.now()); err != nil {
		return nil, err
	}
	if !who.HasRole("reader") && !who.HasRole("admin") {
		return nil, model.Fail(model.Forbidden, "Reading images requires reader or admin role.")
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return nil, err
	}
	if err = u.downloadImage(ctx, who, id); err != nil {
		return nil, err
	}
	leave, err := u.claimDownload()
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			leave()
		}
	}()
	s, err := u.downloadState(ctx, id)
	if err != nil {
		return nil, err
	}
	key, content, err := downloadReceipt(s)
	if err != nil {
		return nil, err
	}
	f, size, err := u.bytes.OpenStream(ctx, key)
	if err != nil {
		if f != nil {
			_ = f.Close()
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, downloadError(ctx, err)
		}
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, ErrBlobMissing
		case errors.Is(err, store.ErrIntegrity):
			return nil, ErrBlobCorrupt
		default:
			return nil, downloadError(ctx, err)
		}
	}
	if f == nil || size != content.Size {
		if f != nil {
			_ = f.Close()
		}
		return nil, ErrBlobCorrupt
	}
	// Opening is now immediate, so the window that once justified re-reading
	// visibility and SQL fencing after a full hash no longer exists. A later
	// deletion may unlink the name; this open descriptor stays valid, as in
	// official Glance.
	if err = ctx.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}
	owned = true
	return &Download{Body: &downloadBody{ReadSeekCloser: f, release: leave}, Size: content.Size, Checksum: content.Checksum}, nil
}

// File exposes the underlying *os.File (filesystem store) so net/http can use
// sendfile; nil for other backends.
func (b *downloadBody) File() *os.File {
	f, _ := b.ReadSeekCloser.(*os.File)
	return f
}
