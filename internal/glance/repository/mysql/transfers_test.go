package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	mysqldriver "github.com/go-sql-driver/mysql"
)

const attemptA = "00000000-0000-4000-8000-000000000002"
const attemptB = "00000000-0000-4000-8000-000000000003"

var dataNow = time.Unix(200, 123456789).UTC()

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }
func receipt() model.DataContent {
	return model.DataContent{Size: 20, VirtualSize: i64(20), Checksum: strings.Repeat("a", 32), SHA512: strings.Repeat("b", 128)}
}
func dataFixture(phase model.DataPhase) *model.DataOperation {
	o := &model.DataOperation{ImageID: catalogID, AttemptID: attemptA, Phase: phase, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(100, 0).UTC()}
	if phase != model.DataReserved {
		c := receipt()
		o.Size = i64(c.Size)
		o.VirtualSize = c.VirtualSize
		o.Checksum = &c.Checksum
		o.SHA512 = &c.SHA512
	}
	if phase == model.DataCleanup || phase == model.DataCleaned {
		o.CleanupTarget = str("queued")
	}
	return o
}
func ptrValue[T int64 | string](v *T) driver.Value {
	if v == nil {
		return nil
	}
	return *v
}
func operationRow(o *model.DataOperation) []driver.Value {
	return []driver.Value{o.ImageID, o.AttemptID, string(o.Phase), ptrValue(o.ExpectedSize), ptrValue(o.Size), ptrValue(o.VirtualSize), ptrValue(o.Checksum), ptrValue(o.SHA512), ptrValue(o.CleanupTarget), ptrValue(o.LastErrorCode), o.CreatedAt, o.UpdatedAt}
}
func transferSteps(status string, deleted bool, o *model.DataOperation) []sqlStep {
	read := sqlStep{query: lockDataOperation}
	if o != nil {
		read.rows = [][]driver.Value{operationRow(o)}
	}
	return []sqlStep{execute("BEGIN"), query(lockDataImage, catalogID, status, deleted, nil, nil, nil, nil, nil, time.Unix(100, 0).UTC()), read}
}

const reserveImageSQL = "UPDATE images SET status='saving', updated_at=? WHERE id=? AND status='queued' AND deleted=FALSE"
const reserveOperationSQL = "INSERT INTO image_data_operations (image_id, attempt_id, phase, expected_size, created_at, updated_at) VALUES (?, ?, 'reserved', ?, ?, ?)"
const reuseOperationSQL = "UPDATE image_data_operations SET attempt_id=?, phase='reserved', expected_size=?, size=NULL, virtual_size=NULL, checksum=NULL, os_hash_value=NULL, cleanup_target=NULL, last_error_code=NULL, created_at=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase='cleaned'"
const stageOperationSQL = "UPDATE image_data_operations SET phase='staged', size=?, virtual_size=?, checksum=?, os_hash_value=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase='reserved'"
const publishOperationSQL = "UPDATE image_data_operations SET phase='published', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='staged'"
const commitImageSQL = "UPDATE images SET status='active', size=?, virtual_size=?, checksum=?, os_hash_algo='sha512', os_hash_value=?, updated_at=? WHERE id=? AND status='saving' AND deleted=FALSE"
const commitOperationSQL = "UPDATE image_data_operations SET phase='committed', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='published'"
const cleanupOperationSQL = "UPDATE image_data_operations SET phase='cleanup', cleanup_target=?, last_error_code=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase=?"
const restoreImageSQL = "UPDATE images SET status=?, size=NULL, virtual_size=NULL, checksum=NULL, os_hash_algo=NULL, os_hash_value=NULL, updated_at=? WHERE id=? AND status='saving' AND deleted=FALSE"
const cleanedOperationSQL = "UPDATE image_data_operations SET phase='cleaned', updated_at=? WHERE image_id=? AND attempt_id=? AND phase='cleanup'"

type transferCall func(*Repository) error

func TestTransferTransitionsBindAndCommit(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, status string
		deleted      bool
		o            *model.DataOperation
		writes       []string
		call         transferCall
	}{
		{"reserve", "queued", false, nil, []string{reserveImageSQL, reserveOperationSQL}, func(r *Repository) error {
			o, e := r.ReserveData(ctx, catalogID, attemptB, i64(20), dataNow)
			if e == nil && (o.Phase != model.DataReserved || o.AttemptID != attemptB || o.ExpectedSize == nil || *o.ExpectedSize != 20) {
				t.Error(o)
			}
			return e
		}},
		{"reuse-cleaned", "queued", false, dataFixture(model.DataCleaned), []string{reserveImageSQL, reuseOperationSQL}, func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptB, nil, dataNow); return e }},
		{"stage", "saving", false, dataFixture(model.DataReserved), []string{stageOperationSQL}, func(r *Repository) error { return r.StageData(ctx, catalogID, attemptA, receipt(), dataNow) }},
		{"publish", "saving", false, dataFixture(model.DataStaged), []string{publishOperationSQL}, func(r *Repository) error { return r.PublishData(ctx, catalogID, attemptA, dataNow) }},
		{"commit", "saving", false, dataFixture(model.DataPublished), []string{commitImageSQL, commitLocationSQL, commitOperationSQL}, func(r *Repository) error { return r.CommitData(ctx, catalogID, attemptA, dataNow) }},
		{"begin-cleanup", "saving", false, dataFixture(model.DataPublished), []string{cleanupOperationSQL}, func(r *Repository) error {
			return r.BeginDataCleanup(ctx, catalogID, attemptA, "queued", str("recovery"), dataNow)
		}},
		{"cleanup-tombstone", "deleted", true, dataFixture(model.DataCommitted), []string{cleanupOperationSQL}, func(r *Repository) error {
			return r.BeginDataCleanup(ctx, catalogID, attemptA, "deleted", str("deleted"), dataNow)
		}},
		{"restore-queued", "saving", false, dataFixture(model.DataCleanup), []string{restoreImageSQL, cleanedOperationSQL}, func(r *Repository) error { return r.CompleteDataCleanup(ctx, catalogID, attemptA, dataNow) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := transferSteps(tc.status, tc.deleted, tc.o)
			for _, q := range tc.writes {
				step := execute(q)
				step.checkArgs = func(args []driver.NamedValue) {
					bound := false
					for _, v := range args {
						if v.Value == catalogID {
							bound = true
						}
						if at, ok := v.Value.(time.Time); ok && (at.Location() != time.UTC || !at.After(time.Unix(100, 0))) {
							t.Error("invalid timestamp", at)
						}
					}
					if !bound {
						t.Error("image not bound")
					}
					if q == commitImageSQL && (strings.Contains(q, "name=") || strings.Contains(q, "tags") || strings.Contains(q, "properties")) {
						t.Error("finalization overwrites editable metadata")
					}
					if q != reserveImageSQL && q != commitImageSQL && q != commitLocationSQL && q != restoreImageSQL {
						attempt := attemptA
						if tc.name == "reserve" || tc.name == "reuse-cleaned" {
							attempt = attemptB
						}
						found := false
						for _, v := range args {
							if v.Value == attempt {
								found = true
							}
						}
						if !found {
							t.Error("attempt not bound")
						}
					}
				}
				steps = append(steps, step)
			}
			steps = append(steps, execute("COMMIT"))
			if err := tc.call(testRepo(scriptDB(t, steps), time.Second)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTransferFencesRejectWithoutWrites(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, status string
		deleted      bool
		o            *model.DataOperation
		call         transferCall
	}{
		{"second-reservation", "saving", false, dataFixture(model.DataReserved), func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptB, nil, dataNow); return e }},
		{"cleanup-blocks-reserve", "queued", false, dataFixture(model.DataCleanup), func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptB, nil, dataNow); return e }},
		{"same-attempt-reuse", "queued", false, dataFixture(model.DataCleaned), func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptA, nil, dataNow); return e }},
		{"deleted-reservation", "deleted", true, nil, func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptB, nil, dataNow); return e }},
		{"late-finalizer", "saving", false, dataFixture(model.DataPublished), func(r *Repository) error { return r.CommitData(ctx, catalogID, attemptB, dataNow) }},
		{"commit-before-publish", "saving", false, dataFixture(model.DataStaged), func(r *Repository) error { return r.CommitData(ctx, catalogID, attemptA, dataNow) }},
		{"publish-before-stage", "saving", false, dataFixture(model.DataReserved), func(r *Repository) error { return r.PublishData(ctx, catalogID, attemptA, dataNow) }},
		{"stage-wrong-attempt", "saving", false, dataFixture(model.DataReserved), func(r *Repository) error { return r.StageData(ctx, catalogID, attemptB, receipt(), dataNow) }},
		{"late-commit-after-cleanup", "saving", false, dataFixture(model.DataCleanup), func(r *Repository) error { return r.CommitData(ctx, catalogID, attemptA, dataNow) }},
		{"late-commit-after-delete", "deleted", true, dataFixture(model.DataPublished), func(r *Repository) error { return r.CommitData(ctx, catalogID, attemptA, dataNow) }},
		{"active-not-restored", "active", false, dataFixture(model.DataCleanup), func(r *Repository) error { return r.CompleteDataCleanup(ctx, catalogID, attemptA, dataNow) }},
		{"tombstone-not-restored", "deleted", true, dataFixture(model.DataCleanup), func(r *Repository) error { return r.CompleteDataCleanup(ctx, catalogID, attemptA, dataNow) }},
		{"cannot-clean-active", "active", false, dataFixture(model.DataCommitted), func(r *Repository) error { return r.BeginDataCleanup(ctx, catalogID, attemptA, "queued", nil, dataNow) }},
		{"cleanup-target-no-tombstone", "saving", false, dataFixture(model.DataStaged), func(r *Repository) error {
			return r.BeginDataCleanup(ctx, catalogID, attemptA, "deleted", nil, dataNow)
		}},
		{"cleaned-wrong-attempt", "saving", false, dataFixture(model.DataCleanup), func(r *Repository) error { return r.CompleteDataCleanup(ctx, catalogID, attemptB, dataNow) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := append(transferSteps(tc.status, tc.deleted, tc.o), execute("ROLLBACK"))
			if err := tc.call(testRepo(scriptDB(t, steps), time.Second)); err == nil {
				t.Fatal("invalid state/fence accepted")
			}
		})
	}
	t.Run("expected-size-mismatch", func(t *testing.T) {
		o := dataFixture(model.DataReserved)
		o.ExpectedSize = i64(21)
		steps := append(transferSteps("saving", false, o), execute("ROLLBACK"))
		if testRepo(scriptDB(t, steps), time.Second).StageData(ctx, catalogID, attemptA, receipt(), dataNow) == nil {
			t.Fatal("mismatch accepted")
		}
	})
}

func TestCleanupPreservesTombstoneAndAllowsOnlyTerminalDestinations(t *testing.T) {
	ctx := context.Background()
	for _, target := range []string{"deleted", "killed"} {
		t.Run(target, func(t *testing.T) {
			o := dataFixture(model.DataCleanup)
			o.CleanupTarget = &target
			status, deleted := "saving", false
			if target == "deleted" {
				status, deleted = "deleted", true
			}
			steps := transferSteps(status, deleted, o)
			if !deleted {
				steps = append(steps, execute(restoreImageSQL))
			}
			steps = append(steps, execute(cleanedOperationSQL), execute("COMMIT"))
			if err := testRepo(scriptDB(t, steps), time.Second).CompleteDataCleanup(ctx, catalogID, attemptA, dataNow); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("repeated-cleanup-intent", func(t *testing.T) {
		steps := append(transferSteps("saving", false, dataFixture(model.DataCleanup)), execute("COMMIT"))
		if err := testRepo(scriptDB(t, steps), time.Second).BeginDataCleanup(ctx, catalogID, attemptA, "queued", nil, dataNow); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delete-supersedes-recovery", func(t *testing.T) {
		steps := append(transferSteps("deleted", true, dataFixture(model.DataCleanup)), execute(cleanupOperationSQL), execute("COMMIT"))
		if err := testRepo(scriptDB(t, steps), time.Second).BeginDataCleanup(ctx, catalogID, attemptA, "deleted", nil, dataNow); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTransferSQLFailuresAndUncertainCommits(t *testing.T) {
	t.Run("deadlock is retried", func(t *testing.T) {
		attempt := func() []sqlStep {
			return append(transferSteps("saving", false, dataFixture(model.DataPublished)), execute(commitImageSQL), execute(commitLocationSQL), execute(commitOperationSQL), execute("COMMIT"))
		}
		first := attempt()
		first[3].err = &mysqldriver.MySQLError{Number: 1213}
		steps := append(append(first[:4], execute("ROLLBACK")), attempt()...)
		if err := testRepo(scriptDB(t, steps), time.Second).CommitData(context.Background(), catalogID, attemptA, dataNow); err != nil {
			t.Fatal("deadlock victim not re-run", err)
		}
	})
	for _, tc := range []string{"begin", "image-read", "operation-read", "image-update", "operation-update", "commit", "zero-CAS"} {
		t.Run(tc, func(t *testing.T) {
			steps := append(transferSteps("saving", false, dataFixture(model.DataPublished)), execute(commitImageSQL), execute(commitLocationSQL), execute(commitOperationSQL), execute("COMMIT"))
			at := map[string]int{"begin": 0, "image-read": 1, "operation-read": 2, "image-update": 3, "operation-update": 5, "commit": 6, "zero-CAS": 5}[tc]
			if tc == "zero-CAS" {
				steps[at].affected = 0
			} else {
				steps[at].err = errors.New("private db failure")
			}
			steps = steps[:at+1]
			if at > 0 && at < 6 {
				steps = append(steps, execute("ROLLBACK"))
			}
			err := testRepo(scriptDB(t, steps), time.Second).CommitData(context.Background(), catalogID, attemptA, dataNow)
			if err == nil {
				t.Fatal("SQL failure reported success")
			}
			if tc == "commit" && (!errors.Is(err, repository.ErrCommitUncertain) || strings.Contains(err.Error(), "private")) {
				t.Fatal("unknown commit not classified/sanitized", err)
			}
			if tc == "zero-CAS" && !errors.Is(err, repository.ErrConflict) {
				t.Fatal(err)
			}
		})
	}
	t.Run("reserve-commit-unknown-retains-key", func(t *testing.T) {
		finish := execute("COMMIT")
		finish.err = errors.New("commit ack lost")
		steps := append(transferSteps("queued", false, nil), execute(reserveImageSQL), execute(reserveOperationSQL), finish)
		o, err := testRepo(scriptDB(t, steps), time.Second).ReserveData(context.Background(), catalogID, attemptB, nil, dataNow)
		if !errors.Is(err, repository.ErrCommitUncertain) || o.AttemptID != attemptB {
			t.Fatal(o, err)
		}
	})
}

func TestAuthoritativeDataReadAndRecoveryInventory(t *testing.T) {
	o := dataFixture(model.DataCommitted)
	steps := append(transferSteps("active", false, o), execute("COMMIT"))
	state, err := testRepo(scriptDB(t, steps), time.Second).DataState(context.Background(), catalogID)
	if err != nil || state.Status != "active" || !reflect.DeepEqual(state.Operation, o) {
		t.Fatal(state, err)
	}
	query := recoveryCandidatesSQL
	step := sqlStep{query: query, rows: [][]driver.Value{operationRow(o)}, checkArgs: func(a []driver.NamedValue) {
		if len(a) != 2 || a[0].Value != "" || a[1].Value != int64(10) {
			t.Error(a)
		}
	}}
	records, err := testRepo(scriptDB(t, []sqlStep{step}), time.Second).ListDataOperations(context.Background(), "", 10)
	if err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], *o) {
		t.Fatal(records, err)
	}
	t.Run("missing-image", func(t *testing.T) {
		steps := []sqlStep{execute("BEGIN"), {query: lockDataImage}, execute("ROLLBACK")}
		if _, err := testRepo(scriptDB(t, steps), time.Second).DataState(context.Background(), catalogID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func TestTransferInputRejectionDoesNotOpenDatabase(t *testing.T) {
	// A nil pool ensures these failures occur before SQL, not after a mutation.
	r := testRepo(nil, time.Second)
	ctx := context.Background()
	calls := []transferCall{
		func(r *Repository) error { _, e := r.ReserveData(ctx, "bad", attemptA, nil, dataNow); return e },
		func(r *Repository) error { _, e := r.ReserveData(ctx, catalogID, attemptA, i64(-1), dataNow); return e },
		func(r *Repository) error { return r.StageData(ctx, catalogID, attemptA, model.DataContent{}, dataNow) },
		func(r *Repository) error { return r.PublishData(ctx, catalogID, "bad", dataNow) },
		func(r *Repository) error { return r.CommitData(ctx, "bad", attemptA, dataNow) },
		func(r *Repository) error { return r.BeginDataCleanup(ctx, catalogID, attemptA, "active", nil, dataNow) },
		func(r *Repository) error {
			return r.BeginDataCleanup(ctx, catalogID, attemptA, "queued", str("raw database password"), dataNow)
		},
		func(r *Repository) error { return r.CompleteDataCleanup(ctx, catalogID, "bad", dataNow) },
		func(r *Repository) error { _, e := r.DataState(ctx, "bad"); return e },
		func(r *Repository) error { _, e := r.ListDataOperations(ctx, "", 0); return e },
		func(r *Repository) error { _, e := r.ListDataOperations(ctx, "bad", 10); return e },
	}
	for _, call := range calls {
		if call(r) == nil {
			t.Fatal("invalid transfer request accepted")
		}
	}
}

func TestTransferOperationFailuresRollbackWithoutConfirmingCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		o            *model.DataOperation
		writes       []string
		call         transferCall
	}{
		{"stage", "saving", dataFixture(model.DataReserved), []string{stageOperationSQL}, func(r *Repository) error {
			return r.StageData(context.Background(), catalogID, attemptA, receipt(), dataNow)
		}},
		{"publish", "saving", dataFixture(model.DataStaged), []string{publishOperationSQL}, func(r *Repository) error { return r.PublishData(context.Background(), catalogID, attemptA, dataNow) }},
		{"begin-cleanup", "saving", dataFixture(model.DataStaged), []string{cleanupOperationSQL}, func(r *Repository) error {
			return r.BeginDataCleanup(context.Background(), catalogID, attemptA, "queued", nil, dataNow)
		}},
		{"complete-cleanup", "saving", dataFixture(model.DataCleanup), []string{restoreImageSQL, cleanedOperationSQL}, func(r *Repository) error {
			return r.CompleteDataCleanup(context.Background(), catalogID, attemptA, dataNow)
		}},
	} {
		for _, failure := range []string{"SQL-error", "zero-CAS", "commit-unknown"} {
			t.Run(tc.name+"/"+failure, func(t *testing.T) {
				steps := transferSteps(tc.status, false, tc.o)
				for n, q := range tc.writes {
					step := execute(q)
					if n == len(tc.writes)-1 && failure != "commit-unknown" {
						if failure == "zero-CAS" {
							step.affected = 0
						} else {
							step.err = errors.New("injected SQL error")
						}
					}
					steps = append(steps, step)
				}
				if failure == "commit-unknown" {
					step := execute("COMMIT")
					step.err = errors.New("ack lost")
					steps = append(steps, step)
				} else {
					steps = append(steps, execute("ROLLBACK"))
				}
				err := tc.call(testRepo(scriptDB(t, steps), time.Second))
				if err == nil {
					t.Fatal("failure acknowledged as success")
				}
				if failure == "commit-unknown" && !errors.Is(err, repository.ErrCommitUncertain) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRecoveryInventoryFailsClosed(t *testing.T) {
	q := recoveryCandidatesSQL
	for _, label := range []string{"empty", "query-error", "cursor-error", "bad-receipt"} {
		t.Run(label, func(t *testing.T) {
			step := sqlStep{query: q}
			switch label {
			case "query-error":
				step.err = errors.New("injected query failure")
			case "cursor-error":
				step.rowsErr = errors.New("injected cursor failure")
			case "bad-receipt":
				o := dataFixture(model.DataStaged)
				o.SHA512 = str("invalid")
				step.rows = [][]driver.Value{operationRow(o)}
			}
			got, err := testRepo(scriptDB(t, []sqlStep{step}), time.Second).ListDataOperations(context.Background(), "", 10)
			if label == "empty" {
				if err != nil || got == nil || len(got) != 0 {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("failed inventory reported success")
			}
		})
	}
}

const finishLocationSQL = "INSERT INTO image_locations (image_id, value, meta_data, status, created_at, updated_at, deleted) VALUES (?, ?, ?, 'active', ?, ?, 0)"
const commitLocationSQL = "INSERT INTO image_locations (image_id, value, meta_data, status, created_at, updated_at, deleted) VALUES (?, ?, '{}', 'active', ?, ?, 0)"

func TestForgetPurgedDataOnlyForMissingImages(t *testing.T) {
	count := "SELECT COUNT(*) FROM images WHERE id=? FOR UPDATE"
	forget := "DELETE FROM image_data_operations WHERE image_id=? AND attempt_id=?"
	steps := []sqlStep{execute("BEGIN"), query(count, int64(0)), execute(forget), execute("COMMIT")}
	if err := testRepo(scriptDB(t, steps), time.Second).ForgetPurgedData(context.Background(), catalogID, attemptA); err != nil {
		t.Fatal(err)
	}
	live := []sqlStep{execute("BEGIN"), query(count, int64(1)), execute("ROLLBACK")}
	if err := testRepo(scriptDB(t, live), time.Second).ForgetPurgedData(context.Background(), catalogID, attemptA); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("live image bookkeeping forgotten", err)
	}
	if testRepo(nil, time.Second).ForgetPurgedData(context.Background(), "bad", attemptA) == nil {
		t.Fatal("invalid ID accepted")
	}
}

// Like oslo.db, a statement waits for a slow MySQL (a commit stalled by disk
// writeback in the v6 campaign) within the deadline of the request that
// issued it; r.timeout only bounds callers without a deadline.
func TestSlowCommitWaitsWithinTheRequestDeadline(t *testing.T) {
	steps := func() []sqlStep {
		s := append(transferSteps("saving", false, dataFixture(model.DataPublished)), execute(commitImageSQL), execute(commitLocationSQL), execute(commitOperationSQL), execute("COMMIT"))
		s[3].delay = 300 * time.Millisecond
		return s
	}
	request, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := testRepo(scriptDB(t, steps()), 100*time.Millisecond).CommitData(request, catalogID, attemptA, dataNow); err != nil {
		t.Fatal("slow commit failed inside the request deadline", err)
	}
	// The request deadline itself still applies: see TestBound… (database/sql
	// rolls back asynchronously on expiry, which this script double cannot order).
}

func TestBoundUsesTheCallerDeadlineOrTheRepositoryTimeout(t *testing.T) {
	r := testRepo(nil, 50*time.Millisecond)
	ctx, cancel := r.bound(context.Background())
	deadline, ok := ctx.Deadline()
	cancel()
	if !ok || time.Until(deadline) > 50*time.Millisecond {
		t.Fatal("no deadline for a caller without one")
	}
	parent, stop := context.WithTimeout(context.Background(), time.Hour)
	defer stop()
	ctx, cancel = r.bound(parent)
	defer cancel()
	if d, _ := ctx.Deadline(); d.Before(time.Now().Add(59 * time.Minute)) {
		t.Fatal("caller deadline shortened")
	}
}

const finishOperationSQL = "UPDATE image_data_operations SET phase='committed', size=?, virtual_size=?, checksum=?, os_hash_value=?, updated_at=? WHERE image_id=? AND attempt_id=? AND phase='reserved'"

// FinishData: one transaction from reserved to committed, as the original's
// image_repo.save after glance_store.add.
func TestFinishDataIsOneTransactionFromReserved(t *testing.T) {
	ok := append(transferSteps("saving", false, dataFixture(model.DataReserved)),
		execute(commitImageSQL), execute(finishLocationSQL), execute(finishOperationSQL), execute("COMMIT"))
	if err := testRepo(scriptDB(t, ok), time.Second).FinishData(context.Background(), catalogID, attemptA, receipt(), dataNow); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []model.DataPhase{model.DataStaged, model.DataCommitted, model.DataCleaned} {
		steps := append(transferSteps("saving", false, dataFixture(phase)), execute("ROLLBACK"))
		if err := testRepo(scriptDB(t, steps), time.Second).FinishData(context.Background(), catalogID, attemptA, receipt(), dataNow); !errors.Is(err, repository.ErrConflict) {
			t.Fatal(phase, err)
		}
	}
	queued := append(transferSteps("queued", false, dataFixture(model.DataReserved)), execute("ROLLBACK"))
	if err := testRepo(scriptDB(t, queued), time.Second).FinishData(context.Background(), catalogID, attemptA, receipt(), dataNow); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("finished an image that is not saving", err)
	}
}
