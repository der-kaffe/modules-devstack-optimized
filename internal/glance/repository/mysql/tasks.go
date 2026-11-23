package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

// The original's tasks and task_info tables (task_create, task_update,
// task_get_all) and the import lock it keeps as an image property.

var _ repository.Tasks = (*Repository)(nil)

const taskColumns = "t.id, t.type, t.status, t.owner, t.image_id, t.request_id, t.user_id, t.expires_at, t.created_at, t.updated_at, i.input, i.result, i.message"
const taskFrom = " FROM tasks t LEFT JOIN task_info i ON i.task_id=t.id"

func scanTask(row scanner) (model.Task, error) {
	var t model.Task
	var expires, updated sql.NullTime
	var message sql.NullString
	err := row.Scan(&t.ID, &t.Type, &t.Status, &t.Owner, &t.ImageID, &t.RequestID, &t.UserID, &expires, &t.CreatedAt, &updated, &t.Input, &t.Result, &message)
	if expires.Valid {
		t.ExpiresAt = &expires.Time
	}
	t.UpdatedAt = t.CreatedAt
	if updated.Valid {
		t.UpdatedAt = updated.Time
	}
	t.Message = message.String
	return t, err
}

func (r *Repository) tasks(ctx context.Context, q string, args ...any) ([]model.Task, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, r.sql(q), args...)
	if err != nil {
		return nil, err
	}
	out := []model.Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			err = e
			break
		}
		out = append(out, t)
	}
	return out, errors.Join(err, rows.Err(), rows.Close())
}

func (r *Repository) GetTask(ctx context.Context, id string) (model.Task, error) {
	out, err := r.tasks(ctx, "SELECT "+taskColumns+taskFrom+" WHERE t.id=? AND t.deleted=0", id)
	if err != nil {
		return model.Task{}, err
	}
	if len(out) == 0 {
		return model.Task{}, repository.ErrTaskNotFound
	}
	return out[0], nil
}

func (r *Repository) ImageTasks(ctx context.Context, imageID string) ([]model.Task, error) {
	return r.tasks(ctx, "SELECT "+taskColumns+taskFrom+" WHERE t.image_id=? AND t.deleted=0 ORDER BY t.created_at DESC, t.id DESC", imageID)
}

func (r *Repository) OpenTasks(ctx context.Context) ([]model.Task, error) {
	return r.tasks(ctx, "SELECT "+taskColumns+taskFrom+" WHERE t.type IN ('api_image_import','import') AND t.status IN ('pending','processing') AND t.deleted=0 ORDER BY t.created_at")
}

// ListTasks is task_get_all: filters, one sort key plus id, marker paging.
func (r *Repository) ListTasks(ctx context.Context, o model.TaskListOptions) ([]model.Task, error) {
	where := []string{"t.deleted=0"}
	args := []any{}
	if o.Owner != "" {
		where, args = append(where, "t.owner=?"), append(args, o.Owner)
	}
	if o.Type != "" {
		where, args = append(where, "t.type=?"), append(args, o.Type)
	}
	if o.Status != "" {
		where, args = append(where, "t.status=?"), append(args, o.Status)
	}
	key, dir := o.SortKey, strings.ToUpper(o.SortDir)
	cmp := "<"
	if dir == "ASC" {
		cmp = ">"
	}
	if o.Marker != "" {
		marker, err := r.GetTask(ctx, o.Marker)
		if err != nil {
			return nil, err
		}
		var value any
		switch key {
		case "created_at":
			value = marker.CreatedAt
		case "updated_at":
			value = marker.UpdatedAt
		case "id":
			value = marker.ID
		case "type":
			value = marker.Type
		case "status":
			value = marker.Status
		}
		if key == "id" {
			where, args = append(where, "t.id "+cmp+" ?"), append(args, marker.ID)
		} else {
			where = append(where, "(t."+key+" "+cmp+" ? OR (t."+key+" = ? AND t.id "+cmp+" ?))")
			args = append(args, value, value, marker.ID)
		}
	}
	args = append(args, o.Limit)
	return r.tasks(ctx, "SELECT "+taskColumns+taskFrom+" WHERE "+strings.Join(where, " AND ")+" ORDER BY t."+key+" "+dir+", t.id "+dir+" LIMIT ?", args...)
}

func (r *Repository) insertTask(ctx context.Context, tx *sql.Tx, t model.Task) error {
	if _, err := r.exec(ctx, tx, "INSERT INTO tasks (id, type, status, owner, expires_at, created_at, updated_at, deleted, image_id, request_id, user_id) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)",
		t.ID, t.Type, t.Status, t.Owner, t.ExpiresAt, t.CreatedAt.UTC(), t.UpdatedAt.UTC(), t.ImageID, t.RequestID, t.UserID); err != nil {
		return err
	}
	_, err := r.exec(ctx, tx, "INSERT INTO task_info (task_id, input, result, message) VALUES (?, ?, ?, ?)", t.ID, t.Input, t.Result, t.Message)
	return err
}

func (r *Repository) UpdateTask(ctx context.Context, t model.Task) error {
	return conflict(withLockRetry(ctx, func() error {
		ctx, cancel := r.bound(ctx)
		defer cancel()
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = r.ack(ctx, tx, "UPDATE tasks SET status=?, expires_at=?, updated_at=? WHERE id=? AND deleted=0", t.Status, t.ExpiresAt, t.UpdatedAt.UTC(), t.ID); err != nil {
			return err
		}
		if _, err = r.exec(ctx, tx, "UPDATE task_info SET result=?, message=? WHERE task_id=?", t.Result, t.Message, t.ID); err != nil {
			return err
		}
		return tx.Commit()
	}))
}

func (r *Repository) setStatus(ctx context.Context, tx *sql.Tx, id, from, to string, at time.Time) error {
	return r.ack(ctx, tx, "UPDATE images SET status=?, updated_at=? WHERE id=? AND status=? AND deleted=FALSE", to, at, id, from)
}

func (r *Repository) BeginImport(ctx context.Context, t model.Task, from []string, check func(model.Image) error) error {
	if t.ImageID == nil {
		return errors.New("import task without image")
	}
	id := *t.ImageID
	return r.lockedImage(ctx, id, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		if err := check(image); err != nil {
			return err
		}
		allowed := false
		for _, s := range from {
			allowed = allowed || image.Status == s
		}
		if !allowed || image.Properties[model.ImportTaskProperty] != "" {
			return repository.ErrConflict
		}
		at := t.CreatedAt.UTC()
		// copy-image keeps the image active; the other methods import it.
		if image.Status != "active" {
			if err := r.setStatus(ctx, tx, id, image.Status, "importing", at); err != nil {
				return err
			}
		}
		for name, value := range map[string]string{model.ImportTaskProperty: t.ID, "os_glance_importing_to_stores": "", "os_glance_failed_import": ""} {
			if _, err := r.exec(ctx, tx, "INSERT INTO image_properties (image_id, name, value, created_at, deleted) VALUES (?, ?, ?, ?, 0) ON DUPLICATE KEY UPDATE value=VALUES(value), deleted=0, deleted_at=NULL, updated_at=VALUES(created_at)",
				id, name, value, at); err != nil {
				return err
			}
		}
		return r.insertTask(ctx, tx, t)
	})
}

func (r *Repository) ReleaseImport(ctx context.Context, imageID, taskID, status string, keep bool, now time.Time) (bool, error) {
	released := false
	err := r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		if image.Properties[model.ImportTaskProperty] != taskID {
			return nil // the lock was broken or released: nothing to do
		}
		if image.Status == "importing" {
			if err := r.ack(ctx, tx, "UPDATE images SET status=?, size=NULL, updated_at=? WHERE id=? AND status='importing' AND deleted=FALSE", status, now.UTC().Truncate(time.Second), imageID); err != nil {
				return err
			}
		}
		if !keep {
			if _, err := r.exec(ctx, tx, "DELETE FROM image_properties WHERE image_id=? AND name=?", imageID, model.ImportTaskProperty); err != nil {
				return err
			}
		}
		released = true
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil // the image was deleted during the import
	}
	return released, err
}

// SetStaged also records the staged size (uploading) or clears it (queued).
func (r *Repository) SetStaged(ctx context.Context, imageID, from, to string, now time.Time, check func(model.Image) error) error {
	return r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		if err := check(image); err != nil {
			return err
		}
		if image.Status != from {
			return repository.ErrConflict
		}
		if to == "queued" {
			return r.ack(ctx, tx, "UPDATE images SET status=?, size=NULL, updated_at=? WHERE id=? AND status=? AND deleted=FALSE", to, now.UTC().Truncate(time.Second), imageID, from)
		}
		return r.setStatus(ctx, tx, imageID, from, to, now.UTC().Truncate(time.Second))
	})
}

// StagedSize records the size of the staged data of an uploading image.
func (r *Repository) StagedSize(ctx context.Context, imageID string, size int64) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return r.ack(ctx, tx, "UPDATE images SET size=? WHERE id=? AND status='uploading' AND deleted=FALSE", size, imageID)
	})
}

// ImageLocations are the image's active locations (url, meta_data JSON).
func (r *Repository) ImageLocations(ctx context.Context, imageID string) ([]model.Location, error) {
	out := []model.Location{}
	err := r.rows(ctx, "SELECT value, meta_data FROM image_locations WHERE image_id=? AND deleted=0 AND status='active' ORDER BY id", []any{imageID}, func(s scanner) error {
		var l model.Location
		var meta sql.NullString
		err := s.Scan(&l.URL, &meta)
		l.Metadata = meta.String
		if l.Metadata == "" {
			l.Metadata = "{}"
		}
		out = append(out, l)
		return err
	})
	return out, err
}

// BeginLocationImport is the original's add_location: the import lock on a
// queued image (set_property_atomic) and the location_import task; the
// image stays queued.
func (r *Repository) BeginLocationImport(ctx context.Context, t model.Task, check func(model.Image) error) error {
	id := *t.ImageID
	return r.lockedImage(ctx, id, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		if err := check(image); err != nil {
			return err
		}
		if image.Properties[model.ImportTaskProperty] != "" {
			return repository.ErrConflict
		}
		if _, err := r.exec(ctx, tx, "INSERT INTO image_properties (image_id, name, value, created_at, deleted) VALUES (?, ?, ?, ?, 0) ON DUPLICATE KEY UPDATE value=VALUES(value), deleted=0, deleted_at=NULL, updated_at=VALUES(created_at)",
			id, model.ImportTaskProperty, t.ID, t.CreatedAt.UTC()); err != nil {
			return err
		}
		return r.insertTask(ctx, tx, t)
	})
}

// CommitLocation activates a queued image held by the task with its
// location, size and hashes, and releases the lock, in one transaction.
func (r *Repository) CommitLocation(ctx context.Context, imageID, taskID, url string, size int64, md5, sha512 string, now time.Time) error {
	return r.lockedImage(ctx, imageID, func(ctx context.Context, tx *sql.Tx, image model.Image) error {
		if image.Properties[model.ImportTaskProperty] != taskID || image.Status != "queued" {
			return repository.ErrConflict
		}
		at := now.UTC().Truncate(time.Second)
		if err := r.ack(ctx, tx, "UPDATE images SET status='active', size=?, checksum=?, os_hash_algo='sha512', os_hash_value=?, updated_at=? WHERE id=? AND status='queued' AND deleted=FALSE", size, md5, sha512, at, imageID); err != nil {
			return err
		}
		if _, err := r.exec(ctx, tx, "INSERT INTO image_locations (image_id, value, meta_data, status, created_at, updated_at, deleted) VALUES (?, ?, '{}', 'active', ?, ?, 0)", imageID, url, at, at); err != nil {
			return err
		}
		_, err := r.exec(ctx, tx, "DELETE FROM image_properties WHERE image_id=? AND name=?", imageID, model.ImportTaskProperty)
		return err
	})
}

// StoreLocation is the location this repository's store gives an image.
func (r *Repository) StoreLocation(imageID string) string { return r.location(imageID) }

// LocationsFor is ImageLocations for a page of images in one query (the
// original loads them with the images, joinedload).
func (r *Repository) LocationsFor(ctx context.Context, ids []string) (map[string][]model.Location, error) {
	out := make(map[string][]model.Location, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	marks := make([]string, len(ids))
	for n, id := range ids {
		args[n], marks[n] = id, "?"
	}
	q := "SELECT image_id, value, meta_data FROM image_locations WHERE image_id IN (" + strings.Join(marks, ",") + ") AND deleted=0 AND status='active' ORDER BY id"
	err := r.rows(ctx, q, args, func(s scanner) error {
		var id string
		var l model.Location
		var meta sql.NullString
		if err := s.Scan(&id, &l.URL, &meta); err != nil {
			return err
		}
		l.Metadata = meta.String
		if l.Metadata == "" {
			l.Metadata = "{}"
		}
		out[id] = append(out[id], l)
		return nil
	})
	return out, err
}
