package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/format"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

var (
	ErrUploadNotReady    = errors.New("upload coordinator is not accepting transfers")
	ErrUploadBusy        = errors.New("upload capacity or image is busy")
	ErrUploadUnavailable = errors.New("upload dependency unavailable")
	ErrUploadInterrupted = errors.New("upload source interrupted")
	// errStorageQuotaFull: the limiting reader passed user_storage_quota.
	errStorageQuotaFull     = errors.New("storage quota full")
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
	// Inspect: [image_format] content inspection of bare uploads.
	Inspect          bool
	RequireFormat    bool
	GPTNonfatalCheck []string
	// StorageQuota is user_storage_quota in bytes (0: none).
	StorageQuota int64
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
	quota                     QuotaEnforcer
	defaultBackend            string
	backends                  map[string]bool
	backendOrder              []string // enabled_backends order
	cache                     *ImageCache
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
	notifier                  notify.Notifier
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
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrUploadInterrupted, errStorageQuotaFull, store.ErrTooLarge, store.ErrSizeMismatch, store.ErrStoreFull, store.ErrIntegrity, store.ErrExists} {
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
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, store.ErrTooLarge, store.ErrSizeMismatch, errStorageQuotaFull} {
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

// uploadFailureMessage mirrors the texts of ImageProxy.set_data's ERROR
// image.upload. The detail is a fixed class, never a path, SQL or broker text.
func uploadFailureMessage(id string, err error) string {
	switch {
	case errors.Is(err, store.ErrStoreFull), errors.Is(err, store.ErrTooLarge):
		return "Image storage media is full: image size or storage limit exceeded"
	case errors.Is(err, store.ErrSizeMismatch), errors.Is(err, store.ErrInvalidSize), errors.Is(err, store.ErrIntegrity), errors.Is(err, ErrUploadInterrupted):
		return "Cannot save data for image " + id + ": invalid or interrupted image payload"
	case errors.Is(err, ErrUploadGone), errors.Is(err, repository.ErrNotFound):
		return "Image " + id + " could not be found after upload. The image may have been deleted during the upload: image deleted"
	}
	var domain *model.Error
	if errors.As(err, &domain) && domain.Kind == model.Conflict {
		return "Unable to upload duplicate image data for image " + id + ": image state conflict"
	}
	return "Failed to upload image data for image " + id + " due to internal error: transfer failed"
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
	return checkedState(s, err, id)
}
func checkedState(s model.DataState, err error, id string) (model.DataState, error) {
	// A deleted row is "deleted", or "pending_delete" with delayed_delete.
	if err != nil || s.ImageID != id || s.Deleted != (s.Status == "deleted" || s.Status == "pending_delete") {
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
	return ownState(s, key) == nil && (s.Status == "active" || s.Status == "deactivated") && s.Operation.Phase == model.DataCommitted && receiptMatches(s.Operation, c) && imageReceiptMatches(s, c)
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
// dataFinisher is implemented by the MySQL repository: the single final
// commit of an upload (see FinishData).
type dataFinisher interface {
	FinishData(ctx context.Context, id, attempt string, c model.DataContent, now time.Time) error
}

func (u *UploadCoordinator) checkpoint(ctx context.Context, key store.Key, phase model.DataPhase, c *model.DataContent, writeErr error) error {
	if writeErr != nil && !errors.Is(writeErr, repository.ErrCommitUncertain) {
		return uploadDBError(writeErr, key.ImageID)
	}
	// An acknowledged write is its own proof: each transition is one
	// conditional UPDATE whose affected rows were checked, so only an
	// uncertain commit is read back to learn what happened.
	if writeErr == nil {
		return nil
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

// authorized: task is the import holding the image's import lock ("" for a
// client upload). A client upload never writes an image being imported.
func (u *UploadCoordinator) authorized(ctx context.Context, who auth.Identity, id, task string) (model.Image, error) {
	if err := who.Check(u.now()); err != nil {
		return model.Image{}, err
	}
	if !overridden() && !who.HasRole("member") && !who.HasRole("admin") {
		return model.Image{}, model.Fail(model.Forbidden, "Uploading images requires member or admin role.")
	}
	i, err := u.images.Get(ctx, id)
	if err != nil {
		return i, uploadDBError(err, id)
	}
	if i.ID != id {
		return i, ErrUploadReconciliation
	}
	if err = writePolicy(who, i, "upload_image"); err != nil {
		return i, err
	}
	if i.Properties[model.ImportTaskProperty] != task {
		return i, model.Fail(model.Conflict, "Image "+id+" is being imported.")
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
// SetNotifier makes transfers emit image.prepare/upload/activate/update,
// image.send and image.delete as official Glance does. nil means noop.
func (u *UploadCoordinator) SetNotifier(n notify.Notifier) { u.notifier = notify.OrNoop(n) }

func (u *UploadCoordinator) notify(eventType, priority string, payload any) {
	if u.notifier != nil {
		u.notifier.Notify(eventType, priority, payload)
	}
}

// Upload emits, like ImageProxy.set_data and the repo saves around it:
// image.update (saving) and image.prepare before the bytes; then image.upload,
// image.activate and image.update (active), or an ERROR image.upload whose
// payload is a message, as official Glance.
func (u *UploadCoordinator) Upload(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64) (model.DataContent, error) {
	return u.write(ctx, who, value, src, declared, "", "")
}

// UploadTo is Upload into one of enabled_backends (X-Image-Meta-Store).
func (u *UploadCoordinator) UploadTo(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64, backend string) (model.DataContent, error) {
	b, err := u.backendKey(backend)
	if err != nil {
		return model.DataContent{}, err
	}
	return u.write(ctx, who, value, src, declared, "", b)
}

// SetBackends enables multi-store: the default store's name and the others.
func (u *UploadCoordinator) SetBackends(def string, names []string) {
	u.defaultBackend, u.backends, u.backendOrder = def, map[string]bool{}, append([]string(nil), names...)
	for _, n := range names {
		u.backends[n] = true
	}
}

// backendKey maps a store name to its store key ("" for the default).
func (u *UploadCoordinator) backendKey(name string) (string, error) {
	if name == "" || name == u.defaultBackend {
		return "", nil
	}
	if !u.backends[name] {
		return "", model.Fail(model.Invalid, "Unknown scheme '"+name+"' found in URI")
	}
	return name, nil
}

// ImportUpload writes an import's data (task holds the image's import lock)
// through the same reserve/stage/publish/commit path as a client upload.
func (u *UploadCoordinator) ImportUpload(ctx context.Context, who auth.Identity, id string, src io.Reader, declared *int64, task string) (model.DataContent, error) {
	if task == "" {
		return model.DataContent{}, model.Fail(model.Invalid, "Import without a task.")
	}
	return u.write(ctx, who, id, src, declared, task, "")
}

// ImportUploadTo is ImportUpload into a backend.
func (u *UploadCoordinator) ImportUploadTo(ctx context.Context, who auth.Identity, id string, src io.Reader, declared *int64, task, backend string) (model.DataContent, error) {
	b, err := u.backendKey(backend)
	if err != nil {
		return model.DataContent{}, err
	}
	return u.write(ctx, who, id, src, declared, task, b)
}

func (u *UploadCoordinator) write(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64, task, backend string) (model.DataContent, error) {
	var prepared *model.Image
	content, err := u.upload(ctx, who, value, src, declared, &prepared, task, backend)
	if prepared == nil {
		return content, err
	}
	if err != nil {
		u.notify("image.upload", notify.Error, uploadFailureMessage(prepared.ID, err))
		return content, err
	}
	active := *prepared
	active.Status, active.UpdatedAt = "active", u.now()
	size, md5, sha := content.Size, content.Checksum, content.SHA512
	algorithm := "sha512"
	active.Size, active.VirtualSize, active.Checksum, active.HashAlgorithm, active.HashValue = &size, content.VirtualSize, &md5, &algorithm, &sha
	u.notify("image.upload", notify.Info, notify.UploadPayload(active))
	u.notify("image.activate", notify.Info, notify.ImagePayload(active))
	u.notify("image.update", notify.Info, notify.ImagePayload(active))
	return content, nil
}

func (u *UploadCoordinator) upload(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64, prepared **model.Image, task, backend string) (model.DataContent, error) {
	var zero model.DataContent
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return zero, err
	}
	i, err := u.authorized(ctx, who, id, task)
	if err != nil {
		return zero, err
	}
	if u.quota != nil && task == "" { // an import was checked when accepted
		if err = u.quota.EnforceUpload(ctx, who.ProjectID); err != nil {
			return zero, err
		}
	}
	if src == nil || declared != nil && *declared < 0 {
		return zero, model.Fail(model.Invalid, "Invalid upload source or declared size.")
	}
	// The original's user_storage_quota (check_quota + LimitingReader),
	// only without Keystone limits.
	if u.limits.StorageQuota > 0 && u.quota == nil {
		remaining, err := u.storageRemaining(ctx, i.Owner)
		if err != nil {
			return zero, err
		}
		if declared != nil && *declared > remaining || declared == nil && remaining <= 0 {
			size := "None"
			if declared != nil {
				size = strconv.FormatInt(*declared, 10)
			}
			return zero, storageQuotaFull(fmt.Sprintf("The size of the data %s will exceed the limit. %d bytes remaining.", size, remaining))
		}
		src = &quotaReader{r: src, left: remaining}
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
	key := store.Key{ImageID: id, AttemptID: attempt.String(), Backend: backend}
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
	current, err := u.authorized(ctx, who, id, task)
	if err != nil {
		return fail(err)
	}
	if current.Status != "saving" || current.Owner != i.Owner {
		return zero, ErrUploadReconciliation
	}
	if err = formats(current); err != nil {
		return fail(err)
	}
	*prepared = &current
	u.notify("image.update", notify.Info, notify.ImagePayload(current))
	u.notify("image.prepare", notify.Info, notify.UploadPayload(current))
	// As the original's set_data: bare uploads are inspected in flight
	// (format_inspector over the same stream the hashes and file read).
	var inspector *format.Wrapper
	if u.limits.Inspect && current.ContainerFormat != nil && *current.ContainerFormat == "bare" {
		inspector = format.NewWrapper()
		src = io.TeeReader(src, inspector)
	}
	info, err := u.bytes.Stage(ctx, key, uploadSource{src}, u.limits.MaxBytes, expected)
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			return zero, ErrUploadReconciliation
		}
		if errors.Is(err, errStorageQuotaFull) {
			return fail(storageQuotaFull("The size of the data %(image_size)s will exceed the limit. %(remaining)s bytes remaining."))
		}
		return fail(uploadStorageError(err))
	}
	content := model.DataContent{Size: info.Size, Checksum: info.Checksum, SHA512: info.SHA512}
	if inspector != nil {
		inspector.Finish()
		virtual, err := inspector.Check(*current.DiskFormat, u.limits.RequireFormat, u.limits.GPTNonfatalCheck)
		var invalid *format.ErrInvalidImageData
		if errors.As(err, &invalid) {
			// InvalidImageData: 415, the image is restored to queued.
			return fail(model.Fail(model.Unsupported, invalid.Msg))
		}
		if virtual > 0 {
			content.VirtualSize = &virtual
		}
	} else if !u.limits.Inspect && *current.DiskFormat == "raw" {
		n := info.Size
		content.VirtualSize = &n
	}
	if err = content.Validate(expected); err != nil {
		return fail(store.ErrIntegrity)
	}
	if content.Size > u.limits.MaxBytes {
		return fail(store.ErrTooLarge)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	if f, ok := u.transfers.(dataFinisher); ok {
		// As the original (image_repo.save after glance_store.add): the file
		// is written at its final name, so after verifying it one commit
		// stores receipt, location and status active.
		if err = u.bytes.Publish(ctx, key, info); err != nil {
			return fail(uploadStorageError(err))
		}
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		if err = u.checkpoint(ctx, key, model.DataCommitted, &content, u.finish(f, ctx, id, key, content)); err != nil {
			return fail(err)
		}
		return content, nil
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

// storageUsage is the repository's project usage (user_get_storage_usage).
type storageUsage interface {
	ProjectUsage(ctx context.Context, project string) (model.Usage, error)
}

func (u *UploadCoordinator) storageRemaining(ctx context.Context, owner string) (int64, error) {
	r, ok := u.transfers.(storageUsage)
	if !ok {
		return u.limits.StorageQuota, nil
	}
	usage, err := r.ProjectUsage(ctx, owner)
	if err != nil {
		return 0, uploadDBError(err, "")
	}
	return u.limits.StorageQuota - usage.SizeTotal, nil
}

func storageQuotaFull(detail string) error {
	return model.Fail(model.LimitExceeded, "Image exceeds the storage quota: "+detail)
}

// quotaReader is the original's LimitingReader with StorageQuotaFull,
// raised without arguments (its message keeps the placeholders).
type quotaReader struct {
	r    io.Reader
	left int64
}

func (q *quotaReader) Read(p []byte) (int, error) {
	n, err := q.r.Read(p)
	q.left -= int64(n)
	if q.left < 0 {
		return n, errStorageQuotaFull
	}
	return n, err
}

// backendFinisher is FinishData into a backend (the MySQL repository).
type backendFinisher interface {
	FinishDataTo(ctx context.Context, id, attempt string, c model.DataContent, now time.Time, backend string) error
}

func (u *UploadCoordinator) finish(f dataFinisher, ctx context.Context, id string, key store.Key, c model.DataContent) error {
	if b, ok := f.(backendFinisher); ok && key.Backend != "" {
		return b.FinishDataTo(ctx, id, key.AttemptID, c, u.now(), key.Backend)
	}
	return f.FinishData(ctx, id, key.AttemptID, c, u.now())
}
