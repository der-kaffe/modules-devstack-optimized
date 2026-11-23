package mysql

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	mysqldriver "github.com/go-sql-driver/mysql"
)

const catalogID = "00000000-0000-4000-8000-000000000001"

func fixtureRow() []driver.Value {
	return []driver.Value{catalogID, "p", "original", "queued", "shared", nil, nil, int64(0), int64(0), false, false, nil, nil, nil, nil, nil, time.Unix(100, 0).UTC(), time.Unix(100, 0).UTC()}
}
func imageReadSteps(lock bool) []sqlStep {
	// One round trip: the image row carries its properties and tags as JSON.
	q := "SELECT " + imageAlias + childColumns + " FROM images i WHERE i.id=? AND i.deleted=FALSE"
	if lock {
		q += " FOR UPDATE"
	}
	return []sqlStep{{query: q, rows: [][]driver.Value{append(fixtureRow(), `{"x": "original"}`, `["a"]`)}}}
}

const updateImageSQL = "UPDATE images SET name=?, visibility=?, disk_format=?, container_format=?, min_disk=?, min_ram=?, protected=?, os_hidden=?, status=?, updated_at=? WHERE id=? AND deleted=FALSE AND status=?"
const deleteImageSQL = "UPDATE images SET deleted=TRUE, deleted_at=?, status='deleted', updated_at=? WHERE id=? AND deleted=FALSE"

func patchSteps() []sqlStep {
	steps := []sqlStep{execute("BEGIN")}
	steps = append(steps, imageReadSteps(true)...)
	return append(steps, execute(updateImageSQL), execute("DELETE FROM image_properties WHERE image_id=?"), execute("DELETE FROM image_tags WHERE image_id=?"), execute("INSERT INTO image_properties (image_id, name, value, created_at, deleted) VALUES (?, ?, ?, UTC_TIMESTAMP(), 0)"), execute("INSERT INTO image_tags (image_id, value, created_at, deleted) VALUES (?, ?, UTC_TIMESTAMP(), 0)"), execute("COMMIT"))
}
func changed(i model.Image) (model.Image, error) {
	return model.ApplyPatch(i, []model.PatchOperation{{Op: "replace", Key: "x", Value: json.RawMessage(`"changed"`)}}, time.Unix(200, 0))
}

func TestSQLPatchCommitsAndRollsBackFailures(t *testing.T) {
	steps := patchSteps()
	steps[2].checkArgs = func(args []driver.NamedValue) {
		if len(args) != 12 || args[10].Value != catalogID || args[11].Value != "queued" {
			t.Error("update identity not bound")
		}
	}
	db := scriptDB(t, steps)
	got, err := testRepo(db, time.Second).Update(context.Background(), catalogID, changed)
	if err != nil || got.Properties["x"] != "changed" {
		t.Fatal(got, err)
	}
	for _, at := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		t.Run("failure-"+string(rune('a'+at)), func(t *testing.T) {
			steps := patchSteps()
			steps[at].err = errors.New("isolated SQL failure")
			steps = steps[:at+1]
			if at != 0 && at != 7 {
				steps = append(steps, execute("ROLLBACK"))
			}
			db := scriptDB(t, steps)
			if _, err := testRepo(db, time.Second).Update(context.Background(), catalogID, changed); err == nil {
				t.Fatal("SQL failure reported success")
			}
		})
	}
	t.Run("zero affected", func(t *testing.T) {
		steps := patchSteps()
		steps[2].affected = 0
		steps = append(steps[:3], execute("ROLLBACK"))
		db := scriptDB(t, steps)
		if _, err := testRepo(db, time.Second).Update(context.Background(), catalogID, changed); !errors.Is(err, repository.ErrConflict) {
			t.Fatal(err)
		}
	})
	// As official Glance (oslo.db retry_on_deadlock): the deadlock victim is
	// rolled back and the WHOLE transaction re-runs, re-reading under lock.
	t.Run("deadlock is retried", func(t *testing.T) {
		steps := patchSteps()
		steps[4].err = &mysqldriver.MySQLError{Number: 1213}
		steps = append(append(steps[:5], execute("ROLLBACK")), patchSteps()...)
		db := scriptDB(t, steps)
		if got, err := testRepo(db, time.Second).Update(context.Background(), catalogID, changed); err != nil || got.Properties["x"] != "changed" {
			t.Fatal("deadlock victim not re-run", err)
		}
	})
}

func TestSQLPatchInvalidSecondOperationDoesNotWrite(t *testing.T) {
	steps := append([]sqlStep{execute("BEGIN")}, imageReadSteps(true)...)
	steps = append(steps, execute("ROLLBACK"))
	db := scriptDB(t, steps)
	_, err := testRepo(db, time.Second).Update(context.Background(), catalogID, func(i model.Image) (model.Image, error) {
		return model.ApplyPatch(i, []model.PatchOperation{{Op: "replace", Key: "name", Value: json.RawMessage(`"must-rollback"`)}, {Op: "replace", Key: "min_ram", Value: json.RawMessage(`-1`)}}, time.Now())
	})
	var d *model.Error
	if !errors.As(err, &d) || d.Kind != model.Invalid {
		t.Fatal(err)
	}
}

func TestSQLDeleteRetainsChildrenAndRequiresAcknowledgement(t *testing.T) {
	for _, tc := range []string{"commit", "policy", "not-found", "zero-affected", "SQL-failure"} {
		t.Run(tc, func(t *testing.T) {
			steps := append([]sqlStep{execute("BEGIN")}, imageReadSteps(true)...)
			if tc == "not-found" {
				steps = steps[:2]
				steps[1].rows = nil
				steps = append(steps, execute("ROLLBACK"))
			} else if tc == "policy" {
				steps = append(steps, execute("ROLLBACK"))
			} else {
				step := execute(deleteImageSQL)
				if tc == "zero-affected" {
					step.affected = 0
				}
				if tc == "SQL-failure" {
					step.err = errors.New("SQL failure")
				}
				steps = append(steps, step)
				if tc == "commit" {
					steps = append(append(steps, destroySteps()...), execute("COMMIT"))
				} else {
					steps = append(steps, execute("ROLLBACK"))
				}
			}
			db := scriptDB(t, steps)
			err := testRepo(db, time.Second).Delete(context.Background(), catalogID, time.Now(), func(i model.Image) error {
				if tc == "policy" {
					return model.Fail(model.Forbidden, "protected")
				}
				return nil
			})
			if (err == nil) != (tc == "commit") {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLListScopesFiltersMarkersAndReadback(t *testing.T) {
	o, err := (model.ListOptions{Limit: 2, ProjectID: "p", Filters: map[string]string{"protected": "true", "description": "' OR 1=1", "visibility": "all"}, Tags: []string{"a", "b"}, Sort: []model.Sort{{Key: "name", Direction: "asc"}}}).Validate()
	if err != nil {
		t.Fatal(err)
	}
	marker := model.Image{ID: catalogID, Owner: "p", CreatedAt: time.Unix(100, 0)}
	q, args := listQuery(o, &marker)
	if strings.Contains(q, "' OR 1=1") || !strings.Contains(q, "i.deleted=FALSE") || !strings.Contains(q, "i.owner=? OR i.visibility IN ('public','community')") || !strings.Contains(q, "i.name IS NOT NULL") || !strings.Contains(q, " <=> ?") || args[len(args)-1] != 2 {
		t.Fatal(q, args)
	}
	trueBound := false
	for _, v := range args {
		if v == true {
			trueBound = true
		}
	}
	if !trueBound {
		t.Fatal("boolean filter not bound as boolean")
	}
	defaultO, _ := (model.ListOptions{Limit: 25, ProjectID: "p"}).Validate()
	dq, _ := listQuery(defaultO, nil)
	if !strings.Contains(dq, "i.owner=? OR i.visibility<>'community'") {
		t.Fatal("community included by default")
	}
	admin := defaultO
	admin.Admin = true
	aq, _ := listQuery(admin, nil)
	if strings.Contains(aq, "i.visibility IN") {
		t.Fatal("admin visibility filter incorrect")
	}
	for _, tc := range []string{"empty", "multiple", "missing-marker", "foreign-marker", "row-error", "properties-error", "query-error", "commit-error"} {
		t.Run(tc, func(t *testing.T) {
			o := defaultO
			steps := []sqlStep{execute("BEGIN")}
			if tc == "missing-marker" {
				o.Marker = catalogID
				steps = append(steps, sqlStep{query: fullImageQuery}, execute("ROLLBACK"))
			} else if tc == "foreign-marker" {
				o.Marker = catalogID
				m := imageReadSteps(false)
				m[0].rows[0][1] = "other"
				steps = append(steps, m...)
				// a shared image of another project: not a member
				steps = append(steps, query("SELECT COUNT(*) FROM image_members WHERE image_id=? AND member=? AND deleted=0", int64(0)))
				steps = append(steps, execute("ROLLBACK"))
			} else {
				q, _ := listQuery(o, nil)
				step := sqlStep{query: q}
				if tc == "multiple" {
					step.rows = [][]driver.Value{append(fixtureRow(), `{"x": "original"}`, `["a"]`)}
				}
				if tc == "properties-error" { // unreadable aggregated children fail the read
					step.rows = [][]driver.Value{append(fixtureRow(), `{broken`, nil)}
				}
				if tc == "row-error" {
					step.rowsErr = errors.New("cursor failed")
				}
				if tc == "query-error" {
					step.err = errors.New("query failed")
				}
				steps = append(steps, step)
				if tc == "empty" || tc == "multiple" || tc == "commit-error" {
					step := execute("COMMIT")
					if tc == "commit-error" {
						step.err = errors.New("commit failed")
					}
					steps = append(steps, step)
				} else {
					steps = append(steps, execute("ROLLBACK"))
				}
			}
			db := scriptDB(t, steps)
			images, err := testRepo(db, time.Second).List(context.Background(), o)
			if tc == "empty" {
				if err != nil || images == nil || len(images) != 0 {
					t.Fatal(images, err)
				}
			} else if tc == "multiple" {
				if err != nil || len(images) != 1 || !reflect.DeepEqual(images[0].Properties, map[string]string{"x": "original"}) || len(images[0].Tags) != 1 {
					t.Fatal(images, err)
				}
			} else if err == nil {
				t.Fatal("failure reported success")
			}
		})
	}
	t.Run("zero limit", func(t *testing.T) {
		o := defaultO
		o.Limit = 0
		q, _ := listQuery(o, nil)
		db := scriptDB(t, []sqlStep{execute("BEGIN"), {query: q}, execute("COMMIT")})
		images, err := testRepo(db, time.Second).List(context.Background(), o)
		if err != nil || len(images) != 0 {
			t.Fatal(images, err)
		}
	})
}

// M1: several properties go in ONE multi-row INSERT, sorted by key, with the
// image id repeated per row; a single tag keeps the single-row statement.
func TestUpdateWritesChildrenWithMultiRowInserts(t *testing.T) {
	addThree := func(i model.Image) (model.Image, error) {
		return model.ApplyPatch(i, []model.PatchOperation{
			{Op: "add", Key: "z", Value: json.RawMessage(`"3"`)},
			{Op: "add", Key: "b", Value: json.RawMessage(`"2"`)},
		}, time.Unix(200, 0))
	}
	steps := []sqlStep{execute("BEGIN")}
	steps = append(steps, imageReadSteps(true)...)
	props := execute("INSERT INTO image_properties (image_id, name, value, created_at, deleted) VALUES (?, ?, ?, UTC_TIMESTAMP(), 0), (?, ?, ?, UTC_TIMESTAMP(), 0), (?, ?, ?, UTC_TIMESTAMP(), 0)")
	props.checkArgs = func(args []driver.NamedValue) {
		want := []string{catalogID, "b", "2", catalogID, "x", "original", catalogID, "z", "3"}
		if len(args) != len(want) {
			t.Fatalf("args %v", args)
		}
		for n, a := range args {
			if a.Value != want[n] {
				t.Fatalf("arg %d = %v, want %v", n, a.Value, want[n])
			}
		}
	}
	steps = append(steps, execute(updateImageSQL), execute("DELETE FROM image_properties WHERE image_id=?"), execute("DELETE FROM image_tags WHERE image_id=?"), props, execute("INSERT INTO image_tags (image_id, value, created_at, deleted) VALUES (?, ?, UTC_TIMESTAMP(), 0)"), execute("COMMIT"))
	got, err := testRepo(scriptDB(t, steps), time.Second).Update(context.Background(), catalogID, addThree)
	if err != nil || got.Properties["b"] != "2" || got.Properties["z"] != "3" || got.Properties["x"] != "original" {
		t.Fatal(got.Properties, err)
	}
}
