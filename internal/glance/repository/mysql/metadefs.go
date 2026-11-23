package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	driver "github.com/go-sql-driver/mysql"
)

// The original's metadata definitions (glance/db/sqlalchemy/metadef_api):
// metadef_namespaces and their objects, properties, tags and resource type
// associations. Children are deleted (not soft-deleted) with their namespace,
// as the original.

var _ repository.Metadefs = (*Repository)(nil)

func duplicate(err error) error {
	var my *driver.MySQLError
	if errors.As(err, &my) && my.Number == 1062 {
		return repository.ErrMetadefDuplicate
	}
	return err
}

func nullTime(t sql.NullTime) *time.Time {
	if t.Valid {
		return &t.Time
	}
	return nil
}

func (r *Repository) inTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	return conflict(withLockRetry(ctx, func() error {
		ctx, cancel := r.bound(ctx)
		defer cancel()
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = fn(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}))
}

func (r *Repository) rows(ctx context.Context, q string, args []any, scan func(scanner) error) error {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, r.sql(q), args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err = scan(rows); err != nil {
			break
		}
	}
	return errors.Join(err, rows.Err(), rows.Close())
}

const namespaceColumns = "n.id, n.namespace, n.display_name, n.description, n.visibility, n.protected, n.owner, n.created_at, n.updated_at"

func scanNamespace(row scanner) (model.Namespace, error) {
	var n model.Namespace
	var visibility sql.NullString
	var protected sql.NullBool
	var updated sql.NullTime
	err := row.Scan(&n.ID, &n.Namespace, &n.DisplayName, &n.Description, &visibility, &protected, &n.Owner, &n.CreatedAt, &updated)
	n.Visibility, n.Protected, n.UpdatedAt = visibility.String, protected.Bool, nullTime(updated)
	if n.Visibility == "" {
		n.Visibility = "private"
	}
	return n, err
}

func (r *Repository) ListNamespaces(ctx context.Context, o model.NamespaceListOptions) ([]model.Namespace, error) {
	where, args := []string{"TRUE"}, []any{}
	if !o.Admin {
		where, args = append(where, "(n.visibility='public' OR n.owner=?)"), append(args, o.Project)
	}
	if o.Visibility != "" {
		where, args = append(where, "n.visibility=?"), append(args, o.Visibility)
	}
	if len(o.ResourceTypes) > 0 {
		where = append(where, "EXISTS (SELECT 1 FROM metadef_namespace_resource_types a JOIN metadef_resource_types t ON t.id=a.resource_type_id WHERE a.namespace_id=n.id AND t.name IN ("+strings.TrimSuffix(strings.Repeat("?,", len(o.ResourceTypes)), ",")+"))")
		for _, t := range o.ResourceTypes {
			args = append(args, t)
		}
	}
	dir := strings.ToUpper(o.SortDir)
	cmp := map[string]string{"ASC": ">", "DESC": "<"}[dir]
	if o.Marker != "" {
		marker, err := r.GetNamespace(ctx, o.Marker)
		if err != nil {
			return nil, err
		}
		value := map[string]any{"created_at": marker.CreatedAt, "updated_at": marker.UpdatedAt, "namespace": marker.Namespace, "id": marker.ID}[o.SortKey]
		where = append(where, "(n."+o.SortKey+" "+cmp+" ? OR (n."+o.SortKey+" = ? AND n.id "+cmp+" ?))")
		args = append(args, value, value, marker.ID)
	}
	args = append(args, o.Limit)
	out := []model.Namespace{}
	err := r.rows(ctx, "SELECT "+namespaceColumns+" FROM metadef_namespaces n WHERE "+strings.Join(where, " AND ")+" ORDER BY n."+o.SortKey+" "+dir+", n.id "+dir+" LIMIT ?", args, func(s scanner) error {
		n, err := scanNamespace(s)
		out = append(out, n)
		return err
	})
	return out, err
}

func (r *Repository) GetNamespace(ctx context.Context, name string) (model.Namespace, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	n, err := scanNamespace(r.db.QueryRowContext(ctx, r.sql("SELECT "+namespaceColumns+" FROM metadef_namespaces n WHERE n.namespace=?"), name))
	if errors.Is(err, sql.ErrNoRows) {
		return n, repository.ErrMetadefNotFound
	}
	return n, err
}

func (r *Repository) Associations(ctx context.Context, namespaceID int64) ([]model.ResourceTypeAssociation, error) {
	out := []model.ResourceTypeAssociation{}
	// The original's query shape and no ORDER BY: MySQL's plan gives the
	// same order the original returns.
	err := r.rows(ctx, "SELECT t.name, a.prefix, a.properties_target, a.created_at, a.updated_at FROM metadef_resource_types t JOIN metadef_namespace_resource_types a ON t.id = a.resource_type_id WHERE a.namespace_id = ?", []any{namespaceID}, func(s scanner) error {
		var a model.ResourceTypeAssociation
		var updated sql.NullTime
		err := s.Scan(&a.Name, &a.Prefix, &a.PropertiesTarget, &a.CreatedAt, &updated)
		a.UpdatedAt = nullTime(updated)
		out = append(out, a)
		return err
	})
	return out, err
}

func (r *Repository) CreateNamespace(ctx context.Context, n model.Namespace, props []model.MetadefProperty, objects []model.MetadefObject, tags []model.MetadefTag, assocs []model.ResourceTypeAssociation) (model.Namespace, error) {
	err := r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.exec(ctx, tx, "INSERT INTO metadef_namespaces (namespace, display_name, description, visibility, protected, owner, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			n.Namespace, n.DisplayName, n.Description, n.Visibility, n.Protected, n.Owner, n.CreatedAt.UTC(), n.CreatedAt.UTC())
		if err != nil {
			return duplicate(err)
		}
		if n.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, p := range props {
			if err = r.insertProperty(ctx, tx, n.ID, p); err != nil {
				return err
			}
		}
		for _, o := range objects {
			if err = r.insertObject(ctx, tx, n.ID, o); err != nil {
				return err
			}
		}
		for _, t := range tags {
			if err = r.insertTag(ctx, tx, n.ID, t); err != nil {
				return err
			}
		}
		for _, a := range assocs {
			if err = r.associate(ctx, tx, n.ID, a); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

func (r *Repository) UpdateNamespace(ctx context.Context, id int64, n model.Namespace) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		err := r.ack(ctx, tx, "UPDATE metadef_namespaces SET namespace=?, display_name=?, description=?, visibility=?, protected=?, owner=?, updated_at=? WHERE id=?",
			n.Namespace, n.DisplayName, n.Description, n.Visibility, n.Protected, n.Owner, n.UpdatedAt, id)
		if errors.Is(err, repository.ErrConflict) {
			return nil // same values: MySQL reports 0 rows changed
		}
		return duplicate(err)
	})
}

func (r *Repository) DeleteNamespace(ctx context.Context, id int64) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, q := range []string{
			"DELETE FROM metadef_objects WHERE namespace_id=?", "DELETE FROM metadef_properties WHERE namespace_id=?",
			"DELETE FROM metadef_tags WHERE namespace_id=?", "DELETE FROM metadef_namespace_resource_types WHERE namespace_id=?",
			"DELETE FROM metadef_namespaces WHERE id=?",
		} {
			if _, err := r.exec(ctx, tx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Objects.

func scanObject(s scanner) (model.MetadefObject, error) {
	var o model.MetadefObject
	var required sql.NullString
	var schema string
	var updated sql.NullTime
	if err := s.Scan(&o.Name, &o.Description, &required, &schema, &o.CreatedAt, &updated); err != nil {
		return o, err
	}
	o.UpdatedAt, o.Properties = nullTime(updated), json.RawMessage(schema)
	if required.Valid && required.String != "" {
		if json.Unmarshal([]byte(required.String), &o.Required) != nil {
			return o, errors.New("unreadable metadef object required list")
		}
	}
	return o, nil
}

const objectColumns = "name, description, required, json_schema, created_at, updated_at"

func (r *Repository) Objects(ctx context.Context, namespaceID int64) ([]model.MetadefObject, error) {
	out := []model.MetadefObject{}
	err := r.rows(ctx, "SELECT "+objectColumns+" FROM metadef_objects WHERE namespace_id=? ORDER BY name", []any{namespaceID}, func(s scanner) error {
		o, err := scanObject(s)
		out = append(out, o)
		return err
	})
	return out, err
}

func (r *Repository) Object(ctx context.Context, namespaceID int64, name string) (model.MetadefObject, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	o, err := scanObject(r.db.QueryRowContext(ctx, r.sql("SELECT "+objectColumns+" FROM metadef_objects WHERE namespace_id=? AND name=?"), namespaceID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return o, repository.ErrMetadefNotFound
	}
	return o, err
}

func requiredText(o model.MetadefObject) *string {
	if len(o.Required) == 0 {
		return nil
	}
	b, _ := json.Marshal(o.Required)
	s := string(b)
	return &s
}

func (r *Repository) insertObject(ctx context.Context, tx *sql.Tx, namespaceID int64, o model.MetadefObject) error {
	_, err := r.exec(ctx, tx, "INSERT INTO metadef_objects (namespace_id, name, description, required, json_schema, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		namespaceID, o.Name, o.Description, requiredText(o), string(o.Properties), o.CreatedAt.UTC(), o.CreatedAt.UTC())
	return duplicate(err)
}

func (r *Repository) CreateObject(ctx context.Context, namespaceID int64, o model.MetadefObject) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error { return r.insertObject(ctx, tx, namespaceID, o) })
}

func (r *Repository) UpdateObject(ctx context.Context, namespaceID int64, name string, o model.MetadefObject) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.exec(ctx, tx, "UPDATE metadef_objects SET name=?, description=?, required=?, json_schema=?, updated_at=? WHERE namespace_id=? AND name=?",
			o.Name, o.Description, requiredText(o), string(o.Properties), o.UpdatedAt, namespaceID, name)
		if err != nil {
			return duplicate(err)
		}
		return found(res)
	})
}

// found: an UPDATE/DELETE by name matched a row (CLIENT_FOUND_ROWS).
func found(res sql.Result) error {
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return repository.ErrMetadefNotFound
	}
	return nil
}

func (r *Repository) deleteChild(ctx context.Context, q string, args ...any) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.exec(ctx, tx, q, args...)
		if err != nil {
			return err
		}
		return found(res)
	})
}

func (r *Repository) DeleteObject(ctx context.Context, namespaceID int64, name string) error {
	return r.deleteChild(ctx, "DELETE FROM metadef_objects WHERE namespace_id=? AND name=?", namespaceID, name)
}

func (r *Repository) DeleteObjects(ctx context.Context, namespaceID int64) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := r.exec(ctx, tx, "DELETE FROM metadef_objects WHERE namespace_id=?", namespaceID)
		return err
	})
}

// Properties.

func scanProperty(s scanner) (model.MetadefProperty, error) {
	var p model.MetadefProperty
	var schema string
	var updated sql.NullTime
	err := s.Scan(&p.Name, &schema, &p.CreatedAt, &updated)
	p.Schema, p.UpdatedAt = json.RawMessage(schema), nullTime(updated)
	return p, err
}

func (r *Repository) Properties(ctx context.Context, namespaceID int64) ([]model.MetadefProperty, error) {
	out := []model.MetadefProperty{}
	err := r.rows(ctx, "SELECT name, json_schema, created_at, updated_at FROM metadef_properties WHERE namespace_id=? ORDER BY name", []any{namespaceID}, func(s scanner) error {
		p, err := scanProperty(s)
		out = append(out, p)
		return err
	})
	return out, err
}

func (r *Repository) Property(ctx context.Context, namespaceID int64, name string) (model.MetadefProperty, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	p, err := scanProperty(r.db.QueryRowContext(ctx, r.sql("SELECT name, json_schema, created_at, updated_at FROM metadef_properties WHERE namespace_id=? AND name=?"), namespaceID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return p, repository.ErrMetadefNotFound
	}
	return p, err
}

func (r *Repository) insertProperty(ctx context.Context, tx *sql.Tx, namespaceID int64, p model.MetadefProperty) error {
	_, err := r.exec(ctx, tx, "INSERT INTO metadef_properties (namespace_id, name, json_schema, created_at, updated_at) VALUES (?, ?, ?, ?, ?)", namespaceID, p.Name, string(p.Schema), p.CreatedAt.UTC(), p.CreatedAt.UTC())
	return duplicate(err)
}

func (r *Repository) CreateProperty(ctx context.Context, namespaceID int64, p model.MetadefProperty) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error { return r.insertProperty(ctx, tx, namespaceID, p) })
}

func (r *Repository) UpdateProperty(ctx context.Context, namespaceID int64, name string, p model.MetadefProperty) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.exec(ctx, tx, "UPDATE metadef_properties SET name=?, json_schema=?, updated_at=? WHERE namespace_id=? AND name=?", p.Name, string(p.Schema), p.UpdatedAt, namespaceID, name)
		if err != nil {
			return duplicate(err)
		}
		return found(res)
	})
}

func (r *Repository) DeleteProperty(ctx context.Context, namespaceID int64, name string) error {
	return r.deleteChild(ctx, "DELETE FROM metadef_properties WHERE namespace_id=? AND name=?", namespaceID, name)
}

func (r *Repository) DeleteProperties(ctx context.Context, namespaceID int64) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := r.exec(ctx, tx, "DELETE FROM metadef_properties WHERE namespace_id=?", namespaceID)
		return err
	})
}

// Tags (names are case insensitive, as the original's collation).

func scanTag(s scanner) (model.MetadefTag, error) {
	var t model.MetadefTag
	var updated sql.NullTime
	err := s.Scan(&t.Name, &t.CreatedAt, &updated)
	t.UpdatedAt = nullTime(updated)
	return t, err
}

func (r *Repository) Tags(ctx context.Context, namespaceID int64) ([]model.MetadefTag, error) {
	out := []model.MetadefTag{}
	err := r.rows(ctx, "SELECT name, created_at, updated_at FROM metadef_tags WHERE namespace_id=? ORDER BY created_at DESC, id DESC", []any{namespaceID}, func(s scanner) error {
		t, err := scanTag(s)
		out = append(out, t)
		return err
	})
	return out, err
}

func (r *Repository) Tag(ctx context.Context, namespaceID int64, name string) (model.MetadefTag, error) {
	ctx, cancel := r.bound(ctx)
	defer cancel()
	t, err := scanTag(r.db.QueryRowContext(ctx, r.sql("SELECT name, created_at, updated_at FROM metadef_tags WHERE namespace_id=? AND name=?"), namespaceID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return t, repository.ErrMetadefNotFound
	}
	return t, err
}

func (r *Repository) insertTag(ctx context.Context, tx *sql.Tx, namespaceID int64, t model.MetadefTag) error {
	_, err := r.exec(ctx, tx, "INSERT INTO metadef_tags (namespace_id, name, created_at, updated_at) VALUES (?, ?, ?, ?)", namespaceID, t.Name, t.CreatedAt.UTC(), t.CreatedAt.UTC())
	return duplicate(err)
}

// CreateTags is the original's metadef_tag_create_tags: all or nothing;
// replace first deletes the namespace's tags (no X-Openstack-Append).
func (r *Repository) CreateTags(ctx context.Context, namespaceID int64, tags []model.MetadefTag, replace bool) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if replace {
			if _, err := r.exec(ctx, tx, "DELETE FROM metadef_tags WHERE namespace_id=?", namespaceID); err != nil {
				return err
			}
		}
		for _, t := range tags {
			if err := r.insertTag(ctx, tx, namespaceID, t); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) UpdateTag(ctx context.Context, namespaceID int64, name string, t model.MetadefTag) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := r.exec(ctx, tx, "UPDATE metadef_tags SET name=?, updated_at=? WHERE namespace_id=? AND name=?", t.Name, t.UpdatedAt, namespaceID, name)
		if err != nil {
			return duplicate(err)
		}
		return found(res)
	})
}

func (r *Repository) DeleteTag(ctx context.Context, namespaceID int64, name string) error {
	return r.deleteChild(ctx, "DELETE FROM metadef_tags WHERE namespace_id=? AND name=?", namespaceID, name)
}

func (r *Repository) DeleteTags(ctx context.Context, namespaceID int64) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := r.exec(ctx, tx, "DELETE FROM metadef_tags WHERE namespace_id=?", namespaceID)
		return err
	})
}

// Resource types.

func (r *Repository) ResourceTypes(ctx context.Context) ([]model.ResourceType, error) {
	out := []model.ResourceType{}
	err := r.rows(ctx, "SELECT name, protected, created_at, updated_at FROM metadef_resource_types ORDER BY created_at, id", nil, func(s scanner) error {
		var t model.ResourceType
		var updated sql.NullTime
		err := s.Scan(&t.Name, &t.Protected, &t.CreatedAt, &updated)
		t.UpdatedAt = nullTime(updated)
		out = append(out, t)
		return err
	})
	return out, err
}

// associate creates the resource type when needed (unprotected), as the
// original's resource type association.
func (r *Repository) associate(ctx context.Context, tx *sql.Tx, namespaceID int64, a model.ResourceTypeAssociation) error {
	var id int64
	err := r.queryRow(ctx, tx, "SELECT id FROM metadef_resource_types WHERE name=?", a.Name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, e := r.exec(ctx, tx, "INSERT INTO metadef_resource_types (name, protected, created_at, updated_at) VALUES (?, 0, ?, ?)", a.Name, a.CreatedAt.UTC(), a.CreatedAt.UTC())
		if e != nil {
			return duplicate(e)
		}
		id, err = res.LastInsertId()
	}
	if err != nil {
		return err
	}
	_, err = r.exec(ctx, tx, "INSERT INTO metadef_namespace_resource_types (resource_type_id, namespace_id, properties_target, prefix, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, namespaceID, a.PropertiesTarget, a.Prefix, a.CreatedAt.UTC(), a.CreatedAt.UTC())
	return duplicate(err)
}

func (r *Repository) Associate(ctx context.Context, namespaceID int64, a model.ResourceTypeAssociation) error {
	return r.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error { return r.associate(ctx, tx, namespaceID, a) })
}

func (r *Repository) Dissociate(ctx context.Context, namespaceID int64, name string) error {
	return r.deleteChild(ctx, "DELETE a FROM metadef_namespace_resource_types a JOIN metadef_resource_types t ON t.id=a.resource_type_id WHERE a.namespace_id=? AND t.name=?", namespaceID, name)
}
