package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

func deletionReadSteps(status string, o *model.DataOperation) []sqlStep {
	steps := append([]sqlStep{execute("BEGIN")}, imageReadSteps(true)...)
	row := steps[1].rows[0]
	row[3] = status
	if status == "active" && o != nil {
		row[11], row[12], row[13], row[14], row[15] = ptrValue(o.Size), ptrValue(o.VirtualSize), ptrValue(o.Checksum), "sha512", ptrValue(o.SHA512)
	}
	op := sqlStep{query: lockDataOperation}
	if o != nil {
		op.rows = [][]driver.Value{operationRow(o)}
	}
	return append(steps, op)
}
func deletionSQLSteps(status string, o *model.DataOperation) []sqlStep {
	steps := append(deletionReadSteps(status, o), execute(tombstoneDataSQL))
	if o != nil {
		steps = append(steps, execute(tombstoneOperationSQL))
	}
	return append(append(steps, destroySteps()...), execute("COMMIT"))
}

// destroySteps: the children the original's image_destroy soft-deletes.
func destroySteps() []sqlStep {
	return []sqlStep{
		query("SELECT id FROM image_locations WHERE image_id=?", int64(7)),
		execute("UPDATE image_locations SET deleted=1, deleted_at=?, updated_at=?, status='deleted' WHERE id=? AND deleted=0"),
		{query: "SELECT id FROM image_members WHERE image_id=? AND deleted=0"}, // no members
		execute("UPDATE image_properties SET deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND deleted=0"),
		execute("UPDATE image_tags SET deleted=1, deleted_at=?, updated_at=? WHERE image_id=? AND deleted=0"),
	}
}
func allowDeletion(model.Image) error { return nil }

func TestTombstoneDataAtomicTransitionsAndRetainedReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		phase        model.DataPhase
		target       string
	}{
		{"queued-empty", "queued", "", ""}, {"queued-cleaned", "queued", model.DataCleaned, "queued"},
		{"saving-reserved", "saving", model.DataReserved, ""}, {"saving-staged", "saving", model.DataStaged, ""}, {"saving-published", "saving", model.DataPublished, ""},
		{"saving-cleanup", "saving", model.DataCleanup, "queued"}, {"terminal-pending", "saving", model.DataCleanup, "killed"},
		{"active", "active", model.DataCommitted, ""}, {"killed-empty", "killed", "", ""}, {"killed-cleaned", "killed", model.DataCleaned, "killed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *model.DataOperation
			var attempt *string
			if tc.phase != "" {
				o = dataFixture(tc.phase)
				attempt = str(o.AttemptID)
				if tc.target != "" {
					o.CleanupTarget = str(tc.target)
				}
			}
			steps := deletionSQLSteps(tc.status, o)
			steps[3].checkArgs = func(args []driver.NamedValue) {
				if len(args) != 4 || args[2].Value != catalogID || args[3].Value != tc.status {
					t.Error("unfenced tombstone", args)
				}
				for _, a := range args[:2] {
					at, ok := a.Value.(time.Time)
					if !ok || at.Location() != time.UTC || !at.After(time.Unix(100, 0)) {
						t.Error("invalid UTC time", a)
					}
				}
			}
			if o != nil {
				steps[4].checkArgs = func(args []driver.NamedValue) {
					if len(args) != 4 || args[1].Value != catalogID || args[2].Value != attemptA || args[3].Value != string(tc.phase) {
						t.Error("unfenced operation", args)
					}
				}
			}
			called := false
			s, err := testRepo(scriptDB(t, steps), time.Second).TombstoneData(context.Background(), catalogID, attempt, dataNow, func(i model.Image) error {
				called = true
				if i.Owner != "p" || i.Properties["x"] != "original" || len(i.Tags) != 1 {
					t.Error("policy did not get locked catalog")
				}
				return nil
			})
			if err != nil || !called || !s.Deleted || s.Status != "deleted" {
				t.Fatal(s, err)
			}
			if o == nil {
				if s.Operation != nil {
					t.Fatal("invented operation")
				}
			} else if s.Operation.AttemptID != attemptA || s.Operation.Phase != model.DataCleanup || !isTarget(s.Operation, "deleted") || !equalPointer(s.Operation.Checksum, o.Checksum) || !equalPointer(s.Operation.Size, o.Size) {
				t.Fatal("lost receipts", s)
			}
			if strings.Contains(tombstoneDataSQL, "name=") || strings.Contains(tombstoneDataSQL, "size=") || strings.Contains(tombstoneOperationSQL, "attempt_id=? SET") {
				t.Fatal("deletion overwrites data")
			}
		})
	}
}

func TestTombstoneDataRejectsFencesPolicyAndContradictions(t *testing.T) {
	for _, tc := range []string{"protected", "policy", "missing", "wrong-attempt", "no-expected", "expected-absent", "queued-live", "active-no-operation", "active-published", "active-receipt", "saving-no-operation", "saving-committed", "killed-pending", "unknown-state"} {
		t.Run(tc, func(t *testing.T) {
			o := dataFixture(model.DataCommitted)
			status := "active"
			attempt := str(attemptA)
			switch tc {
			case "wrong-attempt":
				attempt = str(attemptB)
			case "no-expected":
				attempt = nil
			case "expected-absent":
				o = nil
				status = "queued"
			case "queued-live":
				status = "queued"
				o = dataFixture(model.DataReserved)
			case "active-no-operation":
				o = nil
			case "active-published":
				o = dataFixture(model.DataPublished)
			case "saving-no-operation":
				o = nil
				status = "saving"
				attempt = nil
			case "saving-committed":
				status = "saving"
			case "killed-pending":
				status = "killed"
				o = dataFixture(model.DataCleanup)
				o.CleanupTarget = str("killed")
			case "unknown-state":
				status = "deactivated"
			}
			steps := deletionReadSteps(status, o)
			if tc == "protected" || tc == "policy" {
				steps = steps[:2]
				if tc == "protected" {
					steps[1].rows[0][9] = true
				}
			}
			if tc == "missing" {
				steps = steps[:2]
				steps[1].rows = nil
			}
			if tc == "active-receipt" {
				steps[1].rows[0][13] = "wrong"
			}
			steps = append(steps, execute("ROLLBACK"))
			_, err := testRepo(scriptDB(t, steps), time.Second).TombstoneData(context.Background(), catalogID, attempt, dataNow, func(model.Image) error {
				if tc == "policy" {
					return model.Fail(model.Forbidden, "denied")
				}
				return nil
			})
			if err == nil {
				t.Fatal("unsafe deletion accepted")
			}
		})
	}
}

func TestTombstoneDataFailuresRollbackOrReportUncertainty(t *testing.T) {
	commit := len(deletionSQLSteps("active", dataFixture(model.DataCommitted))) - 1
	for at := 0; at <= commit; at++ {
		t.Run(string(rune('a'+at)), func(t *testing.T) {
			steps := deletionSQLSteps("active", dataFixture(model.DataCommitted))
			steps[at].err = errors.New("SQL disconnected")
			steps = steps[:at+1]
			if at != 0 && at != commit {
				steps = append(steps, execute("ROLLBACK"))
			}
			out, err := testRepo(scriptDB(t, steps), time.Second).TombstoneData(context.Background(), catalogID, str(attemptA), dataNow, allowDeletion)
			if err == nil {
				t.Fatal("failure reported success")
			}
			if at == commit && (!errors.Is(err, repository.ErrCommitUncertain) || !out.Deleted || out.Operation == nil || !isTarget(out.Operation, "deleted")) {
				t.Fatal("commit outcome was hidden", out, err)
			}
		})
	}
	for _, at := range []int{3, 4} {
		t.Run("unacknowledged-"+string(rune('a'+at)), func(t *testing.T) {
			steps := deletionSQLSteps("active", dataFixture(model.DataCommitted))
			steps[at].affected = 0
			steps = append(steps[:at+1], execute("ROLLBACK"))
			_, err := testRepo(scriptDB(t, steps), time.Second).TombstoneData(context.Background(), catalogID, str(attemptA), dataNow, allowDeletion)
			if !errors.Is(err, repository.ErrConflict) {
				t.Fatal(err)
			}
		})
	}
}

func TestTombstoneDataInvalidInputDoesNotOpenSQL(t *testing.T) {
	r := &Repository{}
	for _, tc := range []struct {
		id      string
		attempt *string
		check   func(model.Image) error
	}{{"bad", nil, allowDeletion}, {catalogID, str("bad"), allowDeletion}, {catalogID, nil, nil}} {
		if _, err := r.TombstoneData(context.Background(), tc.id, tc.attempt, dataNow, tc.check); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
