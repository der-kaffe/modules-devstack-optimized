//go:build integration

package mysql

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/google/uuid"
)

// MUTATING opt-in, NEW EMPTY dedicated schema. SQL only; synthetic receipts do
// not stand for real bytes. Never share with upgrade/history-fault/API suites.
func TestB25MySQLConcurrentFences(t *testing.T) {
	if os.Getenv("GLANCE_GO_B25_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable B25 MySQL opt-in required")
	}
	b, err := config.ReadSecret(os.Getenv("GLANCE_GO_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(b))
	if err = ValidateTestDSN(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = CheckAccount(ctx, db, "migrate"); err != nil {
		t.Fatal(err)
	}
	var count int
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count) != nil || count != 0 {
		t.Fatal("NEW EMPTY independent schema required")
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := New(db, 5*time.Second)
	if err := r.PrepareStatements(ctx); err != nil {
		t.Fatal(err)
	}
	receipt := model.DataContent{Size: 6, Checksum: strings.Repeat("a", 32), SHA512: strings.Repeat("b", 128)}
	seed := func() (string, string) {
		t.Helper()
		i, err := model.New(model.CreateInput{Properties: map[string]string{"preserve": "yes"}}, "concurrency-project", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
		a := uuid.NewString()
		if _, err = r.ReserveData(ctx, i.ID, a, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		return i.ID, a
	}
	race := func(f, g func() error) []error {
		start := make(chan struct{})
		out := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; out[0] = f() }()
		go func() { defer wg.Done(); <-start; out[1] = g() }()
		close(start)
		wg.Wait()
		return out
	}
	assertOne := func(errs []error) {
		t.Helper()
		ok, conflict := 0, 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if errors.Is(err, repository.ErrConflict) {
				conflict++
			} else {
				t.Fatal("unexpected transaction error", err)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatal("CAS not serialized", ok, conflict)
		}
	}
	id, a := seed()
	assertOne(race(func() error { return r.StageData(ctx, id, a, receipt, time.Now()) }, func() error { return r.StageData(ctx, id, a, receipt, time.Now()) }))
	assertOne(race(func() error { return r.PublishData(ctx, id, a, time.Now()) }, func() error { return r.PublishData(ctx, id, a, time.Now()) }))
	assertOne(race(func() error { return r.CommitData(ctx, id, a, time.Now()) }, func() error { return r.CommitData(ctx, id, a, time.Now()) }))
	if err = r.BeginDataCleanup(ctx, id, a, "queued", nil, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("active was demoted", err)
	}
	id, a = seed()
	if err = r.StageData(ctx, id, a, receipt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.PublishData(ctx, id, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	errs := race(func() error { return r.CommitData(ctx, id, a, time.Now()) }, func() error {
		_, err := r.TombstoneData(ctx, id, &a, time.Now(), func(model.Image) error { return nil })
		return err
	})
	if errs[1] != nil || errs[0] != nil && !errors.Is(errs[0], repository.ErrConflict) {
		t.Fatal("finalizer/delete race", errs)
	}
	s, err := r.DataState(ctx, id)
	if err != nil || !s.Deleted || s.Operation.Phase != model.DataCleanup {
		t.Fatal("tombstone fence lost", err)
	}
	if err = r.CommitData(ctx, id, a, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("late finalizer resurrected tombstone", err)
	}
	// Inject a REAL failing second SQL statement in a transaction (constraint,
	// not a mock acknowledgement). Parent's update must roll back physically.
	id, a = seed()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE images SET name='must-rollback' WHERE id=?", id); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE image_data_operations SET phase='not-a-phase' WHERE image_id=?", id); err == nil {
		_ = tx.Rollback()
		t.Fatal("CHECK not enforced")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var name *string
	if db.QueryRowContext(ctx, "SELECT name FROM images WHERE id=?", id).Scan(&name) != nil || name != nil {
		t.Fatal("partial mutation persisted")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r = New(db, 5*time.Second)
	s, err = r.DataState(ctx, id)
	if err != nil || s.Status != "saving" || s.Operation.AttemptID != a || s.Operation.Phase != model.DataReserved {
		t.Fatal("reopened state differs", err)
	}
	// Lost COMMIT acknowledgement requires a controllable network fault proxy;
	// it is NOT claimed by the SQL constraint or component-reopen checks above.
}
