package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

func TestUploadSuccessAndCallOrder(t *testing.T) {
	u, d, fs, log := uploadFixture(t)
	size := int64(len(uploadPayload))
	c, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size)
	if err != nil {
		t.Fatal(err)
	}
	expected := uploadInfo(uploadPayload)
	if c.Size != size || c.VirtualSize == nil || *c.VirtualSize != size || c.Checksum != expected.Checksum || c.SHA512 != expected.SHA512 {
		t.Fatal(c)
	}
	want := []string{"catalog.Get", "sql.State", "sql.Reserve", "sql.State", "catalog.Get", "fs.Stage", "sql.Stage", "sql.State", "fs.Publish", "sql.State", "fs.OpenStream", "fs.Close", "sql.Publish", "sql.State", "sql.Commit", "sql.State"}
	if got := log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatal("wrong order", got)
	}
	if fs.lastKey.ImageID != uploadImageID || fs.lastKey.AttemptID == oldUploadAttempt {
		t.Fatal(fs.lastKey)
	}
	if id, err := uuid.Parse(fs.lastKey.AttemptID); err != nil || id.Version() != 4 {
		t.Fatal("attempt not generated on server")
	}
	if d.image.Status != "active" || d.op.Phase != model.DataCommitted || !bytes.Equal(fs.blob, uploadPayload) {
		t.Fatal("incomplete result")
	}
}

func TestUploadRejectsBeforeReservation(t *testing.T) {
	for _, tc := range []string{"reader", "foreign-private", "foreign-public", "expired", "unscoped", "missing", "active", "missing-format", "unsupported-format", "negative-length", "size-limit", "nil-source", "cancelled", "bad-id", "cleanup-pending", "not-ready"} {
		t.Run(tc, func(t *testing.T) {
			u, d, _, log := uploadFixture(t)
			who := identity("member")
			id := uploadImageID
			var expected *int64
			var src io.Reader = bytes.NewReader(uploadPayload)
			ctx := context.Background()
			switch tc {
			case "reader":
				who = identity("reader")
			case "foreign-private":
				who.ProjectID = "other"
				d.image.Visibility = "private"
			case "foreign-public":
				who.ProjectID = "other"
				d.image.Visibility = "public"
			case "expired":
				who.ExpiresAt = who.ExpiresAt.Add(-2 * time.Hour)
			case "unscoped":
				who.ProjectID = ""
			case "missing":
				u.images = uploadCatalogDouble{sql: d, err: repository.ErrNotFound}
			case "active":
				d.image.Status = "active"
			case "missing-format":
				d.image.DiskFormat = nil
			case "unsupported-format":
				d.image.DiskFormat = uploadPtr("invalid")
			case "negative-length":
				expected = uploadPtr(int64(-1))
			case "size-limit":
				expected = uploadPtr(int64(1025))
			case "nil-source":
				src = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "bad-id":
				id = "bad"
			case "cleanup-pending":
				uploadSeed(d, model.DataCleanup)
				d.image.Status = "queued"
			case "not-ready":
				u.ready = false
			}
			_, err := u.Upload(ctx, who, id, src, expected)
			if err == nil {
				t.Fatal("invalid upload accepted")
			}
			for _, call := range log.snapshot() {
				if call == "sql.Reserve" || strings.HasPrefix(call, "fs.") {
					t.Fatal("denied upload wrote", log.snapshot())
				}
			}
			if tc == "reader" && domainKind(err) != model.Forbidden {
				t.Fatal(err)
			}
			if tc == "foreign-private" && domainKind(err) != model.NotFound {
				t.Fatal(err)
			}
			if tc == "foreign-public" && domainKind(err) != model.Forbidden {
				t.Fatal(err)
			}
		})
	}
}

func TestUploadAdminProtectedAndNonRawVirtualSize(t *testing.T) {
	for _, format := range []string{"raw", "qcow2", "vmdk"} {
		t.Run(format, func(t *testing.T) {
			u, d, _, _ := uploadFixture(t)
			d.image.Protected = true
			d.image.DiskFormat = uploadPtr(format)
			who := identity("admin")
			who.ProjectID = "other"
			c, err := u.Upload(context.Background(), who, uploadImageID, bytes.NewReader(uploadPayload), nil)
			if err != nil || (c.VirtualSize != nil) != (format == "raw") {
				t.Fatal(c, err)
			}
		})
	}
}

type uploadBrokenReader struct{}

func (uploadBrokenReader) Read([]byte) (int, error) {
	return 0, errors.New("private source diagnostic")
}

func TestUploadFailureMatrixAndConfirmedCleanup(t *testing.T) {
	for _, tc := range []string{"wrong-length", "too-large", "reader", "reader-partial", "stage", "stage-after-ready", "StageData", "Publish", "publish-after-blob", "publish-exists-matching", "PublishData", "CommitData", "cancel-during-stage", "cancel-after-publish"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var expected *int64
			var src io.Reader = bytes.NewReader(uploadPayload)
			switch tc {
			case "wrong-length":
				expected = uploadPtr(int64(25))
			case "too-large":
				u.limits.MaxBytes = 4
			case "reader":
				src = uploadBrokenReader{}
			case "reader-partial":
				src = io.MultiReader(bytes.NewReader(uploadPayload[:5]), uploadBrokenReader{})
			case "stage":
				fs.faults["Stage"] = uploadStoreFault{err: store.ErrStoreFull}
			case "stage-after-ready":
				fs.faults["Stage"] = uploadStoreFault{err: errors.New("sync stage failed"), applied: true}
			case "StageData":
				d.faults["Stage"] = uploadSQLFault{err: errors.New("SQL unavailable")}
			case "Publish":
				fs.faults["Publish"] = uploadStoreFault{err: errors.New("publish failed")}
			case "publish-after-blob":
				fs.faults["Publish"] = uploadStoreFault{err: errors.New("directory fsync not confirmed"), applied: true}
			case "publish-exists-matching":
				fs.faults["Publish"] = uploadStoreFault{err: store.ErrExists, applied: true}
			case "PublishData":
				d.faults["Publish"] = uploadSQLFault{err: errors.New("SQL unavailable")}
			case "CommitData":
				d.faults["Commit"] = uploadSQLFault{err: errors.New("SQL unavailable")}
			case "cancel-during-stage":
				fs.after = func(name string, _ store.Key) {
					if name == "Stage" {
						cancel()
					}
				}
			case "cancel-after-publish":
				fs.after = func(name string, _ store.Key) {
					if name == "Publish" {
						cancel()
					}
				}
			}
			_, err := u.Upload(ctx, identity("member"), uploadImageID, src, expected)
			if err == nil || errors.Is(err, ErrUploadReconciliation) {
				t.Fatal("known failure not safely cleaned", err, log.snapshot())
			}
			if d.image.Status != "queued" || d.op == nil || d.op.Phase != model.DataCleaned || fs.part || fs.ready || fs.blob != nil {
				t.Fatal("cleanup incomplete", d.image.Status, d.op, log.snapshot())
			}
			calls := log.snapshot()
			begin, del, complete := -1, -1, -1
			for n, c := range calls {
				switch c {
				case "sql.BeginCleanup":
					begin = n
				case "fs.Delete":
					del = n
				case "sql.CompleteCleanup":
					complete = n
				}
			}
			if begin < 0 || del <= begin || complete <= del || calls[del-1] != "sql.State" || calls[complete-1] != "sql.State" {
				t.Fatal("cleanup not fenced", calls)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("raw secret escaped", err)
			}
		})
	}
}

func TestUploadUncertainSQLResolution(t *testing.T) {
	for _, step := range []string{"Reserve", "Stage", "Publish", "Commit"} {
		for _, applied := range []bool{false, true} {
			t.Run(step+map[bool]string{false: "-not-applied", true: "-applied"}[applied], func(t *testing.T) {
				u, d, fs, log := uploadFixture(t)
				d.faults[step] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: applied}
				_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
				if applied {
					if err != nil || d.image.Status != "active" || d.op.Phase != model.DataCommitted || fs.blob == nil {
						t.Fatal("confirmed write not handled", err, log.snapshot())
					}
					for _, c := range log.snapshot() {
						if c == "fs.Delete" {
							t.Fatal("deleted confirmed active bytes")
						}
					}
				} else {
					if err == nil || d.image.Status != "queued" || fs.blob != nil {
						t.Fatal("unapplied write reported success", err)
					}
					if step == "Reserve" {
						if !errors.Is(err, ErrUploadUnavailable) || errors.Is(err, ErrUploadReconciliation) {
							t.Fatal("unapplied reservation was not resolved as a definite failure", err)
						}
						for _, c := range log.snapshot() {
							if strings.HasPrefix(c, "fs.") {
								t.Fatal("I/O without confirmed reservation")
							}
						}
					} else if d.op.Phase != model.DataCleaned {
						t.Fatal("unapplied write not cleaned")
					}
				}
			})
		}
	}
}

func TestUnresolvedAndUnsafeStatesRetainEvidence(t *testing.T) {
	for _, tc := range []string{"reservation-read", "staged-read", "committed-read", "publish-open", "publish-bad-integrity", "stage-exists", "late-attempt", "tombstone-after-stage", "tombstone-after-publish", "tombstone-before-finalize", "active-on-failure"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			switch tc {
			case "reservation-read":
				d.faults["Reserve"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				d.failReadAt[2] = true
			case "staged-read":
				d.faults["Stage"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				d.failReadAt[3] = true
			case "committed-read":
				d.faults["Commit"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
				d.failReadAt[6] = true
			case "publish-open":
				fs.faults["Publish"] = uploadStoreFault{err: errors.New("sync failed"), applied: true}
				fs.faults["Open"] = uploadStoreFault{err: errors.New("unavailable")}
			case "publish-bad-integrity":
				fs.faults["Publish"] = uploadStoreFault{err: store.ErrIntegrity, applied: true}
				fs.after = func(n string, _ store.Key) {
					if n == "Publish" {
						fs.mu.Lock()
						fs.blob = []byte("corrupt")
						fs.mu.Unlock()
					}
				}
			case "stage-exists":
				fs.faults["Stage"] = uploadStoreFault{err: store.ErrExists}
			case "late-attempt":
				d.after = func(n string) {
					if n == "Stage" {
						d.mu.Lock()
						d.op.AttemptID = oldUploadAttempt
						d.mu.Unlock()
					}
				}
			case "tombstone-after-stage":
				d.after = func(n string) {
					if n == "Stage" {
						d.mu.Lock()
						d.image.Status = "deleted"
						d.mu.Unlock()
					}
				}
			case "tombstone-after-publish":
				fs.after = func(n string, _ store.Key) {
					if n == "Publish" {
						d.mu.Lock()
						d.image.Status = "deleted"
						d.mu.Unlock()
					}
				}
			case "tombstone-before-finalize":
				d.before = func(name string) {
					if name == "Commit" {
						d.mu.Lock()
						d.image.Status = "deleted"
						d.op.Phase = model.DataCleanup
						d.op.CleanupTarget = uploadPtr("deleted")
						d.mu.Unlock()
					}
				}
			case "active-on-failure":
				d.faults["Commit"] = uploadSQLFault{err: errors.New("non-uncertain failure"), applied: true}
			}
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			if err == nil {
				t.Fatal("uncertainty reported as success", log.snapshot())
			}
			for _, c := range log.snapshot() {
				if c == "fs.Delete" || c == "sql.CompleteCleanup" {
					t.Fatal("unsafe compensation", log.snapshot())
				}
			}
			if tc == "active-on-failure" && (d.image.Status != "active" || fs.blob == nil) {
				t.Fatal("active image lost")
			}
		})
	}
}

func TestCleanupFailuresPreserveFence(t *testing.T) {
	for _, tc := range []string{"Begin-known", "Begin-unknown-applied", "Begin-unknown-not-applied", "Delete", "Delete-after-unlink", "Complete-known", "Complete-unknown-applied", "Complete-unknown-not-applied", "cleanup-read"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			fs.faults["Stage"] = uploadStoreFault{err: store.ErrStoreFull}
			switch tc {
			case "Begin-known":
				d.faults["BeginCleanup"] = uploadSQLFault{err: errors.New("unavailable")}
			case "Begin-unknown-applied":
				d.faults["BeginCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
			case "Begin-unknown-not-applied":
				d.faults["BeginCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "Delete":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("unlink failed")}
			case "Delete-after-unlink":
				fs.faults["Delete"] = uploadStoreFault{err: errors.New("directory sync failed"), applied: true}
			case "Complete-known":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: errors.New("unavailable")}
			case "Complete-unknown-applied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
			case "Complete-unknown-not-applied":
				d.faults["CompleteCleanup"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "cleanup-read":
				d.failReadAt[4] = true
			}
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			cleaned := tc == "Begin-unknown-applied" || tc == "Complete-unknown-applied"
			if err == nil || (d.image.Status == "queued") != cleaned {
				t.Fatal("wrong cleanup outcome", d.image.Status, err, log.snapshot())
			}
			if !cleaned && !errors.Is(err, ErrUploadReconciliation) {
				t.Fatal(err)
			}
			if strings.HasPrefix(tc, "Begin-") && !cleaned || tc == "cleanup-read" {
				for _, c := range log.snapshot() {
					if c == "fs.Delete" {
						t.Fatal("deletion without confirmed cleanup")
					}
				}
			}
			if strings.HasPrefix(tc, "Delete") {
				for _, c := range log.snapshot() {
					if c == "sql.CompleteCleanup" {
						t.Fatal("confirmed cleanup after Delete error")
					}
				}
			}
		})
	}
}

func TestUploadUsesFormatsReadAfterReservation(t *testing.T) {
	u, d, _, _ := uploadFixture(t)
	d.after = func(n string) {
		if n == "Reserve" {
			d.mu.Lock()
			d.image.DiskFormat = uploadPtr("qcow2")
			d.mu.Unlock()
		}
	}
	c, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
	if err != nil || c.VirtualSize != nil {
		t.Fatal("used stale raw format", c, err)
	}
}

func TestCancellationAtSQLBoundaries(t *testing.T) {
	for _, step := range []string{"Reserve", "Stage", "Publish", "Commit"} {
		t.Run(step, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.after = func(name string) {
				if name == step {
					cancel()
				}
			}
			_, err := u.Upload(ctx, identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			if step == "Commit" {
				if err != nil || d.image.Status != "active" || fs.blob == nil {
					t.Fatal("lost confirmed result on cancellation", err)
				}
				for _, c := range log.snapshot() {
					if c == "fs.Delete" {
						t.Fatal("deleted committed object")
					}
				}
			} else if !errors.Is(err, context.Canceled) || d.image.Status != "queued" || d.op.Phase != model.DataCleaned {
				t.Fatal("cancelled intermediate state not cleaned", err)
			}
		})
	}
}

func TestTombstoneAfterCommitIsNotRetrospectiveGone(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "lost-ack-resolved"}[uncertain], func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			if uncertain {
				d.faults["Commit"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
			}
			d.after = func(name string) {
				if name == "Commit" {
					d.mu.Lock()
					d.image.Status = "deleted"
					d.op.Phase = model.DataCleanup
					d.op.CleanupTarget = uploadPtr("deleted")
					d.mu.Unlock()
				}
			}
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			if err != nil || d.image.Status != "deleted" || fs.blob == nil {
				t.Fatal("retrospective upload error/restoration", err)
			}
			for _, c := range log.snapshot() {
				if c == "fs.Delete" || c == "sql.BeginCleanup" {
					t.Fatal("upload handled DELETE outside scope")
				}
			}
		})
	}
}

func TestCompensationReChecksFenceBeforeAndAfterDeletion(t *testing.T) {
	for _, when := range []string{"before-cleanup", "after-cleanup", "after-delete"} {
		t.Run(when, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			fs.faults["Stage"] = uploadStoreFault{err: store.ErrStoreFull}
			if when == "after-delete" {
				fs.after = func(name string, _ store.Key) {
					if name == "Delete" {
						d.mu.Lock()
						d.image.Status = "deleted"
						d.op.CleanupTarget = uploadPtr("deleted")
						d.mu.Unlock()
					}
				}
			} else {
				d.after = func(name string) {
					if name == "BeginCleanup" {
						d.mu.Lock()
						d.image.Status = "deleted"
						d.op.CleanupTarget = uploadPtr("deleted")
						d.mu.Unlock()
					}
				}
				if when == "before-cleanup" {
					d.before = func(name string) {
						if name == "BeginCleanup" {
							d.mu.Lock()
							d.image.Status = "active"
							d.op.Phase = model.DataCommitted
							info := uploadInfo(uploadPayload)
							d.op.Size = uploadPtr(info.Size)
							d.op.Checksum = uploadPtr(info.Checksum)
							d.op.SHA512 = uploadPtr(info.SHA512)
							d.mu.Unlock()
						}
					}
					d.after = nil
				}
			}
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
			if err == nil || d.image.Status == "queued" {
				t.Fatal("restored over newer image state", err)
			}
			for _, c := range log.snapshot() {
				if c == "sql.CompleteCleanup" || c == "fs.Delete" && when != "after-delete" {
					t.Fatal("late cleanup crossed fence", log.snapshot())
				}
			}
		})
	}
}

func TestDefiniteReservationFailureLeavesNoIntentionOrFiles(t *testing.T) {
	u, d, _, log := uploadFixture(t)
	d.faults["Reserve"] = uploadSQLFault{err: errors.New("private DSN SQL error")}
	if _, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil); !errors.Is(err, ErrUploadUnavailable) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	if d.op != nil || d.image.Status != "queued" {
		t.Fatal("unexpected intent")
	}
	for _, c := range log.snapshot() {
		if strings.HasPrefix(c, "fs.") || c == "sql.BeginCleanup" {
			t.Fatal("compensated absent intent")
		}
	}
}

// A1: a successful Publish is not re-read (presence and size only, as official
// Glance never re-reads an upload); a failed Publish keeps full verification
// before deciding whether to compensate.
func TestUploadVerifiesFullyOnlyAfterPublishFailure(t *testing.T) {
	for _, failing := range []bool{false, true} {
		u, _, fs, log := uploadFixture(t)
		if failing {
			fs.faults["Publish"] = uploadStoreFault{err: errors.New("secret fsync"), applied: true}
		}
		_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), nil)
		if (err != nil) != failing {
			t.Fatal(failing, err)
		}
		calls := strings.Join(log.snapshot(), ",")
		if strings.Contains(calls, "fs.Open,") != failing || !failing && !strings.Contains(calls, "fs.OpenStream") {
			t.Fatal(failing, calls)
		}
	}
}
