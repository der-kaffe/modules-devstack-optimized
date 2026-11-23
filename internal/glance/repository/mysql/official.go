package mysql

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/google/uuid"
)

// The repository reads and writes the original Glance tables (images,
// image_properties, image_tags, image_locations in the official database), so
// both implementations serve one catalog and one filesystem store.
// Differences handled here, as the original's SQLAlchemy models define them:
//   - children carry id/created_at/deleted columns; reads skip deleted rows;
//   - an active image has an image_locations row pointing at its file;
//   - deletion soft-deletes children and locations, as image_destroy does.
// Transfer bookkeeping (image_data_operations) stays in the paired shared
// database; images the original uploaded get a committed operation adopted
// from their verified metadata and single local location.

// SupportedOfficialRevisions are the original's alembic heads this dialect
// was checked against; any other revision fails closed at startup.
var SupportedOfficialRevisions = []string{"2024_1_contract01"}

var officialTables = regexp.MustCompile(`\b(FROM|INTO|UPDATE|JOIN)\s+(images|image_properties|image_tags|image_locations|image_members|tasks|task_info|cached_images|node_reference|metadef_namespaces|metadef_objects|metadef_properties|metadef_tags|metadef_resource_types|metadef_namespace_resource_types)\b`)

// officialNamespace derives the attempt ID of an adopted original upload.
var officialNamespace = uuid.MustParse("6f1d1c64-6c2b-5b8e-9b0a-2f6f4d6e9a10")

type officialCatalog struct {
	schema   string
	storeDir string
	cache    sync.Map // original SQL -> rewritten SQL
}

// New is the repository over the original's tables in schema (normally
// "glance") with files under storeDir (its filesystem_store_datadir).
func New(db *sql.DB, timeout time.Duration, schema, storeDir string) (*Repository, error) {
	if !OfficialCatalogName(schema) || !filepath.IsAbs(storeDir) || filepath.Clean(storeDir) != storeDir {
		return nil, errors.New("invalid official catalog configuration")
	}
	return &Repository{db: db, timeout: timeout, official: &officialCatalog{schema: schema, storeDir: storeDir}}, nil
}

// VerifyOfficialCatalog checks the original's schema revision.
func (r *Repository) VerifyOfficialCatalog(ctx context.Context) error {
	var revision string
	if err := r.db.QueryRowContext(ctx, "SELECT version_num FROM `"+r.official.schema+"`.alembic_version").Scan(&revision); err != nil {
		return errors.New("cannot read the official catalog revision")
	}
	for _, known := range SupportedOfficialRevisions {
		if revision == known {
			return nil
		}
	}
	return errors.New("unsupported official catalog revision " + revision + "; manual review required")
}

// sql qualifies catalog tables with the official schema and adds the
// soft-delete filter to child reads. Own mode returns q unchanged.
func (r *Repository) sql(q string) string {
	o := r.official
	if v, ok := o.cache.Load(q); ok {
		return v.(string)
	}
	out := officialTables.ReplaceAllString(q, "$1 `"+o.schema+"`.$2")
	if strings.HasPrefix(q, "SELECT") {
		for _, table := range []string{"image_properties", "image_tags", "image_locations"} {
			out = strings.ReplaceAll(out, "`"+o.schema+"`."+table+" WHERE ", "`"+o.schema+"`."+table+" WHERE deleted=0 AND ")
		}
		out = strings.ReplaceAll(out, "image_properties p WHERE ", "image_properties p WHERE p.deleted=0 AND ")
		out = strings.ReplaceAll(out, "image_tags t WHERE ", "image_tags t WHERE t.deleted=0 AND ")
	}
	if len(q) < 4096 { // dynamic list queries vary; keep the cache bounded to static SQL
		o.cache.Store(q, out)
	}
	return out
}

func (r *Repository) exec(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, r.sql(q), args...)
}

func (r *Repository) ack(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	return acknowledged(ctx, tx, r.sql(q), args...)
}

func (r *Repository) location(id string) string { return "file://" + r.official.storeDir + "/" + id }

// storeLocation reports whether value is the image's file in one of the
// stores (the default one, or any enabled backend).
func (r *Repository) storeLocation(id, value string) bool {
	if value == r.location(id) {
		return true
	}
	for name := range r.backends {
		if v, _ := r.locationFor(id, name); v == value {
			return true
		}
	}
	return false
}

// OfficialAttempt is the attempt ID adopted for an image the original uploaded.
func OfficialAttempt(id string) string { return uuid.NewSHA1(officialNamespace, []byte(id)).String() }

// adoptable builds the committed operation of an active original upload
// with no bookkeeping row: verified sha512 metadata and exactly one active
// location, the local file named by its ID. Anything else is not adopted.
func (r *Repository) adoptable(ctx context.Context, tx *sql.Tx, s model.DataState) (*model.DataOperation, error) {
	if s.Deleted || (s.Status != "active" && s.Status != "deactivated") || s.Size == nil || s.Checksum == nil ||
		s.HashAlgorithm == nil || *s.HashAlgorithm != "sha512" || s.HashValue == nil {
		return nil, nil
	}
	rows, err := r.query(ctx, tx, "SELECT value FROM image_locations WHERE image_id=? AND status='active'", s.ImageID)
	if err != nil {
		return nil, err
	}
	var values []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			_ = rows.Close()
			return nil, err
		}
		values = append(values, v)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(values) != 1 || !r.storeLocation(s.ImageID, values[0]) {
		return nil, nil
	}
	o := model.DataOperation{ImageID: s.ImageID, AttemptID: OfficialAttempt(s.ImageID), Phase: model.DataCommitted,
		ExpectedSize: s.Size, Size: s.Size, VirtualSize: s.VirtualSize, Checksum: s.Checksum, SHA512: s.HashValue,
		CreatedAt: s.UpdatedAt, UpdatedAt: s.UpdatedAt}
	if o.Validate() != nil {
		return nil, nil
	}
	return &o, nil
}

// materialize records an adopted operation so later bookkeeping updates
// (deletion cleanup) act on a real row, inside the caller's locking tx.
func (r *Repository) materialize(ctx context.Context, tx *sql.Tx, o *model.DataOperation) error {
	return r.ack(ctx, tx, "INSERT INTO image_data_operations (image_id, attempt_id, phase, expected_size, size, virtual_size, checksum, os_hash_value, created_at, updated_at) VALUES (?, ?, 'committed', ?, ?, ?, ?, ?, ?, ?)",
		o.ImageID, o.AttemptID, o.ExpectedSize, o.Size, o.VirtualSize, o.Checksum, o.SHA512, o.CreatedAt, o.UpdatedAt)
}

// destroyChildren soft-deletes what the original's image_destroy removes
// with the image, the same way: locations are read without locking and
// updated by primary key (_image_locations_delete_all); properties and tags
// with one UPDATE by image (_image_child_entry_delete_all). An UPDATE of
// locations by image_id lets MySQL pick ix_image_locations_deleted and lock
// rows of other images: concurrent DELETEs deadlocked until their deadline.
func (r *Repository) destroyChildren(ctx context.Context, tx *sql.Tx, id string, at any) error {
	rows, err := r.query(ctx, tx, "SELECT id FROM image_locations WHERE image_id=?", id)
	if err != nil {
		return err
	}
	var locations []int64
	for rows.Next() {
		var location int64
		if err = rows.Scan(&location); err != nil {
			_ = rows.Close()
			return err
		}
		locations = append(locations, location)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, location := range locations {
		if _, err = r.exec(ctx, tx, "UPDATE image_locations SET deleted=1, deleted_at=?, updated_at=?, status='deleted' WHERE id=? AND deleted=0", at, at, location); err != nil {
			return err
		}
	}
	// Members too (_image_member_delete_all), read first and updated by
	// primary key for the same reason as locations.
	members, err := r.ids(ctx, tx, "SELECT id FROM image_members WHERE image_id=? AND deleted=0", id)
	if err != nil {
		return err
	}
	for _, member := range members {
		if _, err = r.exec(ctx, tx, "UPDATE image_members SET deleted=1, deleted_at=?, updated_at=? WHERE id=? AND deleted=0", at, at, member); err != nil {
			return err
		}
	}
	for _, q := range []string{
		"UPDATE image_properties SET deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND deleted=0",
		"UPDATE image_tags SET deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND deleted=0",
	} {
		if _, err = r.exec(ctx, tx, q, at, at, id); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) ids(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := r.query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, id)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// AdoptOriginalUploads records, once at startup, the committed operation of
// every active original upload that has none yet, so downloads no longer
// look up its location on each request. Each image is adopted in its own
// locking transaction with the same checks as on demand.
func (r *Repository) AdoptOriginalUploads(ctx context.Context) (int, error) {
	rows, err := r.db.QueryContext(ctx, r.sql("SELECT i.id FROM images i WHERE i.deleted=FALSE AND i.status IN ('active','deactivated') AND i.os_hash_algo='sha512'")+
		" AND NOT EXISTS (SELECT 1 FROM image_data_operations o WHERE o.image_id=i.id)")
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	adopted := 0
	for _, id := range ids {
		var s model.DataState
		err := r.dataTransaction(ctx, id, func(_ context.Context, _ *sql.Tx, state model.DataState) error { s = state; return nil })
		if err != nil {
			return adopted, err
		}
		if s.Operation != nil && s.Operation.AttemptID == OfficialAttempt(id) {
			adopted++
		}
	}
	return adopted, nil
}
