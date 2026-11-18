package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

func uploadRestart(t *testing.T, u *UploadCoordinator, d *uploadSQLDouble, fs *uploadStoreDouble) *UploadCoordinator {
	t.Helper()
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := newUploadCoordinator(uploadCatalogDouble{sql: d}, d, fs, u.limits)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestRecoveryCleansInterruptedUploadsWithoutPromotion(t *testing.T) {
	for _, phase := range []model.DataPhase{model.DataReserved, model.DataStaged, model.DataPublished, model.DataCleanup} {
		t.Run(string(phase), func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, phase)
			fs.part = true
			fs.ready = true
			fs.staged = append([]byte{}, uploadPayload...)
			if phase == model.DataPublished || phase == model.DataCleanup {
				fs.blob = append([]byte{}, uploadPayload...)
			}
			u = uploadRestart(t, u, d, fs)
			log.reset()
			report, err := u.RecoverUploads(context.Background())
			if err != nil || report.Cleaned != 1 || report.Examined != 1 || d.image.Status != "queued" || d.op.Phase != model.DataCleaned || fs.part || fs.ready || fs.blob != nil {
				t.Fatal(report, err, log.snapshot())
			}
			for _, c := range log.snapshot() {
				if c == "sql.Commit" || c == "fs.Publish" || c == "fs.Stage" {
					t.Fatal("recovery promoted incomplete upload")
				}
			}
			_, err = u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			if err != nil || d.op.AttemptID == oldUploadAttempt {
				t.Fatal("fresh attempt not allowed after confirmed cleanup", err)
			}
		})
	}
}

func TestRecoveryKeepsConfirmedActiveAndCleaned(t *testing.T) {
	for _, phase := range []model.DataPhase{model.DataCommitted, model.DataCleaned} {
		t.Run(string(phase), func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, phase)
			if phase == model.DataCommitted {
				fs.blob = append([]byte{}, uploadPayload...)
			}
			u = uploadRestart(t, u, d, fs)
			log.reset()
			report, err := u.RecoverUploads(context.Background())
			if err != nil || report.Kept != 1 || report.Cleaned != 0 {
				t.Fatal(report, err)
			}
			for _, c := range log.snapshot() {
				if c == "fs.Delete" || c == "sql.BeginCleanup" || c == "sql.CompleteCleanup" {
					t.Fatal("cleaned confirmed state", log.snapshot())
				}
			}
			if phase == model.DataCommitted && !bytes.Equal(fs.blob, uploadPayload) {
				t.Fatal("active bytes changed")
			}
		})
	}
}

func TestRecoveryConservesUnresolvedAndTombstoneEvidence(t *testing.T) {
	for _, tc := range []string{"active-missing", "active-corrupt", "active-published", "tombstone", "old-inventory", "read-unavailable", "list-unavailable", "delete-unavailable", "terminal-cleanup", "malformed-inventory"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, model.DataPublished)
			fs.blob = append([]byte{}, uploadPayload...)
			switch tc {
			case "active-missing":
				uploadSeed(d, model.DataCommitted)
				fs.blob = nil
			case "active-corrupt":
				uploadSeed(d, model.DataCommitted)
				fs.blob = []byte("not original")
			case "active-published":
				d.image.Status = "active"
			case "tombstone":
				d.image.Status = "deleted"
			case "old-inventory":
				d.inventory = []model.DataOperation{*d.op}
				d.op.AttemptID = "00000000-0000-4000-8000-000000000003"
			case "read-unavailable":
				d.failReadAt[1] = true
			case "list-unavailable":
				d.faults["List"] = uploadSQLFault{err: errors.New("unavailable")}
			case "delete-unavailable":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("unavailable")}
			case "terminal-cleanup":
				d.op.Phase = model.DataCleanup
				d.op.CleanupTarget = uploadPtr("killed")
				// A valid pending terminal cleanup is saving, not already killed.
				// Action 7 covers the supported saving case in positive regressions.
				d.image.Status = "killed"
			case "malformed-inventory":
				d.op.ImageID = "bad"
			}
			u = uploadRestart(t, u, d, fs)
			log.reset()
			_, err := u.RecoverUploads(context.Background())
			if err == nil || u.ready {
				t.Fatal("unsafe startup accepted", log.snapshot())
			}
			if tc != "delete-unavailable" {
				for _, c := range log.snapshot() {
					if c == "fs.Delete" || c == "sql.CompleteCleanup" {
						t.Fatal("unsafe recovery compensation", log.snapshot())
					}
				}
			}
			if tc == "delete-unavailable" && (d.op.Phase != model.DataCleanup || d.image.Status != "saving" || fs.blob == nil) {
				t.Fatal("lost pending cleanup")
			}
		})
	}
}

func TestRecoveryRetriesPendingCleanupAfterSyncFailure(t *testing.T) {
	u, d, fs, log := uploadFixture(t)
	uploadSeed(d, model.DataCleanup)
	fs.blob = append([]byte{}, uploadPayload...)
	fs.faults["Delete"] = uploadStoreFault{err: errors.New("sync failed after unlink"), applied: true}
	u = uploadRestart(t, u, d, fs)
	if _, err := u.RecoverUploads(context.Background()); err == nil || d.op.Phase != model.DataCleanup || d.image.Status != "saving" {
		t.Fatal(err)
	}
	delete(fs.faults, "Delete")
	log.reset()
	if report, err := u.RecoverUploads(context.Background()); err != nil || report.Cleaned != 1 || d.image.Status != "queued" {
		t.Fatal(report, err)
	}
	calls := strings.Join(log.snapshot(), ",")
	if !strings.Contains(calls, "fs.Delete,sql.State,sql.CompleteCleanup,sql.State") {
		t.Fatal("did not re-confirm fs sync", calls)
	}
}

func TestRecoveryReReadsInventoryAndPaginates(t *testing.T) {
	u, d, fs, log := uploadFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	old := *d.op
	old.Phase = model.DataReserved
	old.Size = nil
	old.VirtualSize = nil
	old.Checksum = nil
	old.SHA512 = nil
	d.inventory = []model.DataOperation{old}
	u = uploadRestart(t, u, d, fs)
	if report, err := u.RecoverUploads(context.Background()); err != nil || report.Kept != 1 {
		t.Fatal("trusted stale inventory phase", report, err)
	}
	// 101 clean candidates require two pages; each gets its own authoritative read.
	u, d, fs, log = uploadFixture(t)
	d.inventory = []model.DataOperation{}
	for n := 100; n < 201; n++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
		o := model.DataOperation{ImageID: id, AttemptID: oldUploadAttempt, Phase: model.DataCleaned, CleanupTarget: uploadPtr("queued")}
		d.inventory = append(d.inventory, o)
		d.otherStates[id] = model.DataState{ImageID: id, Status: "queued", Operation: &o}
	}
	u = uploadRestart(t, u, d, fs)
	log.reset()
	report, err := u.RecoverUploads(context.Background())
	if err != nil || report.Examined != 101 || report.Kept != 101 {
		t.Fatal(report, err)
	}
	lists := 0
	for _, c := range log.snapshot() {
		if c == "sql.List" {
			lists++
		}
	}
	if lists != 2 {
		t.Fatal("keyset paging failed", lists)
	}
}

func TestRecoveryHonorsCancellationBeforeAdmission(t *testing.T) {
	u, d, fs, _ := uploadFixture(t)
	u = uploadRestart(t, u, d, fs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.RecoverUploads(ctx); !errors.Is(err, context.Canceled) || u.ready {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	d.after = func(name string) {
		if name == "List" {
			cancel()
		}
	}
	if _, err := u.RecoverUploads(ctx); !errors.Is(err, context.Canceled) || u.ready {
		t.Fatal("cancelled empty startup admitted writers", err)
	}
}

type uploadCooperativeReader struct {
	ctx     context.Context
	entered chan struct{}
}

func (r uploadCooperativeReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func TestRunningWriterExcludesRecoveryAndConcurrentUpload(t *testing.T) {
	u, d, _, log := uploadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := u.Upload(ctx, identity("member"), uploadImageID, uploadCooperativeReader{ctx, entered}, nil)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upload did not reach reader")
	}
	// Snapshot concurrently: no sleeps or timestamp inference establishes quiescence.
	before := len(log.snapshot())
	if _, err := u.RecoverUploads(context.Background()); !errors.Is(err, ErrUploadNotReady) {
		t.Fatal("recovery entered during writer", err)
	}
	if len(log.snapshot()) != before {
		t.Fatal("recovery touched SQL or store with active writer")
	}
	if err := u.Close(); !errors.Is(err, ErrUploadBusy) {
		t.Fatal("ownership released during writer", err)
	}
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil); !errors.Is(err, ErrUploadBusy) {
		t.Fatal("second upload admitted", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled writer leaked")
	}
	d.mu.Lock()
	if d.image.Status != "queued" || d.op.Phase != model.DataCleaned {
		t.Fatal("cancelled writer not cleaned")
	}
	d.mu.Unlock()
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCapacityAndLifecycleConfiguration(t *testing.T) {
	u, d, fs, _ := uploadFixture(t)
	for _, change := range []func(*UploadLimits){
		func(l *UploadLimits) { l.MaxBytes = 0 }, func(l *UploadLimits) { l.MaxBytes = MaxServerUploadBytes + 1 },
		func(l *UploadLimits) { l.MaxConcurrent = 0 }, func(l *UploadLimits) { l.MaxConcurrent = 65 },
		func(l *UploadLimits) { l.ResolutionTimeout = 0 }, func(l *UploadLimits) { l.ResolutionTimeout = 2 * time.Minute },
		func(l *UploadLimits) { l.RecoveryTimeout = 0 }, func(l *UploadLimits) { l.RecoveryTimeout = time.Hour },
	} {
		l := u.limits
		change(&l)
		if _, err := newUploadCoordinator(uploadCatalogDouble{sql: d}, d, fs, l); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	u.limits.MaxConcurrent = 1
	u.active["another-image"] = true
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil); !errors.Is(err, ErrUploadBusy) {
		t.Fatal("capacity exceeded", err)
	}
	delete(u.active, "another-image")
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil); !errors.Is(err, ErrUploadNotReady) {
		t.Fatal("closed upload accepted", err)
	}
}

// Startup no longer re-hashes committed images (official Glance does no
// startup scan): existence and size only, so startup is not O(total bytes).
func TestRecoveryChecksCommittedImagesBySizeOnly(t *testing.T) {
	for _, tc := range []string{"intact", "same-size-corruption", "truncated", "missing"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			switch tc {
			case "same-size-corruption":
				fs.blob[0] ^= 0xff
			case "truncated":
				fs.blob = fs.blob[:len(fs.blob)-1]
			case "missing":
				fs.blob = nil
			}
			u = uploadRestart(t, u, d, fs)
			log.reset()
			report, err := u.RecoverUploads(context.Background())
			if tc == "intact" || tc == "same-size-corruption" {
				if err != nil || report.Kept != 1 {
					t.Fatal(report, err)
				}
			} else if !errors.Is(err, ErrUploadReconciliation) {
				t.Fatal("missing or truncated committed content must stop startup", err)
			}
			for _, call := range log.snapshot() {
				if call == "fs.Open" {
					t.Fatal("startup re-read a committed image", log.snapshot())
				}
			}
		})
	}
}
