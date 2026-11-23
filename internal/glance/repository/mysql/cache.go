package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	driver "github.com/go-sql-driver/mysql"
)

// The original's centralized_db image cache tables: node_reference (one row
// per API node URL) and cached_images (one row per image cached on a node).

// CacheNode returns the node_reference_id of url, creating it if needed
// (node_reference_get_by_url / node_reference_create).
func (r *Repository) CacheNode(ctx context.Context, url string) (int64, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	var id int64
	err := r.db.QueryRowContext(ctx, r.sql("SELECT node_reference_id FROM node_reference WHERE node_reference_url=?"), url).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := r.db.ExecContext(ctx, r.sql("INSERT INTO node_reference (node_reference_url) VALUES (?)"), url)
	var my *driver.MySQLError
	if errors.As(err, &my) && my.Number == 1062 { // created concurrently
		err = r.db.QueryRowContext(ctx, r.sql("SELECT node_reference_id FROM node_reference WHERE node_reference_url=?"), url).Scan(&id)
		return id, err
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) CachedImages(ctx context.Context, node int64) ([]model.CachedImage, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, r.sql("SELECT image_id, last_accessed, last_modified, size, hits, checksum FROM cached_images WHERE node_reference_id=? ORDER BY last_accessed DESC, id"), node)
	if err != nil {
		return nil, err
	}
	out := []model.CachedImage{}
	for rows.Next() {
		var c model.CachedImage
		if err = rows.Scan(&c.ImageID, &c.LastAccessed, &c.LastModified, &c.Size, &c.Hits, &c.Checksum); err != nil {
			break
		}
		out = append(out, c)
	}
	return out, errors.Join(err, rows.Err(), rows.Close())
}

// CacheInsert is insert_cache_details (a replaced copy restarts its row).
func (r *Repository) CacheInsert(ctx context.Context, node int64, imageID string, size int64, checksum *string, now time.Time) error {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	at := now.UTC().Truncate(time.Second)
	_, err := r.db.ExecContext(ctx, r.sql("INSERT INTO cached_images (image_id, last_accessed, last_modified, size, hits, checksum, node_reference_id) VALUES (?, ?, ?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE last_accessed=VALUES(last_accessed), last_modified=VALUES(last_modified), size=VALUES(size), hits=0, checksum=VALUES(checksum)"),
		imageID, at, at, size, checksum, node)
	return err
}

// CacheHit is update_hit_count.
func (r *Repository) CacheHit(ctx context.Context, node int64, imageID string, now time.Time) error {
	return r.CacheHits(ctx, node, imageID, 1, now)
}

// CacheHits is update_hit_count for n accesses at once.
func (r *Repository) CacheHits(ctx context.Context, node int64, imageID string, n int64, now time.Time) error {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	_, err := r.db.ExecContext(ctx, r.sql("UPDATE cached_images SET hits=hits+?, last_accessed=? WHERE image_id=? AND node_reference_id=?"), n, now.UTC().Truncate(time.Second), imageID, node)
	return err
}

// CacheDelete is delete_cached_image ("" deletes every image of the node).
func (r *Repository) CacheDelete(ctx context.Context, node int64, imageID string) error {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	if imageID == "" {
		_, err := r.db.ExecContext(ctx, r.sql("DELETE FROM cached_images WHERE node_reference_id=?"), node)
		return err
	}
	_, err := r.db.ExecContext(ctx, r.sql("DELETE FROM cached_images WHERE image_id=? AND node_reference_id=?"), imageID, node)
	return err
}
