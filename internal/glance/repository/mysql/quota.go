package mysql

import (
	"context"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// One round trip for the four usages of a project, as the original's
// user_get_* DB API computes them.
const usageQuery = `SELECT
 (SELECT COUNT(*) FROM images WHERE owner=? AND deleted=0 AND status NOT IN ('killed','deleted')),
 (SELECT COALESCE(SUM(i.size),0) FROM images i JOIN image_locations l ON l.image_id=i.id AND l.deleted=0 AND l.status<>'deleted'
    WHERE i.owner=? AND i.deleted=0 AND i.size>0 AND i.status NOT IN ('killed','deleted')),
 (SELECT COUNT(*) FROM images WHERE owner=? AND deleted=0 AND status IN ('saving','uploading','importing'))
  + (SELECT COUNT(*) FROM images i JOIN image_properties c ON c.image_id=i.id AND c.deleted=0 AND c.name='os_glance_importing_to_stores' AND c.value<>''
    WHERE i.owner=? AND i.deleted=0 AND i.status NOT IN ('saving','uploading','importing')),
 (SELECT COALESCE(SUM(size),0) FROM images WHERE owner=? AND deleted=0 AND status IN ('uploading','importing'))
  + (SELECT COALESCE(SUM(i.size),0) FROM images i JOIN image_properties c ON c.image_id=i.id AND c.deleted=0 AND c.name='os_glance_importing_to_stores' AND c.value<>''
    WHERE i.owner=? AND i.deleted=0 AND i.status='active')`

func (r *Repository) ProjectUsage(ctx context.Context, project string) (model.Usage, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	var u model.Usage
	err := r.db.QueryRowContext(ctx, r.sql(usageQuery), project, project, project, project, project, project).
		Scan(&u.CountTotal, &u.SizeTotal, &u.CountUploading, &u.StageTotal)
	return u, err
}
