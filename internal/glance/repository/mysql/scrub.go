package mysql

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

// Scrub is the original's glance-scrubber pass: images left pending_delete
// (delayed_delete) longer than scrubTime have their file locations removed
// and become deleted, as scrubber._scrub_image. It returns the images done.
func (r *Repository) Scrub(ctx context.Context, scrubTime time.Duration, now time.Time) (int, error) {
	rows, err := r.db.QueryContext(ctx, r.sql("SELECT id FROM images WHERE status='pending_delete' AND deleted_at < ?"), now.UTC().Add(-scrubTime))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	done := 0
	// The alias keeps r.sql from adding deleted=0: pending_delete locations
	// are deleted rows, as the original's image_location_update leaves them.
	for _, id := range ids {
		lrows, err := r.db.QueryContext(ctx, r.sql("SELECT l.id, l.value FROM image_locations l WHERE l.image_id=? AND l.status IN ('pending_delete','active')"), id)
		if err != nil {
			return done, err
		}
		type loc struct {
			id    int64
			value string
		}
		var locs []loc
		for lrows.Next() {
			var l loc
			if err = lrows.Scan(&l.id, &l.value); err != nil {
				break
			}
			locs = append(locs, l)
		}
		if err = errors.Join(err, lrows.Err(), lrows.Close()); err != nil {
			return done, err
		}
		for _, l := range locs {
			if path, ok := strings.CutPrefix(l.value, "file://"); ok {
				if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return done, err
				}
			}
			at := now.UTC()
			if _, err = r.db.ExecContext(ctx, r.sql("UPDATE image_locations SET status='deleted', deleted=1, deleted_at=?, updated_at=? WHERE id=?"), at, at, l.id); err != nil {
				return done, err
			}
		}
		if _, err = r.db.ExecContext(ctx, r.sql("UPDATE images SET status='deleted', updated_at=? WHERE id=? AND status='pending_delete'"), now.UTC(), id); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
