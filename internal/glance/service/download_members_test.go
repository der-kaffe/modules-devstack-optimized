package service

import (
	"context"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// memberCatalog adds image_members to the catalog reader.
type memberCatalog struct {
	uploadCatalogDouble
	members map[string]bool
}

func (c memberCatalog) IsMember(_ context.Context, _, project string) (bool, error) {
	return c.members[project], nil
}

func TestDownloadMembersAndDeactivatedImages(t *testing.T) {
	u, d, fs, _ := uploadFixture(t)
	next, err := newUploadCoordinator(memberCatalog{uploadCatalogDouble{sql: d}, map[string]bool{"guest": true}}, d, fs, u.limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = next.RecoverUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	open := func(id string, roles ...string) error {
		out, err := next.OpenDownload(context.Background(), project(id, roles...), uploadImageID)
		if err == nil {
			_ = out.Body.Close()
		}
		return err
	}
	d.image.Owner, d.image.Visibility = "owner", "shared"
	if err = open("guest", "reader"); err != nil {
		t.Fatal("member could not download a shared image", err)
	}
	if err = open("stranger", "reader"); domainKind(err) != model.NotFound {
		t.Fatal("non-member downloaded a shared image", err)
	}
	d.image.Status = "deactivated"
	if err = open("owner", "reader", "member"); domainKind(err) != model.Forbidden {
		t.Fatal("deactivated image downloaded by its owner", err)
	}
	if err = open("x", "admin"); err != nil {
		t.Fatal("admin could not download a deactivated image", err)
	}
}
