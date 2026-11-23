package mysql

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/pyjson"
	driver "github.com/go-sql-driver/mysql"
)

// glance-manage over the catalog database (the DSN names the glance
// database itself): the original's schema at its alembic head, purge, and
// metadata definitions load/unload/export, with the same rules and output.

// CatalogHead is the original's alembic head this schema is.
const CatalogHead = "2024_1_contract01"

//go:embed catalog/2024_1_contract01.sql
var catalogDDL string

// CatalogRevision is the alembic_version of the database ("" if none).
func CatalogRevision(ctx context.Context, db *sql.DB) (string, error) {
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='alembic_version'").Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	var rev string
	err := db.QueryRowContext(ctx, "SELECT version_num FROM alembic_version").Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return rev, err
}

// SyncCatalog is db sync: an empty database gets the original's schema at
// its head; a database at the head is left as is; anything else is refused.
func SyncCatalog(ctx context.Context, db *sql.DB) (bool, error) {
	rev, err := CatalogRevision(ctx, db)
	if err != nil {
		return false, err
	}
	if rev == CatalogHead {
		return false, nil
	}
	if rev != "" {
		return false, fmt.Errorf("database is at revision %s; only an empty database or %s is supported", rev, CatalogHead)
	}
	var tables int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables); err != nil {
		return false, err
	}
	if tables != 0 {
		return false, errors.New("database is not empty and not under migration control")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
		return false, err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SET FOREIGN_KEY_CHECKS=1")
	for _, stmt := range catalogStatements() {
		if _, err = conn.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("create catalog schema: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO alembic_version (version_num) VALUES (?)", CatalogHead)
	return true, err
}

func catalogStatements() []string {
	var lines []string
	for _, l := range strings.Split(catalogDDL, "\n") {
		if !strings.HasPrefix(l, "--") {
			lines = append(lines, l)
		}
	}
	var out []string
	for _, s := range strings.Split(strings.Join(lines, "\n"), ";\n") {
		if s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";")); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// purgeTables are the original's models with a deleted column, in model
// order, without images (purge_images_table) and with tasks last.
var purgeTables = []string{"image_properties", "image_tags", "image_locations", "image_members", "tasks"}

func purgeCutoff(age int) time.Time { return time.Now().UTC().AddDate(0, 0, -age) }

func limitClause(max int) string {
	if max == -1 {
		return ""
	}
	return fmt.Sprintf(" LIMIT %d", max)
}

// Purge is db purge: task_info of old deleted tasks first, then the soft
// deleted rows older than age of every table but images. It returns the
// rows deleted per table.
func Purge(ctx context.Context, db *sql.DB, age, max int) (map[string]int64, error) {
	cutoff := purgeCutoff(age)
	out := map[string]int64{}
	res, err := db.ExecContext(ctx, "DELETE FROM task_info WHERE task_id IN (SELECT task_id FROM (SELECT ti.task_id FROM task_info ti JOIN tasks t ON t.id = ti.task_id WHERE t.deleted_at < ? ORDER BY t.deleted_at"+limitClause(max)+") AS victims)", cutoff)
	if err != nil {
		return out, err
	}
	out["task_info"], _ = res.RowsAffected()
	for _, table := range purgeTables {
		res, err = db.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN (SELECT id FROM (SELECT id FROM "+table+" WHERE deleted_at < ? ORDER BY deleted_at"+limitClause(max)+") AS victims)", cutoff)
		if err != nil {
			return out, referenceError(err)
		}
		out[table], _ = res.RowsAffected()
	}
	return out, nil
}

// PurgeImages is db purge_images_table.
func PurgeImages(ctx context.Context, db *sql.DB, age, max int) (int64, error) {
	res, err := db.ExecContext(ctx, "DELETE FROM images WHERE id IN (SELECT id FROM (SELECT id FROM images WHERE deleted_at < ? ORDER BY deleted_at"+limitClause(max)+") AS victims)", purgeCutoff(age))
	if err != nil {
		return 0, referenceError(err)
	}
	return res.RowsAffected()
}

// ErrPurgeReference is the original's DBReferenceError during a purge.
var ErrPurgeReference = errors.New("Purge command failed, check glance-manage logs for more details.")

func referenceError(err error) error {
	var my *driver.MySQLError
	if errors.As(err, &my) && (my.Number == 1451 || my.Number == 1452) {
		return ErrPurgeReference
	}
	return err
}

// MetadefLoadOptions are load_metadefs' --merge, --prefer_new, --overwrite.
type MetadefLoadOptions struct{ Merge, PreferNew, Overwrite bool }

// LoadMetadefs is db load_metadefs from a directory of *.json (or one file),
// file by file as the original: a new namespace is inserted; an existing one
// is skipped unless merge (children added) or overwrite (replaced).
func LoadMetadefs(ctx context.Context, db *sql.DB, path string, o MetadefLoadOptions, log func(string)) error {
	var files []string
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		files = []string{path}
	} else {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("Json schema files not found in %s. Aborting.", path)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil || !json.Valid(raw) {
			log(fmt.Sprintf("Failed to parse json file %s while populating metadata", f))
			continue
		}
		if err = loadNamespaceFile(ctx, db, raw, o, log); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		log("File " + f + " loaded to database.")
	}
	log("Metadata loading finished")
	return nil
}

type rawNamespace struct {
	Namespace    string  `json:"namespace"`
	DisplayName  *string `json:"display_name"`
	Description  *string `json:"description"`
	Visibility   *string `json:"visibility"`
	Protected    *bool   `json:"protected"`
	Owner        *string `json:"owner"`
	Associations []struct {
		Name             string  `json:"name"`
		Prefix           *string `json:"prefix"`
		PropertiesTarget *string `json:"properties_target"`
	} `json:"resource_type_associations"`
	Properties json.RawMessage `json:"properties"`
	Objects    []struct {
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		Properties  json.RawMessage `json:"properties"`
	} `json:"objects"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
}

func now() time.Time { return time.Now().UTC() }

func loadNamespaceFile(ctx context.Context, db *sql.DB, raw []byte, o MetadefLoadOptions, log func(string)) error {
	var n rawNamespace
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	owner := "admin"
	if n.Owner != nil {
		owner = *n.Owner
	}
	insertIgnore := func(q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		var my *driver.MySQLError
		if errors.As(err, &my) && my.Number == 1062 {
			log("Duplicate entry")
			return nil
		}
		return err
	}
	id, err := lookupID(ctx, db, "SELECT id FROM metadef_namespaces WHERE namespace=?", n.Namespace)
	if err != nil {
		return err
	}
	if id != 0 && o.Overwrite {
		log("Overwriting namespace " + n.Namespace)
		for _, q := range []string{"DELETE FROM metadef_properties WHERE namespace_id=?", "DELETE FROM metadef_objects WHERE namespace_id=?", "DELETE FROM metadef_tags WHERE namespace_id=?", "DELETE FROM metadef_namespace_resource_types WHERE namespace_id=?", "DELETE FROM metadef_namespaces WHERE id=?"} {
			if _, err = db.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		id = 0
	}
	switch {
	case id == 0:
		if err = insertIgnore("INSERT INTO metadef_namespaces (namespace, display_name, description, visibility, protected, owner, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			n.Namespace, n.DisplayName, n.Description, n.Visibility, n.Protected, owner, now()); err != nil {
			return err
		}
		if id, err = lookupID(ctx, db, "SELECT id FROM metadef_namespaces WHERE namespace=?", n.Namespace); err != nil {
			return err
		}
	case !o.Merge:
		log("Skipping namespace " + n.Namespace + ". It already exists in the database.")
		return nil
	case o.PreferNew:
		if _, err = db.ExecContext(ctx, "UPDATE metadef_namespaces SET namespace=?, display_name=?, description=?, visibility=?, protected=?, owner=?, updated_at=? WHERE id=?",
			n.Namespace, n.DisplayName, n.Description, n.Visibility, n.Protected, owner, now(), id); err != nil {
			return err
		}
	}
	for _, a := range n.Associations {
		rt, err := lookupID(ctx, db, "SELECT id FROM metadef_resource_types WHERE name=?", a.Name)
		if err != nil {
			return err
		}
		if rt == 0 {
			if err = insertIgnore("INSERT INTO metadef_resource_types (name, protected, created_at) VALUES (?, 1, ?)", a.Name, now()); err != nil {
				return err
			}
			if rt, err = lookupID(ctx, db, "SELECT id FROM metadef_resource_types WHERE name=?", a.Name); err != nil {
				return err
			}
		} else if o.PreferNew {
			if _, err = db.ExecContext(ctx, "UPDATE metadef_resource_types SET updated_at=? WHERE id=?", now(), rt); err != nil {
				return err
			}
		}
		var exists int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM metadef_namespace_resource_types WHERE namespace_id=? AND resource_type_id=?", id, rt).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if err = insertIgnore("INSERT INTO metadef_namespace_resource_types (resource_type_id, namespace_id, properties_target, prefix, created_at) VALUES (?, ?, ?, ?, ?)", rt, id, a.PropertiesTarget, a.Prefix, now()); err != nil {
				return err
			}
		} else if o.PreferNew {
			if _, err = db.ExecContext(ctx, "UPDATE metadef_namespace_resource_types SET properties_target=?, prefix=?, updated_at=? WHERE resource_type_id=? AND namespace_id=?", a.PropertiesTarget, a.Prefix, now(), rt, id); err != nil {
				return err
			}
		}
	}
	child := func(table, name string, insert string, args ...any) error {
		cid, err := lookupID(ctx, db, "SELECT id FROM "+table+" WHERE namespace_id=? AND name=?", id, name)
		if err != nil {
			return err
		}
		if cid == 0 {
			return insertIgnore(insert, args...)
		}
		return nil
	}
	if len(n.Properties) > 0 && string(n.Properties) != "null" {
		keys, props, err := pyjson.Members(n.Properties)
		if err != nil {
			return err
		}
		for _, name := range keys {
			text, err := pyjson.Dumps(props[name])
			if err != nil {
				return err
			}
			if err = child("metadef_properties", name, "INSERT INTO metadef_properties (namespace_id, name, json_schema, created_at) VALUES (?, ?, ?, ?)", id, name, text, now()); err != nil {
				return err
			}
			if o.PreferNew {
				if _, err = db.ExecContext(ctx, "UPDATE metadef_properties SET json_schema=?, updated_at=? WHERE namespace_id=? AND name=?", text, now(), id, name); err != nil {
					return err
				}
			}
		}
	}
	for _, obj := range n.Objects {
		props := obj.Properties
		if len(props) == 0 {
			props = json.RawMessage("null")
		}
		text, err := pyjson.Dumps(props)
		if err != nil {
			return err
		}
		if err = child("metadef_objects", obj.Name, "INSERT INTO metadef_objects (namespace_id, name, description, json_schema, created_at) VALUES (?, ?, ?, ?, ?)", id, obj.Name, obj.Description, text, now()); err != nil {
			return err
		}
	}
	for _, t := range n.Tags {
		if err = child("metadef_tags", t.Name, "INSERT INTO metadef_tags (namespace_id, name, created_at) VALUES (?, ?, ?)", id, t.Name, now()); err != nil {
			return err
		}
	}
	return nil
}

func lookupID(ctx context.Context, db *sql.DB, q string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, q, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// UnloadMetadefs is db unload_metadefs: every metadef table cleared.
func UnloadMetadefs(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"metadef_properties", "metadef_objects", "metadef_tags", "metadef_namespace_resource_types", "metadef_namespaces", "metadef_resource_types"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	return nil
}

var fileName = regexp.MustCompile(`[\W_]+`)

// ExportMetadefs is db export_metadefs: one file per namespace named after
// its display name, written as the original's json.dumps of the namespace.
// path is joined as the original does (no separator added).
func ExportMetadefs(ctx context.Context, db *sql.DB, path string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, namespace, display_name, description, visibility, protected FROM metadef_namespaces ORDER BY id")
	if err != nil {
		return nil, err
	}
	type ns struct {
		id                               int64
		namespace                        string
		display, description, visibility sql.NullString
		protected                        sql.NullBool
	}
	var list []ns
	for rows.Next() {
		var n ns
		if err = rows.Scan(&n.id, &n.namespace, &n.display, &n.description, &n.visibility, &n.protected); err != nil {
			break
		}
		list = append(list, n)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	str := func(s sql.NullString) string {
		if !s.Valid {
			return "null"
		}
		b, _ := json.Marshal(s.String)
		return string(b)
	}
	var written []string
	for _, n := range list {
		var assocs, props, objects, tags []string
		ar, err := db.QueryContext(ctx, "SELECT t.name, a.prefix, a.properties_target FROM metadef_namespace_resource_types a JOIN metadef_resource_types t ON t.id=a.resource_type_id WHERE a.namespace_id=?", n.id)
		if err != nil {
			return written, err
		}
		for ar.Next() {
			var name string
			var prefix, target sql.NullString
			if err = ar.Scan(&name, &prefix, &target); err != nil {
				break
			}
			b, _ := json.Marshal(name)
			assocs = append(assocs, `{"name": `+string(b)+`, "prefix": `+str(prefix)+`, "properties_target": `+str(target)+`}`)
		}
		if err = errors.Join(err, ar.Err(), ar.Close()); err != nil {
			return written, err
		}
		pr, err := db.QueryContext(ctx, "SELECT name, json_schema FROM metadef_properties WHERE namespace_id=?", n.id)
		if err != nil {
			return written, err
		}
		for pr.Next() {
			var name, schema string
			if err = pr.Scan(&name, &schema); err != nil {
				break
			}
			b, _ := json.Marshal(name)
			s, e := pyjson.Dumps([]byte(schema))
			err = e
			props = append(props, string(b)+": "+s)
		}
		if err = errors.Join(err, pr.Err(), pr.Close()); err != nil {
			return written, err
		}
		or, err := db.QueryContext(ctx, "SELECT name, description, json_schema FROM metadef_objects WHERE namespace_id=?", n.id)
		if err != nil {
			return written, err
		}
		for or.Next() {
			var name, schema string
			var desc sql.NullString
			if err = or.Scan(&name, &desc, &schema); err != nil {
				break
			}
			b, _ := json.Marshal(name)
			s, e := pyjson.Dumps([]byte(schema))
			err = e
			objects = append(objects, `{"name": `+string(b)+`, "description": `+str(desc)+`, "properties": `+s+`}`)
		}
		if err = errors.Join(err, or.Err(), or.Close()); err != nil {
			return written, err
		}
		tr, err := db.QueryContext(ctx, "SELECT name FROM metadef_tags WHERE namespace_id=?", n.id)
		if err != nil {
			return written, err
		}
		for tr.Next() {
			var name string
			if err = tr.Scan(&name); err != nil {
				break
			}
			b, _ := json.Marshal(name)
			tags = append(tags, `{"name": `+string(b)+`}`)
		}
		if err = errors.Join(err, tr.Err(), tr.Close()); err != nil {
			return written, err
		}
		protected := "null"
		if n.protected.Valid {
			protected = fmt.Sprint(n.protected.Bool)
		}
		nsName, _ := json.Marshal(n.namespace)
		doc := `{"namespace": ` + string(nsName) + `, "display_name": ` + str(n.display) + `, "description": ` + str(n.description) +
			`, "visibility": ` + str(n.visibility) + `, "protected": ` + protected +
			`, "resource_type_associations": [` + strings.Join(assocs, ", ") + `], "properties": {` + strings.Join(props, ", ") +
			`}, "objects": [` + strings.Join(objects, ", ") + `], "tags": [` + strings.Join(tags, ", ") + `]}`
		// Strings above were JSON-encoded by Go; normalize the whole document
		// to the original's ensure_ascii form.
		out, err := pyjson.Dumps([]byte(doc))
		if err != nil {
			return written, err
		}
		file := path + fileName.ReplaceAllString(n.display.String, "") + ".json"
		if err = os.WriteFile(file, []byte(out), 0o644); err != nil {
			return written, err
		}
		written = append(written, file)
	}
	sort.Strings(written)
	return written, nil
}
