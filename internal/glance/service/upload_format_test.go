package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// As the original's set_data with require_image_format_match: a bare
// upload whose bytes are not its disk_format is refused (415) and the image
// is restored to queued with no content; a raw upload gets its virtual size.
func TestUploadInspectsTheFormat(t *testing.T) {
	u, d, fs, _ := uploadFixture(t)
	u.limits.Inspect, u.limits.RequireFormat = true, true
	qcow2 := "qcow2"
	d.image.DiskFormat = &qcow2
	size := int64(len(uploadPayload))
	_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size)
	var domain *model.Error
	if !asDomain(err, &domain) || domain.Kind != model.Unsupported || domain.Message != "Image disk_format qcow2 does not match stream data raw" {
		t.Fatal("mismatch not refused", err)
	}
	if d.image.Status != "queued" || fs.blob != nil {
		t.Fatal("not restored", d.image.Status)
	}
	raw := "raw"
	d.image.DiskFormat = &raw
	c, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), &size)
	if err != nil || c.VirtualSize == nil || *c.VirtualSize != size {
		t.Fatal("raw upload", c, err)
	}
}

func asDomain(err error, target **model.Error) bool {
	e, ok := err.(*model.Error)
	if ok {
		*target = e
	}
	return ok
}

// As the original's user_storage_quota: refused before the upload when the
// declared size exceeds what remains, and cut by the limiting reader when
// the size is unknown; the image stays queued.
func TestUploadUserStorageQuota(t *testing.T) {
	for _, declare := range []bool{true, false} {
		u, d, fs, _ := uploadFixture(t)
		u.limits.StorageQuota = int64(len(uploadPayload)) - 1
		var size *int64
		if declare {
			n := int64(len(uploadPayload))
			size = &n
		}
		_, err := u.Upload(context.Background(), identity("member"), uploadImageID, bytes.NewReader(uploadPayload), size)
		var domain *model.Error
		if !asDomain(err, &domain) || domain.Kind != model.LimitExceeded {
			t.Fatal(declare, "quota not enforced", err)
		}
		if d.image.Status != "queued" || fs.blob != nil {
			t.Fatal(declare, "not restored", d.image.Status)
		}
	}
}
