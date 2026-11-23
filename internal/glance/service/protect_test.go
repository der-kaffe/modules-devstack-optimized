package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/protect"
)

// As the original's property protections (roles format).
func TestPropertyProtections(t *testing.T) {
	file := filepath.Join(t.TempDir(), "property-protections.conf")
	_ = os.WriteFile(file, []byte("[^x_admin_.*]\ncreate = admin\nread = admin\nupdate = admin\ndelete = admin\n\n[^x_ro_.*]\ncreate = admin\nread = member,reader\nupdate = !\ndelete = admin\n\n[.*]\ncreate = @\nread = @\nupdate = @\ndelete = @\n"), 0o600)
	var err error
	if Protections, err = protect.Load(file, "roles", nil); err != nil {
		t.Fatal(err)
	}
	defer func() { Protections = nil }()
	ctx := context.Background()
	owner := project("owner", "member", "reader")
	s, repo := sharedFixture("private", "queued")
	if _, err = s.Create(ctx, owner, model.CreateInput{Properties: map[string]string{"x_admin_secret": "1"}}); domainKind(err) != model.Forbidden || err.Error() != "Attribute 'x_admin_secret' is reserved." {
		t.Fatal("protected create allowed", err)
	}
	repo.image.Properties = map[string]string{"x_admin_secret": "s", "x_ro_flag": "r", "free": "f"}
	got, err := s.Get(ctx, owner, sharedID)
	if err != nil || got.Properties["x_admin_secret"] != "" || got.Properties["x_ro_flag"] != "r" || got.Properties["free"] != "f" {
		t.Fatal("read protection", got.Properties, err)
	}
	if _, err = s.Patch(ctx, owner, sharedID, []model.PatchOperation{operation("replace", "free", `"g"`)}); err != nil {
		t.Fatal("unprotected change refused (untouched protected properties)", err)
	}
	if _, err = s.Patch(ctx, owner, sharedID, []model.PatchOperation{operation("replace", "x_ro_flag", `"z"`)}); domainKind(err) != model.Forbidden {
		t.Fatal("update ! allowed", err)
	}
	if _, err = s.Patch(ctx, project("owner", "admin"), sharedID, []model.PatchOperation{operation("remove", "x_admin_secret", "")}); err != nil {
		t.Fatal("admin delete refused", err)
	}
}
