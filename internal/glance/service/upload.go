package service

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

var (
	ErrUploadNotReady       = errors.New("upload coordinator is not accepting transfers")
	ErrUploadBusy           = errors.New("upload capacity or image is busy")
	ErrUploadUnavailable    = errors.New("upload dependency unavailable")
	ErrUploadInterrupted    = errors.New("upload source interrupted")
	ErrUploadGone           = errors.New("upload lost to an image tombstone")
	ErrUploadReconciliation = errors.New("upload state requires reconciliation; evidence retained")
)

// MaxServerUploadBytes is a hard ceiling, not a default or a client setting.
const MaxServerUploadBytes int64 = 1 << 40

type UploadLimits struct {
	MaxBytes      int64
	MaxConcurrent int
	// MaxDownloadStreams > 0 queues downloads beyond it (see waitStream).
	MaxDownloadStreams int
	ResolutionTimeout  time.Duration
	RecoveryTimeout    time.Duration
}

type ImageReader interface {
	Get(context.Context, string) (model.Image, error)
}

// UploadCoordinator owns transfer activity on one exclusive store. It must finish
// RecoverUploads before admitting requests. Do not call the injected store or
// transfer mutators outside this owner. It does not own/close SQL or filesystem.
type UploadCoordinator struct {
	images                    ImageReader
	transfers                 repository.Transfers
	bytes                     store.Store
	limits                    UploadLimits
	now                       func() time.Time
	mu                        sync.Mutex
	ready, recovering, closed bool
	stopping                  bool
	changed                   chan struct{}
	active                    map[string]bool
	deleting                  map[string]bool
	readers                   int
	release                   func()
	streams                   chan struct{} // nil: downloads are not queued
	halt                      chan struct{} // closed once admission stops
	haltOnce                  sync.Once
}

// Unexported injection seam for tests with SQL/store doubles. Production obtains
// exclusive store ownership through NewUploadCoordinator, not this seam.
func newUploadCoordinator(images ImageReader, t repository.Transfers, s store.Store, limits UploadLimits) (*UploadCoordinator, error) {
	if images == nil || t == nil || s == nil || limits.MaxBytes <= 0 || limits.MaxBytes > MaxServerUploadBytes || limits.MaxConcurrent < 1 || limits.MaxConcurrent > 64 || limits.MaxDownloadStreams < 0 || limits.MaxDownloadStreams > limits.MaxConcurrent || limits.ResolutionTimeout <= 0 || limits.ResolutionTimeout > time.Minute || limits.RecoveryTimeout <= 0 || limits.RecoveryTimeout > 10*time.Minute {
		return nil, errors.New("invalid upload dependencies or server limits")
	}
	u := &UploadCoordinator{images: images, transfers: t, bytes: s, limits: limits, now: time.Now, active: map[string]bool{}, deleting: map[string]bool{}, changed: make(chan struct{}), halt: make(chan struct{})}
	if limits.MaxDownloadStreams > 0 {
		u.streams = make(chan struct{}, limits.MaxDownloadStreams)
	}
	return u, nil
}

// Close refuses to relinquish ownership while a writer/recovery is still active.
// The caller must cancel and unblock its source, then retry. No goroutine is left
// reading a source behind a supposedly completed upload.
func (u *UploadCoordinator) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	if u.recovering || len(u.active) > 0 || len(u.deleting) > 0 || u.readers > 0 {
		return ErrUploadBusy
	}
	u.closed = true
	u.ready = false
	u.haltLocked()
	u.notifyLocked()
	if u.release != nil {
		u.release()
	}
	return nil
}

func (u *UploadCoordinator) claim(id string) (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.stopping || !u.ready || u.recovering {
		return nil, ErrUploadNotReady
	}
	if u.active[id] || u.deleting[id] || len(u.active)+len(u.deleting)+u.readers >= u.limits.MaxConcurrent {
		return nil, ErrUploadBusy
	}
	u.active[id] = true
	return func() { u.mu.Lock(); delete(u.active, id); u.notifyLocked(); u.mu.Unlock() }, nil
}

func uploadStorageError(err error) error {
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrUploadInterrupted, store.ErrTooLarge, store.ErrSizeMismatch, store.ErrStoreFull, store.ErrIntegrity, store.ErrExists} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUploadUnavailable // unknown storage diagnostics are dependency failures
}

// Mark source failures before entering Stage: otherwise its error can originate
// either in Reader.Read or in filesystem write/fsync. Preserve only controlled
// errors; neither the source diagnostic nor a filesystem path reaches HTTP.
type uploadSource struct{ io.Reader }

func (s uploadSource) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	if err == nil || err == io.EOF {
		return n, err
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, store.ErrTooLarge, store.ErrSizeMismatch} {
		if errors.Is(err, known) {
			return n, known
		}
	}
	return n, ErrUploadInterrupted
}

func uploadCleanupCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, store.ErrStoreFull):
		return "store_full"
	case errors.Is(err, store.ErrTooLarge):
		return "size_limit"
	case errors.Is(err, store.ErrSizeMismatch):
		return "invalid_size"
	case errors.Is(err, store.ErrIntegrity):
		return "integrity"
	case errors.Is(err, ErrUploadUnavailable):
		return "recovery"
	default:
		return "interrupted"
	}
}

func uploadDBError(err error, id string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, repository.ErrNotFound) {
		return notFound(id)
	}
	if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrDuplicate) {
		return model.Fail(model.Conflict, "Image transfer conflicts with its current state.")
	}
	return ErrUploadUnavailable
}

func noImageData(s model.DataState) bool {
	return s.Size == nil && s.VirtualSize == nil && s.Checksum == nil && s.HashAlgorithm == nil && s.HashValue == nil
}
func sameNumber(a, b *int64) bool       { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func sameText(a *string, b string) bool { return a != nil && *a == b }

// readState discards any partial result returned with an error, including an
// uncertain read-transaction completion. Always use the authoritative primary.
func (u *UploadCoordinator) readState(ctx context.Context, id string) (model.DataState, error) {
	s, err := u.transfers.DataState(ctx, id)
	if err != nil || s.ImageID != id || s.Deleted != (s.Status == "deleted") {
		return model.DataState{}, ErrUploadReconciliation
	}
	if s.Operation != nil && (s.Operation.ImageID != id || s.Operation.Validate() != nil) {
		return model.DataState{}, ErrUploadReconciliation
	}
	return s, nil
}
func ownState(s model.DataState, key store.Key) error {
	if s.Deleted {
		return ErrUploadGone
	}
	if s.Operation == nil || s.Operation.AttemptID != key.AttemptID {
		return ErrUploadReconciliation
	}
	return nil
}
func receiptMatches(o *model.DataOperation, c model.DataContent) bool {
	return o != nil && o.Size != nil && *o.Size == c.Size && sameNumber(o.VirtualSize, c.VirtualSize) && sameText(o.Checksum, c.Checksum) && sameText(o.SHA512, c.SHA512)
}
func imageReceiptMatches(s model.DataState, c model.DataContent) bool {
	return s.Size != nil && *s.Size == c.Size && sameNumber(s.VirtualSize, c.VirtualSize) && sameText(s.Checksum, c.Checksum) && sameText(s.HashAlgorithm, "sha512") && sameText(s.HashValue, c.SHA512)
}
func committed(s model.DataState, key store.Key, c model.DataContent) bool {
	return ownState(s, key) == nil && s.Status == "active" && s.Operation.Phase == model.DataCommitted && receiptMatches(s.Operation, c) && imageReceiptMatches(s, c)
}
func committedBeforeTombstone(s model.DataState, key store.Key, c model.DataContent) bool {
	if !s.Deleted || s.Operation == nil || s.Operation.AttemptID != key.AttemptID || !receiptMatches(s.Operation, c) || !imageReceiptMatches(s, c) {
		return false
	}
	return s.Operation.Phase == model.DataCommitted || ((s.Operation.Phase == model.DataCleanup || s.Operation.Phase == model.DataCleaned) && sameText(s.Operation.CleanupTarget, "deleted"))
}

func (u *UploadCoordinator) resolution(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), u.limits.ResolutionTimeout)
}

// checkpoint resolves acknowledged and uncertain SQL writes explicitly. An exact
// expected phase/receipt is required; a merely plausible status is insufficient.
func (u *UploadCoordinator) checkpoint(ctx context.Context, key store.Key, phase model.DataPhase, c *model.DataContent, writeErr error) error {
	if writeErr != nil && !errors.Is(writeErr, repository.ErrCommitUncertain) {
		return uploadDBError(writeErr, key.ImageID)
	}
	resolved, cancel := u.resolution(ctx)
	defer cancel()
	s, err := u.readState(resolved, key.ImageID)
	if err != nil {
		return err
	}
	// A fresh authoritative queued/empty (or previously cleaned) state proves
	// that an uncertain reservation did not leave a live intent. Do not label it
	// an unknown outcome and never compensate an older attempt.
	if phase == model.DataReserved && errors.Is(writeErr, repository.ErrCommitUncertain) && !s.Deleted && s.Status == "queued" && noImageData(s) && (s.Operation == nil || s.Operation.Phase == model.DataCleaned && sameText(s.Operation.CleanupTarget, "queued")) {
		return ErrUploadUnavailable
	}
	// A tombstone after finalization must not retrospectively turn that upload
	// into Gone. Persisted image+attempt receipts prove the earlier commit; no
	// mutation, restoration or deletion is performed for this observation.
	if phase == model.DataCommitted && c != nil && committedBeforeTombstone(s, key, *c) {
		return nil
	}
	if err = ownState(s, key); err != nil {
		return err
	}
	if phase == model.DataCommitted {
		if c != nil && committed(s, key, *c) {
			return nil
		}
	} else if s.Status == "saving" && noImageData(s) && s.Operation.Phase == phase && (c == nil || receiptMatches(s.Operation, *c)) {
		return nil
	}
	// A clean earlier phase can be compensated, but never reported as success.
	if s.Status == "saving" && (s.Operation.Phase == model.DataReserved || s.Operation.Phase == model.DataStaged || s.Operation.Phase == model.DataPublished) {
		return ErrUploadUnavailable
	}
	return ErrUploadReconciliation
}

func (u *UploadCoordinator) authorized(ctx context.Context, who auth.Identity, id string) (model.Image, error) {
	if err := who.Check(u.now()); err != nil {
		return model.Image{}, err
	}
	if !who.HasRole("member") && !who.HasRole("admin") {
		return model.Image{}, model.Fail(model.Forbidden, "Uploading images requires member or admin role.")
	}
	i, err := u.images.Get(ctx, id)
	if err != nil {
		return i, uploadDBError(err, id)
	}
	if i.ID != id {
		return i, ErrUploadReconciliation
	}
	if err = writePolicy(who, i); err != nil {
		return i, err
	}
	return i, nil
}
func formats(i model.Image) error {
	if i.DiskFormat == nil || i.ContainerFormat == nil || *i.DiskFormat == "" || *i.ContainerFormat == "" {
		return model.Fail(model.Invalid, "Image formats must be defined before upload.")
	}
	// Reuse the domain's selected enums without generating a second image UUID.
	_, err := model.New(model.CreateInput{ID: &i.ID, DiskFormat: i.DiskFormat, ContainerFormat: i.ContainerFormat}, i.Owner, i.CreatedAt)
	return err
}

// Upload accepts no client attempt/root/cleanup target. src must support caller-
// managed cancellation/deadlines: an arbitrary blocked Reader cannot be stopped
// safely by abandoning a goroutine. All streaming buffers belong to store.Store.
func (u *UploadCoordinator) Upload(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64) (model.DataContent, error) {
	var zero model.DataContent
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return zero, err
	}
	i, err := u.authorized(ctx, who, id)
	if err != nil {
		return zero, err
	}
	if src == nil || declared != nil && *declared < 0 {
		return zero, model.Fail(model.Invalid, "Invalid upload source or declared size.")
	}
	var expected *int64
	if declared != nil {
		n := *declared
		expected = &n
		if n > u.limits.MaxBytes {
			return zero, store.ErrTooLarge
		}
	}
	if err = formats(i); err != nil {
		return zero, err
	}
	leave, err := u.claim(id)
	if err != nil {
		return zero, err
	}
	defer leave()
	before, err := u.readState(ctx, id)
	if err != nil {
		return zero, err
	}
	if before.Deleted {
		return zero, notFound(id)
	}
	if before.Operation != nil && before.Operation.Phase == model.DataCleanup {
		return zero, ErrUploadReconciliation
	}
	if i.Status != "queued" || before.Status != "queued" || !noImageData(before) {
		return zero, model.Fail(model.Conflict, "Image is not queued without content.")
	}
	if before.Operation != nil && (before.Operation.Phase != model.DataCleaned || !sameText(before.Operation.CleanupTarget, "queued")) {
		return zero, ErrUploadReconciliation
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	attempt, err := uuid.NewRandom()
	if err != nil {
		return zero, ErrUploadUnavailable
	}
	key := store.Key{ImageID: id, AttemptID: attempt.String()}
	_, reserveErr := u.transfers.ReserveData(ctx, id, key.AttemptID, expected, u.now())
	if reserveErr != nil && !errors.Is(reserveErr, repository.ErrCommitUncertain) {
		return zero, uploadDBError(reserveErr, id)
	}
	// Reservation may not have applied. Never clean an older/absent intention.
	if err = u.checkpoint(ctx, key, model.DataReserved, nil, reserveErr); err != nil {
		return zero, err
	}
	fail := func(cause error) (model.DataContent, error) {
		if errors.Is(cause, ErrUploadReconciliation) || errors.Is(cause, ErrUploadGone) {
			return zero, cause
		}
		if e := u.compensate(ctx, key, uploadCleanupCode(cause)); e != nil {
			return zero, errors.Join(cause, e)
		}
		return zero, cause
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	// Formats are mutable while queued. Re-read after reservation froze formats
	// in saving; do not use a stale pre-reservation format to derive virtual_size.
	current, err := u.authorized(ctx, who, id)
	if err != nil {
		return fail(err)
	}
	if current.Status != "saving" || current.Owner != i.Owner {
		return zero, ErrUploadReconciliation
	}
	if err = formats(current); err != nil {
		return fail(err)
	}
	info, err := u.bytes.Stage(ctx, key, uploadSource{src}, u.limits.MaxBytes, expected)
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			return zero, ErrUploadReconciliation
		}
		return fail(uploadStorageError(err))
	}
	content := model.DataContent{Size: info.Size, Checksum: info.Checksum, SHA512: info.SHA512}
	if *current.DiskFormat == "raw" {
		n := info.Size
		content.VirtualSize = &n
	}
	// QCOW2 and other formats without a verified inspector remain unknown (null).
	if err = content.Validate(expected); err != nil {
		return fail(store.ErrIntegrity)
	}
	if content.Size > u.limits.MaxBytes {
		return fail(store.ErrTooLarge)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	err = u.checkpoint(ctx, key, model.DataStaged, &content, u.transfers.StageData(ctx, id, key.AttemptID, content, u.now()))
	if err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	publishErr := u.bytes.Publish(ctx, key, info)
	// Even a successful filesystem call must not override a newer SQL fence.
	resolved, cancel := u.resolution(ctx)
	s, stateErr := u.readState(resolved, id)
	if stateErr == nil {
		stateErr = ownState(s, key)
	}
	if stateErr == nil && (s.Status != "saving" || s.Operation.Phase != model.DataStaged || !receiptMatches(s.Operation, content)) {
		stateErr = ErrUploadReconciliation
	}
	if stateErr != nil {
		cancel()
		return zero, stateErr
	}
	// A successful Publish already established these bytes (official Glance
	// never re-reads an upload): confirm presence and size only. A failed
	// Publish keeps the full verification before any compensation.
	var blobErr error
	if publishErr == nil {
		blobErr = u.committedBlobPresent(resolved, key, info.Size)
	} else {
		blobErr = u.verifyBlob(resolved, key, info)
	}
	cancel()
	if publishErr != nil {
		// Matching bytes alone cannot prove directory fsync after Publish failed.
		// Probe integrity, then compensate only a confirmed, still-uncommitted key.
		if blobErr != nil && !errors.Is(blobErr, store.ErrNotFound) {
			return zero, ErrUploadReconciliation
		}
		return fail(uploadStorageError(publishErr))
	}
	if blobErr != nil {
		return zero, ErrUploadReconciliation
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	err = u.checkpoint(ctx, key, model.DataPublished, &content, u.transfers.PublishData(ctx, id, key.AttemptID, u.now()))
	if err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	err = u.checkpoint(ctx, key, model.DataCommitted, &content, u.transfers.CommitData(ctx, id, key.AttemptID, u.now()))
	if err != nil {
		return fail(err)
	}
	return content, nil
}

// committedBlobPresent is the startup check for already committed images:
// content exists with its committed size. Official Glance does no startup scan
// at all; re-hashing every image made startup O(total bytes) (was verifyBlob).
func (u *UploadCoordinator) committedBlobPresent(ctx context.Context, key store.Key, size int64) error {
	f, actual, err := u.bytes.OpenStream(ctx, key)
	if err != nil {
		if f != nil {
			_ = f.Close()
		}
		return err
	}
	if f == nil {
		return store.ErrIntegrity
	}
	closeErr := f.Close()
	if actual != size {
		return store.ErrIntegrity
	}
	return closeErr
}

func (u *UploadCoordinator) verifyBlob(ctx context.Context, key store.Key, expected store.Info) error {
	f, actual, err := u.bytes.Open(ctx, key)
	if err != nil {
		if f != nil {
			_ = f.Close()
		}
		return err
	}
	if f == nil {
		return store.ErrIntegrity
	}
	closeErr := f.Close()
	if actual != expected {
		return store.ErrIntegrity
	}
	return closeErr
}
