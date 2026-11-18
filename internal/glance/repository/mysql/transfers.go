package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	driver "github.com/go-sql-driver/mysql"
)

var _ repository.Transfers = (*Repository)(nil)

const dataColumns = "image_id, attempt_id, phase, expected_size, size, virtual_size, checksum, os_hash_value, cleanup_target, last_error_code, created_at, updated_at"
const readDataImage = "SELECT id, status, deleted, size, virtual_size, checksum, os_hash_algo, os_hash_value, updated_at FROM images WHERE id=?"
const readDataOperation = "SELECT " + dataColumns + " FROM image_data_operations WHERE image_id=?"
const lockDataImage = readDataImage + " FOR UPDATE"
const lockDataOperation = readDataOperation + " FOR UPDATE"

func transferIDs(image, attempt string) error {
	for _, v := range []string{image, attempt} {
		id, err := model.CanonicalID(v)
		if err != nil || id != v {
			return model.Fail(model.Invalid, "Canonical transfer UUIDs required.")
		}
	}
	return nil
}

func scanOperation(row scanner) (model.DataOperation, error) {
	var o model.DataOperation
	err := row.Scan(&o.ImageID, &o.AttemptID, &o.Phase, &o.ExpectedSize, &o.Size, &o.VirtualSize, &o.Checksum, &o.SHA512, &o.CleanupTarget, &o.LastErrorCode, &o.CreatedAt, &o.UpdatedAt)
	if err == nil {
		err = o.Validate()
	}
	return o, err
}

func (r *Repository) lockedDataState(ctx context.Context, tx *sql.Tx, id string) (model.DataState, error) {
	return r.queryDataState(ctx, tx, id, lockDataImage, lockDataOperation)
}

func (r *Repository) queryDataState(ctx context.Context, tx *sql.Tx, id, imageQuery, operationQuery string) (model.DataState, error) {
	var s model.DataState
	err := r.queryRow(ctx, tx, imageQuery, id).Scan(&s.ImageID, &s.Status, &s.Deleted, &s.Size, &s.VirtualSize, &s.Checksum, &s.HashAlgorithm, &s.HashValue, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, repository.ErrNotFound
	}
	if err != nil {
		return s, err
	}
	o, err := scanOperation(r.queryRow(ctx, tx, operationQuery, id))
	if err == nil {
		s.Operation = &o
	} else if !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	return s, nil
}

// Always lock parent first, as PATCH/DELETE do. No caller callback or filesystem
// work runs under these locks. Commit failure is explicitly an unknown outcome.
func (r *Repository) dataTransaction(ctx context.Context, id string, change func(context.Context, *sql.Tx, model.DataState) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return conflict(err)
	}
	defer tx.Rollback()
	s, err := r.lockedDataState(ctx, tx, id)
	if err != nil {
		return conflict(err)
	}
	if err = change(ctx, tx, s); err != nil {
		return conflict(err)
	}
	if tx.Commit() != nil {
		return repository.ErrCommitUncertain
	}
	return nil
}

func acknowledged(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	result, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return repository.ErrConflict
	}
	return nil
}

func dataTime(now time.Time, s model.DataState) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	latest := s.UpdatedAt
	if s.Operation != nil && s.Operation.UpdatedAt.After(latest) {
		latest = s.Operation.UpdatedAt
	}
	if !now.After(latest) {
		now = latest.Add(time.Microsecond)
	}
	return now
}

func currentAttempt(s model.DataState, attempt string, phase model.DataPhase) error {
	if s.Operation == nil || s.Operation.AttemptID != attempt || s.Operation.Phase != phase {
		return repository.ErrConflict
	}
	return nil
}
func saving(s model.DataState) bool { return !s.Deleted && s.Status == "saving" }

func (r *Repository) ReserveData(ctx context.Context, id, attempt string, expected *int64, now time.Time) (model.DataOperation, error) {
	var out model.DataOperation
	if err := transferIDs(id, attempt); err != nil {
		return out, err
	}
	if expected != nil && *expected < 0 {
		return out, model.Fail(model.Invalid, "Invalid expected size.")
	}
	err := r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		if s.Deleted {
			return repository.ErrNotFound
		}
		if s.Status != "queued" || s.Size != nil || s.VirtualSize != nil || s.Checksum != nil || s.HashAlgorithm != nil || s.HashValue != nil {
			return repository.ErrConflict
		}
		if s.Operation != nil && (s.Operation.Phase != model.DataCleaned || s.Operation.CleanupTarget == nil || *s.Operation.CleanupTarget != "queued" || s.Operation.AttemptID == attempt) {
			return repository.ErrConflict
		}
		at := dataTime(now, s)
		out = model.DataOperation{ImageID: id, AttemptID: attempt, Phase: model.DataReserved, ExpectedSize: expected, CreatedAt: at, UpdatedAt: at}
		if err := acknowledged(ctx, tx, "UPDATE images SET status='saving', updated_at=? WHERE id=? AND status='queued' AND deleted=FALSE", at, id); err != nil {
			return err
		}
		if s.Operation == nil {
			err := acknowledged(ctx, tx, "INSERT INTO image_data_operations (image_id, attempt_id, phase, expected_size, created_at, updated_at) VALUES (?, ?, 'reserved', ?, ?, ?)", id, attempt, expected, at, at)
			var my *driver.MySQLError
			if errors.As(err, &my) && my.Number == 1062 {
				return repository.ErrDuplicate
			}
			return err
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET attempt_id=?, phase='reserved', expected_size=?, size=NULL, virtual_size=NULL, checksum=NULL, os_hash_value=NULL, cleanup_target=NULL, last_error_code=NULL, created_at=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase='cleaned'", attempt, expected, at, at, id, s.Operation.AttemptID)
	})
	// Return the proposed attempt even on uncertain commit so the caller can resolve
	// it by image/attempt before performing any filesystem action.
	return out, err
}

func (r *Repository) StageData(ctx context.Context, id, attempt string, c model.DataContent, now time.Time) error {
	if err := transferIDs(id, attempt); err != nil {
		return err
	}
	if err := c.Validate(nil); err != nil {
		return err
	}
	return r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		if !saving(s) {
			return repository.ErrConflict
		}
		if err := currentAttempt(s, attempt, model.DataReserved); err != nil {
			return err
		}
		if err := c.Validate(s.Operation.ExpectedSize); err != nil {
			return err
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET phase='staged', size=?, virtual_size=?, checksum=?, os_hash_value=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase='reserved'", c.Size, c.VirtualSize, c.Checksum, c.SHA512, dataTime(now, s), id, attempt)
	})
}

func (r *Repository) PublishData(ctx context.Context, id, attempt string, now time.Time) error {
	if err := transferIDs(id, attempt); err != nil {
		return err
	}
	return r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		if !saving(s) {
			return repository.ErrConflict
		}
		if err := currentAttempt(s, attempt, model.DataStaged); err != nil {
			return err
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET phase='published', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='staged'", dataTime(now, s), id, attempt)
	})
}

func (r *Repository) CommitData(ctx context.Context, id, attempt string, now time.Time) error {
	if err := transferIDs(id, attempt); err != nil {
		return err
	}
	return r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		if !saving(s) {
			return repository.ErrConflict
		}
		if err := currentAttempt(s, attempt, model.DataPublished); err != nil {
			return err
		}
		o := s.Operation
		at := dataTime(now, s)
		if err := acknowledged(ctx, tx, "UPDATE images SET status='active', size=?, virtual_size=?, checksum=?, os_hash_algo='sha512', os_hash_value=?, updated_at=? WHERE id=? AND status='saving' AND deleted=FALSE", o.Size, o.VirtualSize, o.Checksum, o.SHA512, at, id); err != nil {
			return err
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET phase='committed', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='published'", at, id, attempt)
	})
}

func (r *Repository) BeginDataCleanup(ctx context.Context, id, attempt, target string, code *string, now time.Time) error {
	if err := transferIDs(id, attempt); err != nil {
		return err
	}
	if (target != "queued" && target != "deleted" && target != "killed") || !model.DataErrorCode(code) {
		return model.Fail(model.Invalid, "Invalid cleanup destination or error code.")
	}
	return r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		o := s.Operation
		if o == nil || o.AttemptID != attempt || o.Phase == model.DataCleaned {
			return repository.ErrConflict
		}
		if target == "deleted" {
			// This is not DELETE orchestration: a tombstone must already be confirmed.
			if !s.Deleted || s.Status != "deleted" {
				return repository.ErrConflict
			}
		} else if !saving(s) || o.Phase == model.DataCommitted {
			return repository.ErrConflict
		}
		if o.Phase == model.DataCleanup {
			if *o.CleanupTarget == target {
				return nil
			}
			if target != "deleted" {
				return repository.ErrConflict
			}
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET phase='cleanup', cleanup_target=?, last_error_code=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase=?", target, code, dataTime(now, s), id, attempt, string(o.Phase))
	})
}

func (r *Repository) CompleteDataCleanup(ctx context.Context, id, attempt string, now time.Time) error {
	if err := transferIDs(id, attempt); err != nil {
		return err
	}
	return r.dataTransaction(ctx, id, func(ctx context.Context, tx *sql.Tx, s model.DataState) error {
		if err := currentAttempt(s, attempt, model.DataCleanup); err != nil {
			return err
		}
		target := *s.Operation.CleanupTarget
		at := dataTime(now, s)
		if target == "deleted" {
			if !s.Deleted || s.Status != "deleted" {
				return repository.ErrConflict
			}
		} else {
			if !saving(s) {
				return repository.ErrConflict
			}
			if err := acknowledged(ctx, tx, "UPDATE images SET status=?, size=NULL, virtual_size=NULL, checksum=NULL, os_hash_algo=NULL, os_hash_value=NULL, updated_at=? WHERE id=? AND status='saving' AND deleted=FALSE", target, at, id); err != nil {
				return err
			}
		}
		return acknowledged(ctx, tx, "UPDATE image_data_operations SET phase='cleaned', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='cleanup'", at, id, attempt)
	})
}

// DataState uses a new locking transaction, not an earlier repeatable-read view.
// Call against the authoritative primary to resolve uncertain commits before I/O.
func (r *Repository) DataState(ctx context.Context, id string) (model.DataState, error) {
	var result model.DataState
	if canonical, err := model.CanonicalID(id); err != nil || canonical != id {
		return result, model.Fail(model.Invalid, "Canonical image UUID required.")
	}
	err := r.dataTransaction(ctx, id, func(_ context.Context, _ *sql.Tx, s model.DataState) error { result = s; return nil })
	return result, err
}

// DataStateSnapshot is a consistent, non-locking read (read-only repeatable
// read transaction) for GET: it neither waits for nor blocks writers, so
// concurrent downloads of one image do not serialize on its row lock. Writers
// and uncertain-commit resolution keep the locking DataState.
func (r *Repository) DataStateSnapshot(ctx context.Context, id string) (model.DataState, error) {
	if canonical, err := model.CanonicalID(id); err != nil || canonical != id {
		return model.DataState{}, model.Fail(model.Invalid, "Canonical image UUID required.")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return model.DataState{}, err
	}
	defer tx.Rollback()
	s, err := r.queryDataState(ctx, tx, id, readDataImage, readDataOperation)
	if err != nil {
		return model.DataState{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.DataState{}, err
	}
	return s, nil
}

// DownloadView is GET's single read-only transaction: the live image row (as
// Get, without properties/tags) and its data state, both from one snapshot.
func (r *Repository) DownloadView(ctx context.Context, id string) (model.Image, model.DataState, error) {
	if canonical, err := model.CanonicalID(id); err != nil || canonical != id {
		return model.Image{}, model.DataState{}, model.Fail(model.Invalid, "Canonical image UUID required.")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return model.Image{}, model.DataState{}, err
	}
	defer tx.Rollback()
	image, err := scanImage(r.queryRow(ctx, tx, readImageQuery, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Image{}, model.DataState{}, repository.ErrNotFound
	}
	if err != nil {
		return model.Image{}, model.DataState{}, err
	}
	s, err := r.queryDataState(ctx, tx, id, readDataImage, readDataOperation)
	if err != nil {
		return model.Image{}, model.DataState{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Image{}, model.DataState{}, err
	}
	return image, s, nil
}

// Listing is only a bounded candidate inventory. Recovery must re-read DataState
// under locks before acting. It must not infer abandonment from timestamps alone.
func (r *Repository) ListDataOperations(ctx context.Context, after string, limit int) ([]model.DataOperation, error) {
	if limit < 1 || limit > 1000 {
		return nil, model.Fail(model.Invalid, "Invalid recovery page size.")
	}
	if after != "" {
		if id, err := model.CanonicalID(after); err != nil || id != after {
			return nil, model.Fail(model.Invalid, "Invalid recovery marker.")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, "SELECT "+dataColumns+" FROM image_data_operations WHERE image_id>? ORDER BY image_id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.DataOperation{}
	for rows.Next() {
		o, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}
