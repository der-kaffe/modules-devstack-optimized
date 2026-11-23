package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// Extends the independent SQL state double; it performs no store I/O. The SQL
// driver tests separately check real repository statement ordering and rollback.
type deletionSQLDouble struct{ *uploadSQLDouble }

func (d deletionSQLDouble) TombstoneData(ctx context.Context, id string, attempt *string, at time.Time, check func(model.Image) error) (out model.DataState, err error) {
	err = d.run(ctx, "Tombstone", func() error {
		if id != d.image.ID || d.image.Status == "deleted" {
			return repository.ErrNotFound
		}
		if err := check(d.image); err != nil {
			return err
		}
		if d.image.Protected {
			return model.Fail(model.Forbidden, "protected")
		}
		if attempt == nil && d.op != nil || attempt != nil && (d.op == nil || d.op.AttemptID != *attempt) {
			return repository.ErrConflict
		}
		d.image.Status = "deleted"
		if d.op != nil {
			d.op = cloneUploadOp(d.op)
			d.op.Phase = model.DataCleanup
			d.op.CleanupTarget = uploadPtr("deleted")
			d.op.LastErrorCode = uploadPtr("deleted")
			d.op.UpdatedAt = at
		}
		out = d.snapshot()
		return nil
	})
	return
}

// BeginDataCleanup adds the tombstone target, as the MySQL repository: any
// unfinished phase of a deleted image may move to cleanup(deleted).
func (d deletionSQLDouble) BeginDataCleanup(ctx context.Context, id, attempt, target string, code *string, at time.Time) error {
	if target != "deleted" {
		return d.uploadSQLDouble.BeginDataCleanup(ctx, id, attempt, target, code, at)
	}
	return d.run(ctx, "BeginCleanup", func() error {
		if d.op == nil || id != d.image.ID || d.op.AttemptID != attempt || d.op.Phase == model.DataCleaned || d.image.Status != "deleted" {
			return repository.ErrConflict
		}
		d.op.Phase, d.op.CleanupTarget, d.op.LastErrorCode, d.op.UpdatedAt = model.DataCleanup, uploadPtr("deleted"), code, at
		return nil
	})
}

// ForgetPurgedData: the image row is gone (purged by the original).
func (d deletionSQLDouble) ForgetPurgedData(ctx context.Context, id, attempt string) error {
	return d.run(ctx, "ForgetPurged", func() error {
		if d.op == nil || id != d.op.ImageID || d.op.AttemptID != attempt || d.image.ID == id {
			return repository.ErrConflict
		}
		d.op = nil
		return nil
	})
}

func (d deletionSQLDouble) CompleteDataCleanup(ctx context.Context, id, attempt string, at time.Time) error {
	return d.run(ctx, "CompleteCleanup", func() error {
		if d.op == nil || id != d.image.ID || d.op.AttemptID != attempt || d.op.Phase != model.DataCleanup || d.op.CleanupTarget == nil {
			return repository.ErrConflict
		}
		target := *d.op.CleanupTarget
		if target == "deleted" {
			if d.image.Status != "deleted" {
				return repository.ErrConflict
			}
		} else {
			if d.image.Status != "saving" || target != "queued" && target != "killed" {
				return repository.ErrConflict
			}
			d.image.Status = target
			d.image.Size = nil
			d.image.VirtualSize = nil
			d.image.Checksum = nil
			d.image.HashAlgorithm = nil
			d.image.HashValue = nil
		}
		d.op.Phase = model.DataCleaned
		d.op.UpdatedAt = at
		return nil
	})
}

func deletionFixture(t *testing.T) (*UploadCoordinator, *uploadSQLDouble, *uploadStoreDouble, *uploadEvents) {
	u, d, fs, log := uploadFixture(t)
	u.transfers = deletionSQLDouble{d}
	return u, d, fs, log
}
func restartDeletion(t *testing.T, u *UploadCoordinator, d *uploadSQLDouble, fs *uploadStoreDouble) *UploadCoordinator {
	t.Helper()
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := newUploadCoordinator(uploadCatalogDouble{sql: d}, deletionSQLDouble{d}, fs, u.limits)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func seedDeleted(d *uploadSQLDouble, phase model.DataPhase) {
	if phase != model.DataCommitted {
		d.image.Size = nil
		d.image.VirtualSize = nil
		d.image.Checksum = nil
		d.image.HashAlgorithm = nil
		d.image.HashValue = nil
	}
	uploadSeed(d, phase)
	d.image.Status = "deleted"
	d.op.Phase = model.DataCleanup
	d.op.CleanupTarget = uploadPtr("deleted")
	d.op.LastErrorCode = uploadPtr("deleted")
}

func TestDeleteImageDurableOrderAndPreservedMetadata(t *testing.T) {
	for _, phase := range []model.DataPhase{"", model.DataReserved, model.DataStaged, model.DataPublished, model.DataCommitted, model.DataCleanup, model.DataCleaned} {
		t.Run(string(phase), func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			if phase != "" {
				uploadSeed(d, phase)
				fs.part = true
				fs.ready = true
				fs.blob = append([]byte{}, uploadPayload...)
			}
			fs.before = func(name string, key store.Key) {
				if name == "Delete" && key != (store.Key{ImageID: uploadImageID, AttemptID: oldUploadAttempt}) {
					t.Error("wrong storage key", key)
				}
			}
			d.image.Tags = []string{"keep"}
			d.image.Properties = map[string]string{"description": "keep"}
			if err := u.DeleteImage(context.Background(), identity("member"), uploadImageID); err != nil {
				t.Fatal(err)
			}
			// After an acknowledged tombstone: the resolution read, one read before
			// the unlink and the conditional cleanup transition. Nothing is read
			// back after the unlink or the cleanup commit.
			want := []string{"catalog.Get", "sql.State", "sql.Tombstone", "sql.State"}
			if phase != "" {
				want = append(want, "sql.State", "fs.Delete", "sql.CompleteCleanup")
			}
			if !reflect.DeepEqual(log.snapshot(), want) {
				t.Fatal(log.snapshot())
			}
			if d.image.Status != "deleted" || len(d.image.Tags) != 1 || d.image.Properties["description"] != "keep" {
				t.Fatal("lost tombstone metadata")
			}
			if phase == "" {
				if d.op != nil {
					t.Fatal("invented attempt")
				}
			} else if d.op.Phase != model.DataCleaned || *d.op.CleanupTarget != "deleted" || fs.blob != nil || fs.part || fs.ready {
				t.Fatal("incomplete cleanup")
			}
			if phase == model.DataCommitted && (d.image.Size == nil || *d.image.Size != int64(len(uploadPayload))) {
				t.Fatal("tombstone receipts lost")
			}
			if err := u.DeleteImage(context.Background(), identity("member"), uploadImageID); err == nil {
				t.Fatal("repeated delete succeeded")
			}
		})
	}
}

func TestDeleteImageAuthorizationBeforeAnyMutation(t *testing.T) {
	for _, tc := range []string{"reader", "private", "shared", "public", "community", "protected-owner", "protected-admin", "missing", "expired", "bad-id", "not-ready", "admin", "cancelled"} {
		t.Run(tc, func(t *testing.T) {
			u, d, _, log := deletionFixture(t)
			who, id, ctx := identity("member"), uploadImageID, context.Background()
			success := tc == "admin"
			switch tc {
			case "reader":
				who = identity("reader")
			case "private", "shared", "public", "community":
				who.ProjectID = "other"
				d.image.Visibility = tc
			case "protected-owner":
				d.image.Protected = true
			case "protected-admin":
				d.image.Protected = true
				who = identity("admin")
			case "missing":
				u.images = uploadCatalogDouble{sql: d, err: repository.ErrNotFound}
			case "expired":
				who.ExpiresAt = who.ExpiresAt.Add(-2 * time.Hour)
			case "bad-id":
				id = "bad"
			case "not-ready":
				u.ready = false
			case "admin":
				who = identity("admin")
				who.ProjectID = "other"
				d.image.Visibility = "private"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := u.DeleteImage(ctx, who, id)
			if (err == nil) != success {
				t.Fatal(err)
			}
			if !success {
				for _, call := range log.snapshot() {
					if call == "sql.Tombstone" || strings.HasPrefix(call, "fs.") {
						t.Fatal("denied request wrote", log.snapshot())
					}
				}
			}
		})
	}
}

func TestDeleteImagePolicyRecheckedUnderLock(t *testing.T) {
	u, d, _, log := deletionFixture(t)
	d.before = func(name string) {
		if name == "Tombstone" {
			d.mu.Lock()
			d.image.Protected = true
			d.mu.Unlock()
		}
	}
	err := u.DeleteImage(context.Background(), identity("admin"), uploadImageID)
	var domain *model.Error
	if !errors.As(err, &domain) || domain.Kind != model.Forbidden || d.image.Status != "queued" {
		t.Fatal(err)
	}
	for _, call := range log.snapshot() {
		if call == "fs.Delete" {
			t.Fatal("protected bytes removed")
		}
	}
}

func TestDeleteImageUncertainCommitsAndFailures(t *testing.T) {
	for _, tc := range []string{"tombstone-unapplied", "tombstone-applied", "tombstone-sql", "read-before-tombstone", "read-resolution", "read-before-unlink", "read-after-unlink", "read-final", "delete-failed", "unlink-sync-failed", "complete-unapplied", "complete-applied", "complete-sql", "old-attempt", "replacement-before-tombstone"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			uploadSeed(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			success := false
			switch tc {
			case "tombstone-unapplied":
				d.faults["Tombstone"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "tombstone-applied":
				d.faults["Tombstone"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				success = true
			case "tombstone-sql":
				d.faults["Tombstone"] = uploadSQLFault{err: errors.New("private SQL password")}
			case "read-before-tombstone":
				d.failReadAt[1] = true
			case "replacement-before-tombstone":
				d.before = func(name string) {
					if name == "Tombstone" {
						d.mu.Lock()
						d.op.AttemptID = "00000000-0000-4000-8000-000000000099"
						d.mu.Unlock()
					}
				}
			case "read-resolution":
				d.failReadAt[2] = true
			case "read-before-unlink":
				d.failReadAt[3] = true
			// The reads after the unlink and after the cleanup commit exist
			// only for an uncertain tombstone.
			case "read-after-unlink":
				d.faults["Tombstone"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				d.failReadAt[4] = true
			case "read-final":
				d.faults["Tombstone"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				d.failReadAt[5] = true
			case "delete-failed":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("private filesystem")}
			case "unlink-sync-failed":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("private fsync"), applied: true}
			case "complete-unapplied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "complete-applied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				success = true
			case "complete-sql":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: errors.New("private SQL")}
			case "old-attempt":
				d.after = func(name string) {
					if name == "Tombstone" {
						d.mu.Lock()
						d.op.AttemptID = "00000000-0000-4000-8000-000000000099"
						d.mu.Unlock()
					}
				}
			}
			err := u.DeleteImage(context.Background(), identity("member"), uploadImageID)
			if (err == nil) != success || err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal(err, log.snapshot())
			}
			if tc == "tombstone-unapplied" || tc == "tombstone-sql" || tc == "read-before-tombstone" || tc == "read-resolution" || tc == "read-before-unlink" || tc == "old-attempt" || tc == "replacement-before-tombstone" {
				for _, call := range log.snapshot() {
					if call == "fs.Delete" {
						t.Fatal("unlink before confirmation", log.snapshot())
					}
				}
			}
			if tc == "delete-failed" || tc == "unlink-sync-failed" {
				if d.op.Phase != model.DataCleanup || d.image.Status != "deleted" {
					t.Fatal("lost cleanup")
				}
				for _, call := range log.snapshot() {
					if call == "sql.CompleteCleanup" {
						t.Fatal("fsync failure reported complete")
					}
				}
			}
		})
	}
}

func TestDeleteRejectsLiveWriterAtEveryCriticalBoundary(t *testing.T) {
	for _, boundary := range []string{"Stage", "Publish", "Commit"} {
		t.Run(boundary, func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			block := func() { once.Do(func() { close(entered); <-release }) }
			if boundary == "Commit" {
				d.before = func(name string) {
					if name == boundary {
						block()
					}
				}
			} else {
				fs.before = func(name string, _ store.Key) {
					if name == boundary {
						block()
					}
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("writer did not enter boundary")
			}
			err := u.DeleteImage(context.Background(), identity("member"), uploadImageID)
			if !errors.Is(err, ErrUploadBusy) {
				t.Error("writer not excluded", err)
			}
			for _, call := range log.snapshot() {
				if call == "sql.Tombstone" || call == "fs.Delete" {
					t.Error("DELETE touched running writer", log.snapshot())
				}
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("writer leaked")
			}
			if err := u.DeleteImage(context.Background(), identity("member"), uploadImageID); err != nil {
				t.Fatal(err)
			}
			if err := d.CommitData(context.Background(), uploadImageID, fs.lastKey.AttemptID, time.Now()); !errors.Is(err, repository.ErrConflict) {
				t.Fatal("late finalizer activated tombstone", err)
			}
			if d.image.Status != "deleted" || fs.blob != nil {
				t.Fatal("tombstone resurrected")
			}
		})
	}
}

func TestConcurrentDeleteExcludesNewWriterAndOwnershipRelease(t *testing.T) {
	u, d, fs, log := deletionFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d.before = func(name string) {
		if name == "Tombstone" {
			once.Do(func() { close(entered); <-release })
		}
	}
	done := make(chan error, 1)
	go func() { done <- u.DeleteImage(context.Background(), identity("member"), uploadImageID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delete did not claim")
	}
	if err := u.DeleteImage(context.Background(), identity("member"), uploadImageID); !errors.Is(err, ErrUploadBusy) {
		t.Error(err)
	}
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil); !errors.Is(err, ErrUploadBusy) {
		t.Error("writer admitted during deletion", err)
	}
	if err := u.Close(); !errors.Is(err, ErrUploadBusy) {
		t.Error("ownership released during DELETE", err)
	}
	if _, err := u.RecoverUploads(context.Background()); !errors.Is(err, ErrUploadNotReady) {
		t.Error("recovery entered live delete", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("delete leaked")
	}
	if fs.blob != nil || d.image.Status != "deleted" {
		t.Fatal(log.snapshot())
	}
}

func TestDeletePreservesAlreadyOpenDownload(t *testing.T) {
	u, d, fs, _ := deletionFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	fd, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.DeleteImage(context.Background(), identity("member"), uploadImageID); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(fd.Body)
	if err != nil || !bytes.Equal(got, uploadPayload) {
		t.Fatal("open fd damaged", err)
	}
	_ = fd.Body.Close()
	if _, err = u.OpenDownload(context.Background(), identity("reader"), uploadImageID); err == nil {
		t.Fatal("new download read deleted image")
	}
}

func TestDeletionRecoveryRepeatedRestartsAndTerminalCleanup(t *testing.T) {
	for _, tc := range []string{"reserved-deleted", "staged-deleted", "published-deleted", "committed-deleted", "missing", "corrupt", "unlink-sync-failed", "complete-unapplied", "complete-applied", "terminal-killed"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			seedDeleted(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			switch tc {
			case "reserved-deleted":
				seedDeleted(d, model.DataReserved)
				fs.part = true
			case "staged-deleted":
				seedDeleted(d, model.DataStaged)
				fs.ready = true
			case "published-deleted":
				seedDeleted(d, model.DataPublished)
			case "missing":
				fs.blob = nil
			case "corrupt":
				fs.blob = []byte("corrupt bytes")
			case "unlink-sync-failed":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("sync"), applied: true}
			case "complete-unapplied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "complete-applied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
			case "terminal-killed":
				seedDeleted(d, model.DataCleanup)
				d.image.Status = "saving"
				d.op.CleanupTarget = uploadPtr("killed")
			}
			u = restartDeletion(t, u, d, fs)
			log.reset()
			report, err := u.RecoverUploads(context.Background())
			if tc == "unlink-sync-failed" || tc == "complete-unapplied" {
				if err == nil || u.ready || d.op.Phase != model.DataCleanup {
					t.Fatal("uncertain cleanup enabled traffic", err)
				}
				delete(fs.faults, "Delete")
				delete(d.faults, "CompleteCleanup")
				log.reset()
				report, err = u.RecoverUploads(context.Background())
			}
			if err != nil || report.Cleaned != 1 || d.op.Phase != model.DataCleaned || fs.blob != nil {
				t.Fatal(report, err, log.snapshot())
			}
			status := "deleted"
			if tc == "terminal-killed" {
				status = "killed"
			}
			if d.image.Status != status {
				t.Fatal("wrong cleanup destination", d.image.Status)
			}
			u = restartDeletion(t, u, d, fs)
			log.reset()
			if report, err = u.RecoverUploads(context.Background()); err != nil || report.Kept != 1 {
				t.Fatal("non-idempotent recovery", report, err)
			}
			for _, call := range log.snapshot() {
				if call == "fs.Delete" || call == "sql.Tombstone" || call == "sql.CompleteCleanup" {
					t.Fatal("cleaned state mutated again", log.snapshot())
				}
			}
		})
	}
}

func TestDeletionRecoveryFailsClosedOnOwnershipOrSQLContradictions(t *testing.T) {
	for _, tc := range []string{"old-inventory", "active-cleanup", "sql-unavailable", "state-after-unlink", "receipt-mismatch", "active-with-cleanup-target", "reserved-with-cleanup-target"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			seedDeleted(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			switch tc {
			case "old-inventory":
				d.inventory = []model.DataOperation{*d.op}
				d.op.AttemptID = "00000000-0000-4000-8000-000000000099"
			case "active-cleanup":
				d.image.Status = "active"
			case "sql-unavailable":
				d.failReadAt[1] = true
			case "receipt-mismatch":
				d.image.Checksum = uploadPtr(strings.Repeat("c", 32))
			case "active-with-cleanup-target":
				uploadSeed(d, model.DataCommitted)
				d.op.CleanupTarget = uploadPtr("queued")
			case "reserved-with-cleanup-target":
				seedDeleted(d, model.DataReserved)
				d.image.Status = "saving"
				d.op.Phase = model.DataReserved
				d.op.CleanupTarget = uploadPtr("queued")
			case "state-after-unlink":
				d.failReadAt[3] = true
			}
			u = restartDeletion(t, u, d, fs)
			log.reset()
			if _, err := u.RecoverUploads(context.Background()); err == nil || u.ready {
				t.Fatal("unsafe recovery admitted traffic")
			}
			for _, call := range log.snapshot() {
				if call == "fs.Delete" && tc != "state-after-unlink" {
					t.Fatal("unsafe unlink", log.snapshot())
				}
				if call == "sql.CompleteCleanup" {
					t.Fatal("unsafe finalization")
				}
			}
		})
	}
}

func TestDeleteCancellationAfterTombstoneAndEmptyUncertainCommit(t *testing.T) {
	for _, tc := range []string{"cancel-after-tombstone", "empty-applied", "empty-unapplied", "unsupported-backend", "catalog-failure"} {
		t.Run(tc, func(t *testing.T) {
			u, d, _, log := deletionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc == "cancel-after-tombstone" {
				uploadSeed(d, model.DataCommitted)
				d.after = func(name string) {
					if name == "Tombstone" {
						cancel()
					}
				}
			}
			if strings.HasPrefix(tc, "empty-") {
				d.faults["Tombstone"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: tc == "empty-applied"}
			}
			if tc == "unsupported-backend" {
				u.transfers = d
			}
			if tc == "catalog-failure" {
				u.images = uploadCatalogDouble{sql: d, err: errors.New("private SQL")}
			}
			err := u.DeleteImage(ctx, identity("member"), uploadImageID)
			wantSuccess := tc == "cancel-after-tombstone" || tc == "empty-applied"
			if (err == nil) != wantSuccess || err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal(err, log.snapshot())
			}
			if tc != "cancel-after-tombstone" {
				for _, call := range log.snapshot() {
					if call == "fs.Delete" {
						t.Fatal("empty or unsupported deletion touched files")
					}
				}
			}
		})
	}
}

// The original shares the catalog: it may delete an image Glance Go uploaded
// (removing the file itself) or purge its row later. Recovery finishes that
// bookkeeping instead of refusing to start.
func TestRecoveryReleasesImagesTheOriginalDeletedOrPurged(t *testing.T) {
	for _, tc := range []string{"deleted-committed", "deleted-published", "deleted-during-cleanup", "purged"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := deletionFixture(t)
			switch tc {
			case "deleted-committed":
				uploadSeed(d, model.DataCommitted)
				d.image.Status = "deleted"
			case "deleted-published":
				uploadSeed(d, model.DataPublished)
				d.image.Status = "deleted"
				fs.blob = append([]byte{}, uploadPayload...) // left behind
			case "deleted-during-cleanup":
				uploadSeed(d, model.DataCleanup)
				d.image.Status = "deleted"
			case "purged":
				uploadSeed(d, model.DataCleaned)
				d.inventory = []model.DataOperation{*d.op}
				d.image.ID = "00000000-0000-4000-8000-0000000000ff" // the row is gone
			}
			u = restartDeletion(t, u, d, fs)
			log.reset()
			report, err := u.RecoverUploads(context.Background())
			if err != nil || report.Cleaned != 1 || !u.ready {
				t.Fatal(report, err, log.snapshot())
			}
			if fs.blob != nil {
				t.Fatal("file left behind")
			}
			if tc == "purged" {
				if d.op != nil {
					t.Fatal("orphaned bookkeeping kept")
				}
				return
			}
			if d.op.Phase != model.DataCleaned || *d.op.CleanupTarget != "deleted" {
				t.Fatal("bookkeeping not released", d.op)
			}
			// A second start keeps the released state.
			u = restartDeletion(t, u, d, fs)
			if report, err = u.RecoverUploads(context.Background()); err != nil || report.Kept != 1 {
				t.Fatal(report, err)
			}
		})
	}
}
