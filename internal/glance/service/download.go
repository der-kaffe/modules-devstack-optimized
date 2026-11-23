package service

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
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
	// Sent reports the bytes written once the body was sent without a write
	// error (official image.send: INFO when it equals the requested count,
	// ERROR otherwise; nothing when the client went away). nil = no-op.
	Sent func(sent, requested int64)
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

// waitStream takes one of the MaxDownloadStreams slots, waiting in line (Go's
// channel wait queue is FIFO) like requests beyond official Glance's uwsgi
// workers. Waiting holds no reader capacity, and only after the authorization
// and visibility checks, so a 403/404 never waits. The client's context and
// StopAdmission/Close end the wait. The slot is returned when the body closes.
func (u *UploadCoordinator) waitStream(ctx context.Context) (func(), error) {
	if u.streams == nil {
		return func() {}, nil
	}
	select {
	case u.streams <- struct{}{}:
	case <-ctx.Done():
		return nil, downloadError(ctx, ctx.Err())
	case <-u.halt:
		return nil, ErrUploadNotReady
	}
	select {
	case <-u.halt: // admission stopped while this one was being granted
		<-u.streams
		return nil, ErrUploadNotReady
	default:
	}
	return sync.OnceFunc(func() { <-u.streams }), nil
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

func (u *UploadCoordinator) downloadImage(ctx context.Context, who auth.Identity, id string) (model.Image, error) {
	i, err := u.images.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return i, notFound(id)
	}
	if err != nil {
		return i, downloadError(ctx, err)
	}
	return i, u.downloadVisible(ctx, who, id, i)
}

// downloadViewer reads the image row and its data state in ONE read-only
// transaction (no properties/tags, which a download does not use).
type downloadViewer interface {
	DownloadView(context.Context, string) (model.Image, model.DataState, error)
}

// memberChecker is implemented by the MySQL repository (image_members).
type memberChecker interface {
	IsMember(ctx context.Context, imageID, project string) (bool, error)
}

func (u *UploadCoordinator) downloadVisible(ctx context.Context, who auth.Identity, id string, i model.Image) error {
	if i.ID != id {
		return ErrUploadReconciliation
	}
	if i.Status == "deleted" {
		return notFound(id)
	}
	// Same project/visibility policy as the metadata reader; identity and
	// reader/admin role were checked before any catalog or filesystem access.
	// Only a shared image of another project needs the membership lookup.
	if !who.HasRole("admin") && i.Owner != who.ProjectID && i.Visibility != "public" && i.Visibility != "community" {
		m, ok := u.images.(memberChecker)
		if !ok {
			m, ok = u.transfers.(memberChecker)
		}
		if i.Visibility != "shared" || !ok {
			return notFound(id)
		}
		member, err := m.IsMember(ctx, id, who.ProjectID)
		if err != nil {
			return downloadError(ctx, err)
		}
		if !member {
			return notFound(id)
		}
	}
	if overridden() {
		target := visibilityTarget(who, i)
		if i.Owner != who.ProjectID && i.Visibility == "shared" {
			target["member_id"] = who.ProjectID // a member: checked above
		}
		if err := Permit(who, "download_image", target, nil); err != nil {
			return err
		}
	}
	// As the original's image_data download.
	if i.Status == "deactivated" && !who.HasRole("admin") {
		return model.Fail(model.Forbidden, "The requested image has been deactivated. Image data download is forbidden.")
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
	return downloadStateValid(id, s)
}

func downloadStateValid(id string, s model.DataState) (model.DataState, error) {
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
	k := store.Key{ImageID: s.ImageID, AttemptID: o.AttemptID, Backend: s.Backend}
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
	if !overridden() && !who.HasRole("reader") && !who.HasRole("admin") {
		return nil, model.Fail(model.Forbidden, "Reading images requires reader or admin role.")
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return nil, err
	}
	var s model.DataState
	var owner string
	viewer, combined := u.transfers.(downloadViewer)
	if combined {
		i, state, err := viewer.DownloadView(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return nil, notFound(id)
		}
		if err != nil {
			return nil, downloadError(ctx, err)
		}
		if err = u.downloadVisible(ctx, who, id, i); err != nil {
			return nil, err
		}
		s, owner = state, i.Owner
	} else {
		i, err := u.downloadImage(ctx, who, id)
		if err != nil {
			return nil, err
		}
		owner = i.Owner
	}
	slot, err := u.waitStream(ctx)
	if err != nil {
		return nil, err
	}
	claimed, err := u.claimDownload()
	if err != nil {
		slot()
		return nil, err
	}
	leave := func() { claimed(); slot() }
	owned := false
	defer func() {
		if !owned {
			leave()
		}
	}()
	if combined {
		s, err = downloadStateValid(id, s)
	} else {
		s, err = u.downloadState(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	key, content, err := downloadReceipt(s)
	if err != nil {
		return nil, err
	}
	// The image cache serves a complete copy when it has one.
	var f store.ReadSeekCloser
	var size int64
	cached := false
	if u.cache != nil {
		if cf, ok := u.cache.open(id, content.Size); ok {
			f, size, cached = cf, content.Size, true
		}
	}
	if !cached {
		f, size, err = u.bytes.OpenStream(ctx, key)
	}
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
	sent := func(n, requested int64) {
		priority := notify.Info
		if n != requested {
			priority = notify.Error
		}
		u.notify("image.send", priority, notify.SendPayload(id, owner, who.ProjectID, who.UserID, n))
		if u.cache != nil && n == requested && requested == content.Size { // a complete download
			if cached {
				u.cache.hit(id)
			} else {
				u.cache.fill(id, content.Size, content.Checksum, func(ctx context.Context) (io.ReadCloser, int64, string, error) {
					f, _, err := u.bytes.OpenStream(ctx, key)
					return f, content.Size, content.Checksum, err
				}, nil)
			}
		}
	}
	return &Download{Body: &downloadBody{ReadSeekCloser: f, release: leave}, Size: content.Size, Checksum: content.Checksum, Sent: sent}, nil
}

// File exposes the underlying *os.File (filesystem store) so net/http can use
// sendfile; nil for other backends.
func (b *downloadBody) File() *os.File {
	f, _ := b.ReadSeekCloser.(*os.File)
	return f
}
