package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	driver "github.com/go-sql-driver/mysql"
)

const imageColumns = "id, owner, name, status, visibility, disk_format, container_format, min_disk, min_ram, protected, os_hidden, size, virtual_size, checksum, os_hash_algo, os_hash_value, created_at, updated_at"

// imageAlias is imageColumns qualified with the alias i.
const imageAlias = "i.id, i.owner, i.name, i.status, i.visibility, i.disk_format, i.container_format, i.min_disk, i.min_ram, i.protected, i.os_hidden, i.size, i.virtual_size, i.checksum, i.os_hash_algo, i.os_hash_value, i.created_at, i.updated_at"

type scanner interface{ Scan(...any) error }

// childColumns load properties and tags in the image's own row (one round
// trip, as the original's joinedload), aggregated as JSON per image.
const childColumns = ", (SELECT JSON_OBJECTAGG(name, value) FROM image_properties WHERE image_id=i.id), (SELECT JSON_ARRAYAGG(value) FROM image_tags WHERE image_id=i.id)"

// scanImageWithChildren scans imageColumns plus childColumns.
func scanImageWithChildren(row scanner) (model.Image, error) {
	i := model.Image{Tags: []string{}, Properties: map[string]string{}}
	var props, tags sql.NullString
	err := row.Scan(&i.ID, &i.Owner, &i.Name, &i.Status, &i.Visibility, &i.DiskFormat, &i.ContainerFormat, &i.MinDisk, &i.MinRAM, &i.Protected, &i.Hidden, &i.Size, &i.VirtualSize, &i.Checksum, &i.HashAlgorithm, &i.HashValue, &i.CreatedAt, &i.UpdatedAt, &props, &tags)
	if err != nil {
		return i, err
	}
	if props.Valid {
		values := map[string]*string{}
		if json.Unmarshal([]byte(props.String), &values) != nil {
			return i, errors.New("unreadable image properties")
		}
		for k, v := range values {
			if v != nil {
				i.Properties[k] = *v
			} else {
				i.Properties[k] = ""
			}
		}
	}
	if tags.Valid {
		if json.Unmarshal([]byte(tags.String), &i.Tags) != nil {
			return i, errors.New("unreadable image tags")
		}
		sort.Strings(i.Tags) // same order as the former ORDER BY value
	}
	return i, nil
}

func scanImage(row scanner) (model.Image, error) {
	i := model.Image{Tags: []string{}, Properties: map[string]string{}}
	err := row.Scan(&i.ID, &i.Owner, &i.Name, &i.Status, &i.Visibility, &i.DiskFormat, &i.ContainerFormat, &i.MinDisk, &i.MinRAM, &i.Protected, &i.Hidden, &i.Size, &i.VirtualSize, &i.Checksum, &i.HashAlgorithm, &i.HashValue, &i.CreatedAt, &i.UpdatedAt)
	return i, err
}

const (
	readImageQuery     = "SELECT " + imageColumns + " FROM images WHERE id=? AND deleted=FALSE"
	fullImageQuery     = "SELECT " + imageAlias + childColumns + " FROM images i WHERE i.id=? AND i.deleted=FALSE"
	lockImageQuery     = fullImageQuery + " FOR UPDATE"
	oneImagePropsQuery = "SELECT image_id, name, value FROM image_properties WHERE image_id IN (?) ORDER BY image_id, name"
	oneImageTagsQuery  = "SELECT image_id, value FROM image_tags WHERE image_id IN (?) ORDER BY image_id, value"
)

func (r *Repository) readImage(ctx context.Context, tx *sql.Tx, id string, lock bool) (model.Image, error) {
	query := fullImageQuery
	if lock {
		query = lockImageQuery
	}
	i, err := scanImageWithChildren(r.queryRow(ctx, tx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return i, repository.ErrNotFound
	}
	return i, err
}

// All cursors are closed before issuing another statement on the pinned tx.
func (r *Repository) readChildren(ctx context.Context, tx *sql.Tx, images []model.Image) error {
	if len(images) == 0 {
		return nil
	}
	args := make([]any, 0, len(images))
	byID := map[string]*model.Image{}
	for n := range images {
		args = append(args, images[n].ID)
		byID[images[n].ID] = &images[n]
	}
	places := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := r.query(ctx, tx, "SELECT image_id, name, value FROM image_properties WHERE image_id IN ("+places+") ORDER BY image_id, name", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, key, value string
		if err = rows.Scan(&id, &key, &value); err != nil {
			break
		}
		if image := byID[id]; image != nil {
			image.Properties[key] = value
		} else {
			err = errors.New("unexpected property image")
			break
		}
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	if closeErr != nil {
		return closeErr
	}
	rows, err = r.query(ctx, tx, "SELECT image_id, value FROM image_tags WHERE image_id IN ("+places+") ORDER BY image_id, value", args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, value string
		if err = rows.Scan(&id, &value); err != nil {
			break
		}
		if image := byID[id]; image != nil {
			image.Tags = append(image.Tags, value)
		} else {
			err = errors.New("unexpected tag image")
			break
		}
	}
	rowErr = rows.Err()
	closeErr = rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	return closeErr
}

// childBatch keeps a multi-row INSERT far below MySQL's 65535 placeholders.
const childBatch = 500

// insertChildren writes properties (sorted by key) and tags (in their order)
// with one multi-row INSERT per table and batch instead of one per row. A
// single row produces exactly the former single-row statement.
func (r *Repository) insertChildren(ctx context.Context, tx *sql.Tx, id string, properties map[string]string, tags []string) error {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for start := 0; start < len(keys); start += childBatch {
		batch := keys[start:min(start+childBatch, len(keys))]
		args := make([]any, 0, 3*len(batch))
		for _, key := range batch {
			args = append(args, id, key, properties[key])
		}
		q := "INSERT INTO image_properties (image_id, name, value, created_at, deleted) VALUES " + valueRows("(?, ?, ?, UTC_TIMESTAMP(), 0)", len(batch))
		if _, err := r.exec(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("insert image property: %w", err)
		}
	}
	for start := 0; start < len(tags); start += childBatch {
		batch := tags[start:min(start+childBatch, len(tags))]
		args := make([]any, 0, 2*len(batch))
		for _, tag := range batch {
			args = append(args, id, tag)
		}
		q := "INSERT INTO image_tags (image_id, value, created_at, deleted) VALUES " + valueRows("(?, ?, UTC_TIMESTAMP(), 0)", len(batch))
		if _, err := r.exec(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("insert image tag: %w", err)
		}
	}
	return nil
}

func valueRows(row string, n int) string {
	return strings.TrimSuffix(strings.Repeat(row+", ", n), ", ")
}

func conflict(err error) error {
	var my *driver.MySQLError
	if errors.As(err, &my) && (my.Number == 1213 || my.Number == 1205) {
		return repository.ErrConflict
	}
	return err
}

func (r *Repository) Update(ctx context.Context, id string, change func(model.Image) (model.Image, error)) (result model.Image, err error) {
	err = withLockRetry(ctx, func() (e error) { result, e = r.update(ctx, id, change); return e })
	return result, conflict(err)
}

func (r *Repository) update(ctx context.Context, id string, change func(model.Image) (model.Image, error)) (result model.Image, err error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	current, err := r.readImage(ctx, tx, id, true)
	if err != nil {
		return result, err
	}
	// change may edit the maps and slices it is given in place: what to compare
	// with is a copy taken before.
	before := model.Image{Properties: maps.Clone(current.Properties), Tags: slices.Clone(current.Tags)}
	result, err = change(current)
	if err != nil {
		return model.Image{}, err
	}
	if result.ID != id || result.Owner != current.Owner || !result.CreatedAt.Equal(current.CreatedAt) || !statusChangeAllowed(current.Status, result.Status) {
		return model.Image{}, errors.New("immutable image fields changed")
	}
	updated, err := r.exec(ctx, tx, `UPDATE images SET name=?, visibility=?, disk_format=?, container_format=?, min_disk=?, min_ram=?, protected=?, os_hidden=?, status=?, updated_at=? WHERE id=? AND deleted=FALSE AND status=?`,
		result.Name, result.Visibility, result.DiskFormat, result.ContainerFormat, result.MinDisk, result.MinRAM, result.Protected, result.Hidden, result.Status, result.UpdatedAt.UTC(), id, current.Status)
	if err != nil {
		return model.Image{}, err
	}
	if n, e := updated.RowsAffected(); e != nil {
		return model.Image{}, e
	} else if n != 1 {
		return model.Image{}, repository.ErrConflict
	}
	// Parent lock serializes writers; child changes and parent update share commit.
	// Properties and tags are rewritten only when they changed: renaming an
	// image with many properties does not touch them.
	if !maps.Equal(before.Properties, result.Properties) || !slices.Equal(before.Tags, result.Tags) {
		if _, err = r.exec(ctx, tx, "DELETE FROM image_properties WHERE image_id=?", id); err != nil {
			return model.Image{}, err
		}
		if _, err = r.exec(ctx, tx, "DELETE FROM image_tags WHERE image_id=?", id); err != nil {
			return model.Image{}, err
		}
		if err = r.insertChildren(ctx, tx, id, result.Properties, result.Tags); err != nil {
			return model.Image{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.Image{}, err
	}
	return result, nil
}

// statusChangeAllowed: a metadata update keeps the status, except the
// original's deactivate/reactivate actions (active <-> deactivated). Data
// transitions (saving, active after upload, deleted) have their own paths.
func statusChangeAllowed(from, to string) bool {
	return from == to || from == "active" && to == "deactivated" || from == "deactivated" && to == "active"
}

func (r *Repository) Delete(ctx context.Context, id string, now time.Time, check func(model.Image) error) error {
	return conflict(withLockRetry(ctx, func() error { return r.deleteImage(ctx, id, now, check) }))
}

func (r *Repository) deleteImage(ctx context.Context, id string, now time.Time, check func(model.Image) error) (err error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := r.readImage(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if err = check(current); err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)
	if !now.After(current.UpdatedAt) {
		now = current.UpdatedAt.Add(time.Microsecond)
	}
	res, err := r.exec(ctx, tx, "UPDATE images SET deleted=TRUE, deleted_at=?, status='deleted', updated_at=? WHERE id=? AND deleted=FALSE", now, now, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return repository.ErrConflict
	}
	// Soft-delete properties, tags and locations with it, as the original's
	// image_destroy; the UUID stays reserved by the tombstone.
	if err = r.destroyChildren(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

func markerValue(i model.Image, key string) any {
	switch key {
	case "id":
		return i.ID
	case "created_at":
		return i.CreatedAt.UTC()
	case "updated_at":
		return i.UpdatedAt.UTC()
	case "status":
		return i.Status
	case "name":
		if i.Name != nil {
			return *i.Name
		}
	case "disk_format":
		if i.DiskFormat != nil {
			return *i.DiskFormat
		}
	case "container_format":
		if i.ContainerFormat != nil {
			return *i.ContainerFormat
		}
	case "size":
		if i.Size != nil {
			return *i.Size
		}
	}
	return nil
}

// Values are always bound; identifiers/directions come only from whitelists.
func listQuery(o model.ListOptions, marker *model.Image) (string, []any) {
	where := []string{"i.deleted=FALSE"}
	args := []any{}
	vis := o.Filters["visibility"]
	if !o.Admin {
		// As the original: own, public and community images, and shared images
		// of which the project is a member with member_status (accepted by
		// default; "all" means any status).
		member := "EXISTS (SELECT 1 FROM image_members m WHERE m.image_id=i.id AND m.member=? AND m.deleted=0"
		if o.MemberStatus != "all" {
			member += " AND m.status=?"
		}
		where = append(where, "(i.owner=? OR i.visibility IN ('public','community') OR (i.visibility='shared' AND "+member+")))")
		args = append(args, o.ProjectID, o.ProjectID)
		if o.MemberStatus != "all" {
			args = append(args, o.MemberStatus)
		}
	}
	if vis == "" {
		where = append(where, "(i.owner=? OR i.visibility<>'community')")
		args = append(args, o.ProjectID)
	}
	keys := make([]string, 0, len(o.Filters))
	for key := range o.Filters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := o.Filters[key]
		if key == "visibility" && value == "all" {
			continue
		}
		switch key {
		case "protected", "os_hidden":
			where = append(where, "i."+key+"=?")
			args = append(args, value == "true")
		case "id", "name", "status", "visibility", "owner", "disk_format", "container_format", "min_disk", "min_ram":
			where = append(where, "i."+key+"=?")
			args = append(args, value)
		default:
			where = append(where, "EXISTS (SELECT 1 FROM image_properties p WHERE p.image_id=i.id AND p.name=? AND p.value=?)")
			args = append(args, key, value)
		}
	}
	for _, tag := range o.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM image_tags t WHERE t.image_id=i.id AND t.value=?)")
		args = append(args, tag)
	}
	order := []string{}
	for _, s := range o.Sort {
		order = append(order, "i."+s.Key+" "+strings.ToUpper(s.Direction))
	}
	if marker != nil {
		alternatives := []string{}
		for n, s := range o.Sort {
			prefix := []string{}
			for _, prior := range o.Sort[:n] {
				prefix = append(prefix, "i."+prior.Key+" <=> ?")
				args = append(args, markerValue(*marker, prior.Key))
			}
			value := markerValue(*marker, s.Key)
			col := "i." + s.Key
			if value == nil {
				if s.Direction == "asc" {
					prefix = append(prefix, col+" IS NOT NULL")
				} else {
					prefix = append(prefix, "FALSE")
				}
			} else if s.Direction == "asc" {
				prefix = append(prefix, col+" > ?")
				args = append(args, value)
			} else {
				prefix = append(prefix, "("+col+" < ? OR "+col+" IS NULL)")
				args = append(args, value)
			}
			alternatives = append(alternatives, "("+strings.Join(prefix, " AND ")+")")
		}
		where = append(where, "("+strings.Join(alternatives, " OR ")+")")
	}
	args = append(args, o.Limit)
	columns := strings.Split(imageColumns, ", ")
	for n := range columns {
		columns[n] = "i." + columns[n]
	}
	return "SELECT " + strings.Join(columns, ", ") + childColumns + " FROM images i WHERE " + strings.Join(where, " AND ") + " ORDER BY " + strings.Join(order, ", ") + " LIMIT ?", args
}

func (r *Repository) List(ctx context.Context, o model.ListOptions) ([]model.Image, error) {
	o, err := o.Validate()
	if err != nil {
		return nil, err
	}
	if o.ProjectID == "" {
		return nil, errors.New("verified project scope is required")
	}
	ctx, cancel := r.bound(ctx)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var marker *model.Image
	if o.Marker != "" {
		image, e := r.readImage(ctx, tx, o.Marker, false)
		if e == nil && !o.Admin && image.Owner != o.ProjectID && image.Visibility == "shared" {
			var member bool
			if member, e = r.isMemberTx(ctx, tx, image.ID, o.ProjectID); e == nil && member {
				image.Visibility = "public" // visible: only for the check below
			}
		}
		if errors.Is(e, repository.ErrNotFound) || (e == nil && !o.Admin && image.Owner != o.ProjectID && image.Visibility != "public" && image.Visibility != "community") {
			return nil, model.Fail(model.Invalid, "Invalid or inaccessible marker.")
		}
		if e != nil {
			return nil, e
		}
		marker = &image
	}
	query, args := listQuery(o, marker)
	rows, err := r.query(ctx, tx, query, args...)
	if err != nil {
		return nil, err
	}
	images := []model.Image{}
	for rows.Next() {
		image, e := scanImageWithChildren(rows)
		if e != nil {
			err = e
			break
		}
		images = append(images, image)
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return images, nil
}
