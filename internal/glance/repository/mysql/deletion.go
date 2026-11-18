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
	case "active":
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
	defer func() { err = conflict(err) }()
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
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
	o, err := scanOperation(tx.QueryRowContext(ctx, lockDataOperation, id))
	if err == nil {
		s.Operation = &o
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if i.ID != id || s.Operation != nil && s.Operation.ImageID != id || !deletableData(s) {
		return out, repository.ErrConflict
	}
	if expectedAttempt == nil && s.Operation != nil || expectedAttempt != nil && (s.Operation == nil || s.Operation.AttemptID != *expectedAttempt) {
		return out, repository.ErrConflict
	}
	at := dataTime(now, s)
	if err = acknowledged(ctx, tx, tombstoneDataSQL, at, at, id, i.Status); err != nil {
		return out, err
	}
	if s.Operation != nil {
		if err = acknowledged(ctx, tx, tombstoneOperationSQL, at, id, o.AttemptID, string(o.Phase)); err != nil {
			return out, err
		}
		target, code := "deleted", "deleted"
		o.Phase, o.CleanupTarget, o.LastErrorCode, o.UpdatedAt = model.DataCleanup, &target, &code, at
		s.Operation = &o
	}
	s.Deleted, s.Status, s.UpdatedAt = true, "deleted", at
	out = s
	if tx.Commit() != nil {
		return out, repository.ErrCommitUncertain
	}
	return out, nil
}
