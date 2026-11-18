package mysql

import (
	"context"
	"testing"
	"time"
)

// A failed preparation keeps nothing and every query still runs unprepared,
// so correctness never depends on the statement cache.
func TestPrepareStatementsFailureKeepsUnpreparedPath(t *testing.T) {
	db := scriptDB(t, nil) // the script driver refuses Prepare
	r := New(db, time.Second)
	if err := r.PrepareStatements(context.Background()); err == nil || r.stmts != nil {
		t.Fatal("failed preparation must not install a partial cache", err)
	}
	for _, q := range hotStatements {
		if r.stmts[q] != nil {
			t.Fatal("unexpected cached statement", q)
		}
	}
}

// The cached texts must be exactly what the readers build, or nothing is reused.
func TestHotStatementsMatchTheQueriesTheReadersBuild(t *testing.T) {
	want := map[string]bool{}
	for _, q := range hotStatements {
		want[q] = true
	}
	for _, q := range []string{
		"SELECT image_id, name, value FROM image_properties WHERE image_id IN (" + "?" + ") ORDER BY image_id, name",
		"SELECT image_id, value FROM image_tags WHERE image_id IN (" + "?" + ") ORDER BY image_id, value",
		readImageQuery, lockImageQuery, readDataImage, lockDataOperation,
	} {
		if !want[q] {
			t.Fatal("reader query not in the hot set:", q)
		}
	}
}
