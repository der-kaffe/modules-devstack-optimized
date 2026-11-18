package service

import (
	"context"
	"errors"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// compensate is called only after this writer has stopped all filesystem I/O,
// or during exclusive startup with no admitted writers. It never cleans tombstones.
func (u *UploadCoordinator) compensate(ctx context.Context, key store.Key, code string) error {
	ctx, cancel := u.resolution(ctx)
	defer cancel()
	s, err := u.readState(ctx, key.ImageID)
	if err != nil {
		return err
	}
	if err = ownState(s, key); err != nil {
		return err
	}
	if s.Status != "saving" || !noImageData(s) {
		return ErrUploadReconciliation
	}
	switch s.Operation.Phase {
	case model.DataReserved, model.DataStaged, model.DataPublished:
		err = u.transfers.BeginDataCleanup(ctx, key.ImageID, key.AttemptID, "queued", &code, u.now())
		if err != nil && !errors.Is(err, repository.ErrCommitUncertain) {
			return ErrUploadReconciliation
		}
	case model.DataCleanup:
		if !sameText(s.Operation.CleanupTarget, "queued") {
			return ErrUploadReconciliation
		}
	default:
		return ErrUploadReconciliation
	}
	s, err = u.readState(ctx, key.ImageID)
	if err != nil {
		return err
	}
	if err = ownState(s, key); err != nil {
		return err
	}
	if s.Status != "saving" || s.Operation.Phase != model.DataCleanup || !sameText(s.Operation.CleanupTarget, "queued") || !noImageData(s) {
		return ErrUploadReconciliation
	}
	// Only this confirmed cleanup fence permits exact-key deletion. SQL locks
	// ended before Delete starts; a finalizer can no longer commit this attempt.
	if u.bytes.Delete(ctx, key) != nil {
		return ErrUploadReconciliation
	}
	s, err = u.readState(ctx, key.ImageID)
	if err != nil {
		return err
	}
	if err = ownState(s, key); err != nil {
		return err
	}
	if s.Status != "saving" || s.Operation.Phase != model.DataCleanup || !sameText(s.Operation.CleanupTarget, "queued") {
		return ErrUploadReconciliation
	}
	err = u.transfers.CompleteDataCleanup(ctx, key.ImageID, key.AttemptID, u.now())
	if err != nil && !errors.Is(err, repository.ErrCommitUncertain) {
		return ErrUploadReconciliation
	}
	s, err = u.readState(ctx, key.ImageID)
	if err != nil {
		return err
	}
	if err = ownState(s, key); err != nil {
		return err
	}
	if s.Status != "queued" || !noImageData(s) || s.Operation.Phase != model.DataCleaned || !sameText(s.Operation.CleanupTarget, "queued") {
		return ErrUploadReconciliation
	}
	return nil
}

type UploadRecoveryReport struct{ Examined, Cleaned, Kept int }

// RecoverUploads is startup-only and enables uploads only on complete success.
// The exclusive filesystem owner plus the lifecycle gate, not age or TTL, is the
// proof of writer quiescence. Running/closed coordinators cannot invoke recovery.
// Cleanup is allowed only for a confirmed destination and matching current key.
func (u *UploadCoordinator) RecoverUploads(ctx context.Context) (report UploadRecoveryReport, result error) {
	u.mu.Lock()
	if u.closed || u.stopping || u.ready || u.recovering || len(u.active) != 0 || len(u.deleting) != 0 || u.readers != 0 {
		u.mu.Unlock()
		return report, ErrUploadNotReady
	}
	u.recovering = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.recovering = false
		if result == nil && !u.closed && !u.stopping {
			u.ready = true
		}
		u.notifyLocked()
		u.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, u.limits.RecoveryTimeout)
	defer cancel()
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		candidates, err := u.transfers.ListDataOperations(ctx, after, 100)
		if err != nil || len(candidates) > 100 {
			return report, ErrUploadReconciliation
		}
		for _, candidate := range candidates {
			if err = ctx.Err(); err != nil {
				return report, err
			}
			if candidate.Validate() != nil || candidate.ImageID <= after {
				return report, ErrUploadReconciliation
			}
			after = candidate.ImageID
			report.Examined++
			s, err := u.readState(ctx, candidate.ImageID)
			if err != nil {
				return report, err
			}
			key := store.Key{ImageID: candidate.ImageID, AttemptID: candidate.AttemptID}
			if s.Operation == nil || s.Operation.AttemptID != key.AttemptID {
				return report, ErrUploadReconciliation
			}
			if s.Deleted {
				switch {
				case cleanupFence(s, key, "deleted", model.DataCleanup):
					if err = u.finishCleanup(ctx, s, "deleted"); err != nil {
						return report, err
					}
					report.Cleaned++
				case cleanupFence(s, key, "deleted", model.DataCleaned):
					report.Kept++
				default:
					return report, ErrUploadReconciliation
				}
				continue
			}
			if err = ownState(s, key); err != nil {
				return report, err
			}
			switch s.Operation.Phase {
			case model.DataCommitted:
				o := s.Operation
				if o.CleanupTarget != nil {
					return report, ErrUploadReconciliation
				}
				content := model.DataContent{Size: *o.Size, VirtualSize: o.VirtualSize, Checksum: *o.Checksum, SHA512: *o.SHA512}
				if !committed(s, key, content) {
					return report, ErrUploadReconciliation
				}
				if u.committedBlobPresent(ctx, key, content.Size) != nil {
					return report, ErrUploadReconciliation
				}
				report.Kept++
			case model.DataCleaned:
				if !noImageData(s) || !(s.Status == "queued" && sameText(s.Operation.CleanupTarget, "queued") || s.Status == "killed" && sameText(s.Operation.CleanupTarget, "killed")) {
					return report, ErrUploadReconciliation
				}
				report.Kept++
			case model.DataReserved, model.DataStaged, model.DataPublished, model.DataCleanup:
				if s.Operation.Phase != model.DataCleanup && s.Operation.CleanupTarget != nil {
					return report, ErrUploadReconciliation
				}
				if cleanupFence(s, key, "killed", model.DataCleanup) {
					if err = u.finishCleanup(ctx, s, "killed"); err != nil {
						return report, err
					}
					report.Cleaned++
					continue
				}
				if err = u.compensate(ctx, key, "recovery"); err != nil {
					return report, err
				}
				report.Cleaned++
			default:
				return report, ErrUploadReconciliation
			}
		}
		if len(candidates) < 100 {
			return report, ctx.Err()
		}
	}
}
