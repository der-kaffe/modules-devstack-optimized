package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

var _ repository.ImageDeletion = (*Repository)(nil)

const tombstoneDataSQL = "UPDATE images SET deleted=TRUE, deleted_at=?, status='deleted', updated_at=? WHERE id=? AND status=? AND deleted=FALSE AND protected=FALSE"
const delayedDataSQL = "UPDATE images SET deleted=TRUE, deleted_at=?, status='pending_delete', updated_at=? WHERE id=? AND status=? AND deleted=FALSE AND protected=FALSE"
const delayedOperationSQL = "UPDATE image_data_operations SET phase='cleaned', cleanup_target='deleted', last_error_code='deleted', updated_at=? WHERE image_id=? AND attempt_id=? AND phase=?"
const tombstoneOperationSQL = "UPDATE image_data_operations SET phase='cleanup', cleanup_target='deleted', last_error_code='deleted', updated_at=? WHERE image_id=? AND attempt_id=? AND phase=?"

func noCalculatedData(s model.DataState) bool {
	return s.Size == nil && s.VirtualSize == nil && s.Checksum == nil && s.HashAlgorithm == nil && s.HashValue == nil
}

func equalPointer[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func isTarget(o *model.DataOperation, target string) bool {
	return o.CleanupTarget != nil && *o.CleanupTarget == target
}

// Validate relationships in addition to scanOperation's column-level checks.
// Unknown or contradictory states are retained for reconciliation, not repaired.
func deletableData(s model.DataState) bool {
	o := s.Operation
	switch s.Status {
	case "queued":
		return noCalculatedData(s) && (o == nil || o.Phase == model.DataCleaned && isTarget(o, "queued"))
	case "killed":
		return noCalculatedData(s) && (o == nil || o.Phase == model.DataCleaned && isTarget(o, "killed"))
	case "saving":
		return noCalculatedData(s) && o != nil && ((o.Phase == model.DataReserved || o.Phase == model.DataStaged || o.Phase == model.DataPublished) && o.CleanupTarget == nil || o.Phase == model.DataCleanup && (isTarget(o, "queued") || isTarget(o, "killed")))
	case "uploading", "importing": // staged or importing: no data in the store yet (size is the staged size)
		return s.Checksum == nil && s.HashAlgorithm == nil && s.HashValue == nil && (o == nil || o.Phase == model.DataCleaned)
	case "active", "deactivated":
		return o != nil && o.Phase == model.DataCommitted && o.CleanupTarget == nil && equalPointer(s.Size, o.Size) && s.Size != nil && equalPointer(s.VirtualSize, o.VirtualSize) && equalPointer(s.Checksum, o.Checksum) && equalPointer(s.HashValue, o.SHA512) && s.HashAlgorithm != nil && *s.HashAlgorithm == "sha512"
	}
	return false
}

func (r *Repository) TombstoneData(ctx context.Context, id string, expectedAttempt *string, now time.Time, check func(model.Image) error) (out model.DataState, err error) {
	if canonical, e := model.CanonicalID(id); e != nil || canonical != id || check == nil {
		return out, model.Fail(model.Invalid, "Invalid deletion request.")
	}
	if expectedAttempt != nil {
		if e := transferIDs(id, *expectedAttempt); e != nil {
			return out, e
		}
	}
	err = withLockRetry(ctx, func() (e error) { out, e = r.tombstoneData(ctx, id, expectedAttempt, now, check); return e })
	return out, conflict(err)
}

func (r *Repository) tombstoneData(ctx context.Context, id string, expectedAttempt *string, now time.Time, check func(model.Image) error) (out model.DataState, err error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Same order as catalog PATCH/DELETE: image first, operation second.
	i, err := r.readImage(ctx, tx, id, true)
	if err != nil {
		return out, err
	}
	if err = check(i); err != nil {
		return out, err
	}
	if i.Protected {
		return out, model.Fail(model.Forbidden, "Protected images cannot be deleted.")
	}
	s := model.DataState{ImageID: i.ID, Status: i.Status, Size: i.Size, VirtualSize: i.VirtualSize, Checksum: i.Checksum, HashAlgorithm: i.HashAlgorithm, HashValue: i.HashValue, UpdatedAt: i.UpdatedAt}
	o, err := scanOperation(r.queryRow(ctx, tx, lockDataOperation, id))
	if err == nil {
		s.Operation = &o
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	} else if adopted, e := r.adoptable(ctx, tx, s); e != nil {
		return out, e
	} else if adopted != nil {
		if e = r.materialize(ctx, tx, adopted); e != nil {
			return out, e
		}
		o, s.Operation = *adopted, adopted
	}
	if i.ID != id || s.Operation != nil && s.Operation.ImageID != id || !deletableData(s) {
		return out, repository.ErrConflict
	}
	if expectedAttempt == nil && s.Operation != nil || expectedAttempt != nil && (s.Operation == nil || s.Operation.AttemptID != *expectedAttempt) {
		return out, repository.ErrConflict
	}
	at := dataTime(now, s)
	// delayed_delete: an image with data becomes pending_delete and its
	// active locations too (deleted, as image_location_update); the file
	// stays for glance-scrubber, so the data operation is already cleaned.
	if r.delayedDelete && s.Operation != nil && (i.Status == "active" || i.Status == "deactivated") {
		if err = r.ack(ctx, tx, delayedDataSQL, at, at, id, i.Status); err != nil {
			return out, err
		}
		if err = r.ack(ctx, tx, delayedOperationSQL, at, id, o.AttemptID, string(o.Phase)); err != nil {
			return out, err
		}
		if _, err = r.exec(ctx, tx, "UPDATE image_locations SET status='pending_delete', deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND deleted=0 AND status='active'", at, at, id); err != nil {
			return out, err
		}
		if err = r.destroyChildren(ctx, tx, id, at); err != nil {
			return out, err
		}
		target, code := "deleted", "deleted"
		o.Phase, o.CleanupTarget, o.LastErrorCode, o.UpdatedAt = model.DataCleaned, &target, &code, at
		s.Operation = &o
		s.Deleted, s.Status, s.UpdatedAt = true, "pending_delete", at
		out = s
		if tx.Commit() != nil {
			return out, repository.ErrCommitUncertain
		}
		return out, nil
	}
	if err = r.ack(ctx, tx, tombstoneDataSQL, at, at, id, i.Status); err != nil {
		return out, err
	}
	if s.Operation != nil {
		if err = r.ack(ctx, tx, tombstoneOperationSQL, at, id, o.AttemptID, string(o.Phase)); err != nil {
			return out, err
		}
		target, code := "deleted", "deleted"
		o.Phase, o.CleanupTarget, o.LastErrorCode, o.UpdatedAt = model.DataCleanup, &target, &code, at
		s.Operation = &o
	}
	if err = r.destroyChildren(ctx, tx, id, at); err != nil {
		return out, err
	}
	s.Deleted, s.Status, s.UpdatedAt = true, "deleted", at
	out = s
	if tx.Commit() != nil {
		return out, repository.ErrCommitUncertain
	}
	return out, nil
}
