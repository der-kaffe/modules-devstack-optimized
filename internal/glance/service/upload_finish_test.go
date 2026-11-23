package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// finishingSQL adds the repository's single final commit (FinishData) to the
// upload double, so the coordinator takes the two-commit path of the original.
type finishingSQL struct{ *uploadSQLDouble }

func (d finishingSQL) FinishData(ctx context.Context, id, attempt string, c model.DataContent, now time.Time) error {
	return d.run(ctx, "Finish", func() error {
		if err := d.check(id, attempt, model.DataReserved); err != nil {
			return err
		}
		if err := c.Validate(d.op.ExpectedSize); err != nil {
			return err
		}
		d.op.Phase, d.op.UpdatedAt = model.DataCommitted, now
		d.op.Size, d.op.VirtualSize, d.op.Checksum, d.op.SHA512 = uploadPtr(c.Size), c.VirtualSize, uploadPtr(c.Checksum), uploadPtr(c.SHA512)
		d.image.Status, d.image.Size, d.image.VirtualSize = "active", d.op.Size, d.op.VirtualSize
		d.image.Checksum, d.image.HashAlgorithm, d.image.HashValue = d.op.Checksum, uploadPtr("sha512"), d.op.SHA512
		return nil
	})
}

func finishingFixture(t *testing.T) (*UploadCoordinator, *uploadSQLDouble, *uploadStoreDouble, *uploadEvents) {
	t.Helper()
	u, d, fs, log := uploadFixture(t)
	next, err := newUploadCoordinator(uploadCatalogDouble{sql: d}, finishingSQL{d}, fs, u.limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = next.RecoverUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	log.reset()
	return next, d, fs, log
}

// As the original: one commit reserves (saving), one commit finishes
// (receipt, location, active) after the file is written and verified.
func TestUploadTwoCommitsLikeTheOriginal(t *testing.T) {
	u, d, fs, log := finishingFixture(t)
	size := int64(len(uploadPayload))
	c, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"catalog.Get", "sql.State", "sql.Reserve", "catalog.Get", "fs.Stage", "fs.Publish", "sql.Finish"}
	if got := log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatal("wrong order", got)
	}
	expected := uploadInfo(uploadPayload)
	if c.Checksum != expected.Checksum || c.SHA512 != expected.SHA512 || d.image.Status != "active" || d.op.Phase != model.DataCommitted ||
		*d.image.HashValue != expected.SHA512 || !bytes.Equal(fs.blob, uploadPayload) {
		t.Fatal("incomplete result", d.image.Status, d.op.Phase)
	}
}

func TestUploadFinishFailuresAreCompensatedOrResolved(t *testing.T) {
	for _, tc := range []string{"finish-error", "finish-uncertain-applied", "finish-uncertain-unapplied", "publish-error"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, _ := finishingFixture(t)
			switch tc {
			case "finish-error":
				d.faults["Finish"] = uploadSQLFault{err: errors.New("SQL unavailable")}
			case "finish-uncertain-applied":
				d.faults["Finish"] = uploadSQLFault{err: repository.ErrCommitUncertain, applied: true}
			case "finish-uncertain-unapplied":
				d.faults["Finish"] = uploadSQLFault{err: repository.ErrCommitUncertain}
			case "publish-error":
				fs.faults["Publish"] = uploadStoreFault{err: store.ErrIntegrity}
			}
			size := int64(len(uploadPayload))
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size)
			switch tc {
			case "finish-uncertain-applied":
				if err != nil || d.image.Status != "active" || d.op.Phase != model.DataCommitted {
					t.Fatal("applied commit not recognized", err)
				}
			case "finish-uncertain-unapplied":
				// The state is still reserved: never reported as success.
				if err == nil || d.image.Status == "active" {
					t.Fatal("unconfirmed commit reported as success")
				}
			default:
				// A definite failure before the final commit: the reservation is
				// cleaned up and the image is uploadable again, as the original.
				if err == nil || d.image.Status != "queued" || d.op.Phase != model.DataCleaned || fs.blob != nil {
					t.Fatal("not compensated", err, d.image.Status, d.op.Phase)
				}
			}
		})
	}
}
