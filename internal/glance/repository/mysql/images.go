package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	driver "github.com/go-sql-driver/mysql"
)

type Repository struct {
	db      *sql.DB
	timeout time.Duration
	// stmts holds server-side prepared statements for hot static reads,
	// created once by PrepareStatements (outside any transaction) and then
	// only read. nil means every query is prepared and executed per call.
	stmts map[string]*sql.Stmt
	// official, when set, makes this repository use the original Glance tables.
	official *officialCatalog
	// backends (multi-store): name -> directory, and the default's name.
	backends       map[string]string
	defaultBackend string
	// delayedDelete is [DEFAULT] delayed_delete: DELETE leaves the data
	// pending_delete for glance-scrubber instead of removing it.
	delayedDelete bool
}

// SetDelayedDelete enables the original's delayed_delete.
func (r *Repository) SetDelayedDelete(on bool) { r.delayedDelete = on }

// SetBackends enables multi-store locations (enabled_backends).
func (r *Repository) SetBackends(def string, dirs map[string]string) {
	r.defaultBackend, r.backends = def, dirs
}

// MultiStore reports whether enabled_backends is configured.
func (r *Repository) MultiStore() bool { return len(r.backends) > 0 }

// locationFor is the location value and meta_data of an image's file in a
// backend ("" is the default store).
func (r *Repository) locationFor(id, backend string) (string, string) {
	if !r.MultiStore() {
		return r.location(id), "{}"
	}
	if backend == "" {
		backend = r.defaultBackend
	}
	meta, _ := json.Marshal(map[string]string{"store": backend})
	return "file://" + r.backends[backend] + "/" + id, string(meta)
}

// backendOf is the store of the image's first active location ("" for the
// default one), as the original serves from the first location.
func (r *Repository) backendOf(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	if !r.MultiStore() {
		return "", nil
	}
	var meta sql.NullString
	err := r.queryRow(ctx, tx, "SELECT meta_data FROM image_locations WHERE image_id=? AND deleted=0 AND status='active' ORDER BY id LIMIT 1", id).Scan(&meta)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(meta.String), &m)
	if m["store"] == r.defaultBackend {
		return "", nil
	}
	return m["store"], nil
}

var _ repository.Repository = (*Repository)(nil)

func (r *Repository) Create(ctx context.Context, image model.Image) error {
	if image.Status != "queued" {
		return errors.New("only queued metadata can be created")
	}
	return withLockRetry(ctx, func() error { return r.create(ctx, image) })
}

func (r *Repository) create(ctx context.Context, image model.Image) error {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata transaction: %w", err)
	}
	defer tx.Rollback()
	_, err = r.exec(ctx, tx, `INSERT INTO images
		(id, owner, name, status, visibility, disk_format, container_format, min_disk, min_ram, protected, os_hidden, created_at, updated_at, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, FALSE)`,
		image.ID, image.Owner, image.Name, image.Status, image.Visibility, image.DiskFormat, image.ContainerFormat,
		image.MinDisk, image.MinRAM, image.Protected, image.Hidden, image.CreatedAt.UTC(), image.UpdatedAt.UTC())
	if err != nil {
		var my *driver.MySQLError
		if errors.As(err, &my) && my.Number == 1062 {
			return repository.ErrDuplicate
		}
		return fmt.Errorf("insert image: %w", err)
	}
	if err = r.insertChildren(ctx, tx, image.ID, image.Properties, image.Tags); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit metadata transaction: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id string) (model.Image, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return model.Image{}, fmt.Errorf("begin metadata read: %w", err)
	}
	defer tx.Rollback()
	image, err := r.readImage(ctx, tx, id, false)
	if err != nil {
		return model.Image{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Image{}, fmt.Errorf("complete metadata read: %w", err)
	}
	return image, nil
}

// AddStoreLocation records the image's copy in a backend (copy-image).
func (r *Repository) AddStoreLocation(ctx context.Context, id, backend string, now time.Time) error {
	value, meta := r.locationFor(id, backend)
	at := now.UTC()
	_, err := r.db.ExecContext(ctx, r.sql("INSERT INTO image_locations (image_id, value, meta_data, status, created_at, updated_at, deleted) VALUES (?, ?, ?, 'active', ?, ?, 0)"), id, value, meta, at, at)
	return err
}

// RemoveStoreLocation soft-deletes the image's location in a backend.
func (r *Repository) RemoveStoreLocation(ctx context.Context, id, backend string, now time.Time) error {
	value, _ := r.locationFor(id, backend)
	at := now.UTC()
	_, err := r.db.ExecContext(ctx, r.sql("UPDATE image_locations SET status='deleted', deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND value=? AND deleted=0"), at, at, id, value)
	return err
}
