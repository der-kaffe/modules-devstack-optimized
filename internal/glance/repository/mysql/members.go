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

// The original's image_members rows, as its ImageMember model and
// image_member_* DB API: soft-deleted (deleted, deleted_at), unique on
// (image_id, member, deleted_at), can_share always false in API v2.

var _ repository.Members = (*Repository)(nil)

const memberColumns = "image_id, member, status, created_at, updated_at"

func scanMember(row scanner) (model.Member, error) {
	var m model.Member
	var updated sql.NullTime
	if err := row.Scan(&m.ImageID, &m.MemberID, &m.Status, &m.CreatedAt, &updated); err != nil {
		return m, err
	}
	m.UpdatedAt = m.CreatedAt
	if updated.Valid {
		m.UpdatedAt = updated.Time
	}
	return m, nil
}

func (r *Repository) ListMembers(ctx context.Context, imageID string) ([]model.Member, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := r.query(ctx, tx, "SELECT "+memberColumns+" FROM image_members WHERE image_id=? AND deleted=0 ORDER BY created_at, id", imageID)
	if err != nil {
		return nil, err
	}
	members := []model.Member{}
	for rows.Next() {
		m, e := scanMember(rows)
		if e != nil {
			err = e
			break
		}
		members = append(members, m)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return members, tx.Commit()
}

func (r *Repository) GetMember(ctx context.Context, imageID, memberID string) (model.Member, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	m, err := scanMember(r.db.QueryRowContext(ctx, r.sql("SELECT "+memberColumns+" FROM image_members WHERE image_id=? AND member=? AND deleted=0"), imageID, memberID))
	if errors.Is(err, sql.ErrNoRows) {
		return m, repository.ErrMemberNotFound
	}
	return m, err
}

// IsMember reports whether project is a member of the image, with any status
// (the original's is_image_visible for a single image).
func (r *Repository) IsMember(ctx context.Context, imageID, project string) (bool, error) {
	_, err := r.GetMember(ctx, imageID, project)
	if errors.Is(err, repository.ErrMemberNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (r *Repository) isMemberTx(ctx context.Context, tx *sql.Tx, imageID, project string) (bool, error) {
	var n int
	err := r.queryRow(ctx, tx, "SELECT COUNT(*) FROM image_members WHERE image_id=? AND member=? AND deleted=0", imageID, project).Scan(&n)
	return n > 0, err
}

// lockedImage runs fn inside one transaction holding the image row lock.
func (r *Repository) lockedImage(ctx context.Context, imageID string, fn func(context.Context, *sql.Tx, model.Image) error) error {
	return conflict(withLockRetry(ctx, func() error {
		ctx, cancel := r.bound(ctx)
		defer cancel()
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		image, err := r.readImage(ctx, tx, imageID, true)
		if err != nil {
			return err
		}
		if err = fn(ctx, tx, image); err != nil {
			return err
		}
		return tx.Commit()
	}))
}

func (r *Repository) lockedMember(ctx context.Context, tx *sql.Tx, imageID, memberID string) (int64, model.Member, error) {
	var id int64
	var m model.Member
	var updated sql.NullTime
	err := r.queryRow(ctx, tx, "SELECT id, "+memberColumns+" FROM image_members WHERE image_id=? AND member=? AND deleted=0 FOR UPDATE", imageID, memberID).
		Scan(&id, &m.ImageID, &m.MemberID, &m.Status, &m.CreatedAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, m, repository.ErrMemberNotFound
	}
	m.UpdatedAt = m.CreatedAt
	if updated.Valid {
		m.UpdatedAt = updated.Time
	}
	return id, m, err
}

func (r *Repository) CreateMember(ctx context.Context, imageID, memberID string, now time.Time, check func(model.Image, int) error) (model.Member, error) {
	at := now.UTC().Truncate(time.Second)
	m := model.Member{ImageID: imageID, MemberID: memberID, Status: "pending", CreatedAt: at, UpdatedAt: at}
	err := r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		var count int
		if err := r.queryRow(ctx, tx, "SELECT COUNT(*) FROM image_members WHERE image_id=? AND deleted=0", imageID).Scan(&count); err != nil {
			return err
		}
		if err := check(image, count); err != nil {
			return err
		}
		if _, _, err := r.lockedMember(ctx, tx, imageID, memberID); err == nil {
			return repository.ErrMemberDuplicate
		} else if !errors.Is(err, repository.ErrMemberNotFound) {
			return err
		}
		_, err := r.exec(ctx, tx, "INSERT INTO image_members (image_id, member, can_share, status, created_at, updated_at, deleted) VALUES (?, ?, 0, 'pending', ?, ?, 0)", imageID, memberID, at, at)
		var my *driver.MySQLError
		if errors.As(err, &my) && my.Number == 1062 {
			return repository.ErrMemberDuplicate
		}
		return err
	})
	return m, err
}

func (r *Repository) UpdateMember(ctx context.Context, imageID, memberID, status string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	var out model.Member
	err := r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		id, m, err := r.lockedMember(ctx, tx, imageID, memberID)
		if err != nil {
			return err
		}
		if err = check(image, m); err != nil {
			return err
		}
		at := now.UTC().Truncate(time.Second)
		if _, err = r.exec(ctx, tx, "UPDATE image_members SET status=?, updated_at=? WHERE id=?", status, at, id); err != nil {
			return err
		}
		m.Status, m.UpdatedAt = status, at
		out = m
		return nil
	})
	return out, err
}

func (r *Repository) DeleteMember(ctx context.Context, imageID, memberID string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	var out model.Member
	err := r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		id, m, err := r.lockedMember(ctx, tx, imageID, memberID)
		if err != nil {
			return err
		}
		if err = check(image, m); err != nil {
			return err
		}
		at := now.UTC().Truncate(time.Second)
		// image_member_delete: soft delete by primary key.
		if _, err = r.exec(ctx, tx, "UPDATE image_members SET deleted=1, deleted_at=?, updated_at=? WHERE id=?", at, at, id); err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}
