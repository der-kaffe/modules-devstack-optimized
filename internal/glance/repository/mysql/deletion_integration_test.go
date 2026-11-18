//go:build integration

package mysql_test

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
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/google/uuid"
)

// MUTATING, opt-in, NEW EMPTY independently authorized schema only. NOT EXECUTED
// in Action 7. This checks SQL alone: no filesystem bytes, fsync, Keystone or
// physical durability. CompleteDataCleanup acknowledges an empty test store.
func TestMySQLCoordinatedDeletion(t *testing.T) {
	if os.Getenv("GLANCE_GO_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable MySQL opt-in required")
	}
	secret, err := config.ReadSecret(os.Getenv("GLANCE_GO_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(secret))
	if err = mysql.ValidateTestDSN(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = mysql.CheckAccount(ctx, db, "migrate"); err != nil {
		t.Fatal(err)
	}
	var tables int
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables) != nil || tables != 0 {
		t.Fatal("deletion suite requires a NEW EMPTY independent schema")
	}
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = mysql.VerifySchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := mysql.New(db, 3*time.Second)
	if err := r.PrepareStatements(ctx); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, phase := range []model.DataPhase{"", model.DataReserved, model.DataStaged, model.DataPublished, model.DataCommitted} {
		i, e := model.New(model.CreateInput{Tags: []string{"retain"}, Properties: map[string]string{"retain": "yes"}}, "delete-fixture", time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Create(ctx, i); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, i.ID)
		var attempt *string
		if phase != "" {
			a := uuid.NewString()
			attempt = &a
			if _, e = r.ReserveData(ctx, i.ID, a, nil, time.Now()); e != nil {
				t.Fatal(e)
			}
			if phase != model.DataReserved {
				if e = r.StageData(ctx, i.ID, a, model.DataContent{Size: 20, Checksum: strings.Repeat("a", 32), SHA512: strings.Repeat("b", 128)}, time.Now()); e != nil {
					t.Fatal(e)
				}
			}
			if phase == model.DataPublished || phase == model.DataCommitted {
				if e = r.PublishData(ctx, i.ID, a, time.Now()); e != nil {
					t.Fatal(e)
				}
			}
			if phase == model.DataCommitted {
				if e = r.CommitData(ctx, i.ID, a, time.Now()); e != nil {
					t.Fatal(e)
				}
			}
		}
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for n := 0; n < 2; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, e := r.TombstoneData(ctx, i.ID, attempt, time.Now(), func(current model.Image) error {
					if current.Owner != "delete-fixture" || current.Properties["retain"] != "yes" || len(current.Tags) != 1 {
						return errors.New("locked metadata changed")
					}
					return nil
				})
				errs <- e
			}()
		}
		wg.Wait()
		close(errs)
		success, missing := 0, 0
		for e := range errs {
			if e == nil {
				success++
			} else if errors.Is(e, repository.ErrNotFound) {
				missing++
			} else {
				t.Fatal(e)
			}
		}
		if success != 1 || missing != 1 {
			t.Fatal("concurrent deletes were not serialized", success, missing)
		}
		s, e := r.DataState(ctx, i.ID)
		if e != nil || !s.Deleted || s.Status != "deleted" {
			t.Fatal("missing atomic tombstone", e)
		}
		if attempt == nil {
			if s.Operation != nil {
				t.Fatal("invented operation")
			}
		} else {
			if s.Operation == nil || s.Operation.AttemptID != *attempt || s.Operation.Phase != model.DataCleanup || s.Operation.CleanupTarget == nil || *s.Operation.CleanupTarget != "deleted" {
				t.Fatal("missing atomic cleanup intent")
			}
			if e = r.CommitData(ctx, i.ID, *attempt, time.Now()); !errors.Is(e, repository.ErrConflict) {
				t.Fatal("late commit activated tombstone", e)
			}
			if e = r.CompleteDataCleanup(ctx, i.ID, *attempt, time.Now()); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = r.Get(ctx, i.ID); !errors.Is(e, repository.ErrNotFound) {
			t.Fatal("tombstone visible", e)
		}
		if e = r.Create(ctx, i); !errors.Is(e, repository.ErrDuplicate) {
			t.Fatal("tombstone UUID reused", e)
		}
		var tags, props int
		if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM image_tags WHERE image_id=?", i.ID).Scan(&tags) != nil || db.QueryRowContext(ctx, "SELECT COUNT(*) FROM image_properties WHERE image_id=?", i.ID).Scan(&props) != nil || tags != 1 || props != 1 {
			t.Fatal("tombstone children removed")
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	r = mysql.New(db, 3*time.Second)
	for _, id := range ids {
		s, e := r.DataState(ctx, id)
		if e != nil || !s.Deleted || s.Operation != nil && s.Operation.Phase != model.DataCleaned {
			t.Fatal("tombstone did not persist across pools", e)
		}
	}
}
