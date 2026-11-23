//go:build integration

package mysql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

// MUTATING, opt-in: image_members, tags by path, deactivate/reactivate and
// the Keystone-limit usage on a copy of the original's real DDL.
func TestMySQLMembersTagsActionsUsage(t *testing.T) {
	if os.Getenv("GLANCE_GO_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable MySQL opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, _, official := mysql.IntegrationFixture(t, ctx)
	defer func() { _ = db.Close() }()
	r := mysql.IntegrationRepo(t, db, 5*time.Second, official)
	s := service.New(r, time.Now)
	id := func(project string, roles ...string) auth.Identity {
		return auth.Identity{UserID: "u", ProjectID: project, Roles: roles, ExpiresAt: time.Now().Add(time.Hour)}
	}
	owner, guest, admin := id("project-a", "member", "reader"), id("project-b", "member", "reader"), id("project-x", "admin")
	shared, name := "shared", "to-share"
	image, err := s.Create(ctx, owner, model.CreateInput{Name: &name, Visibility: &shared})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, guest, image.ID); err == nil {
		t.Fatal("non-member saw a shared image")
	}
	if _, err = s.CreateMember(ctx, owner, image.ID, guest.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateMember(ctx, owner, image.ID, guest.ProjectID); err == nil {
		t.Fatal("duplicate member")
	}
	if _, err = s.Get(ctx, guest, image.ID); err != nil {
		t.Fatal("pending member cannot see", err)
	}
	list := func(status string) int {
		page, err := s.List(ctx, guest, model.ListOptions{Limit: 25, MemberStatus: status})
		if err != nil {
			t.Fatal(err)
		}
		return len(page)
	}
	if list("") != 0 || list("pending") != 1 || list("all") != 1 {
		t.Fatal("member_status listing", list(""), list("pending"), list("all"))
	}
	if _, err = s.UpdateMember(ctx, guest, image.ID, guest.ProjectID, "accepted"); err != nil {
		t.Fatal(err)
	}
	if list("") != 1 || list("pending") != 0 {
		t.Fatal("accepted member not listed")
	}
	members, err := s.ListMembers(ctx, owner, image.ID)
	if err != nil || len(members) != 1 || members[0].Status != "accepted" {
		t.Fatal(members, err)
	}
	// Re-adding after removal works: the unique key includes deleted_at.
	if err = s.DeleteMember(ctx, owner, image.ID, guest.ProjectID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err = s.CreateMember(ctx, owner, image.ID, guest.ProjectID); err != nil {
		t.Fatal("re-add after delete", err)
	}
	// Tags by path.
	for _, tag := range []string{"t1", "t2", "t1"} {
		if err = s.AddTag(ctx, owner, image.ID, tag); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.RemoveTag(ctx, owner, image.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, owner, image.ID); len(got.Tags) != 1 || got.Tags[0] != "t2" {
		t.Fatal(got.Tags)
	}
	// Actions on an active image (status set as an upload would leave it).
	if _, err = db.ExecContext(ctx, "UPDATE `"+official+"`.images SET status='active', size=1048576 WHERE id=?", image.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO `"+official+"`.image_locations (image_id, value, meta_data, status, created_at, deleted) VALUES (?, 'file:///x', '{}', 'active', UTC_TIMESTAMP(), 0)", image.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Deactivate(ctx, admin, image.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, owner, image.ID); got.Status != "deactivated" {
		t.Fatal(got.Status)
	}
	usage, err := r.ProjectUsage(ctx, owner.ProjectID)
	if err != nil || usage.CountTotal != 1 || usage.SizeTotal != 1048576 || usage.CountUploading != 0 {
		t.Fatal("usage", usage, err)
	}
	if err = s.Reactivate(ctx, admin, image.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting the image soft-deletes its members, as image_destroy.
	if _, err = db.ExecContext(ctx, "UPDATE `"+official+"`.images SET status='queued', size=NULL WHERE id=?", image.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, owner, image.ID); err != nil {
		t.Fatal(err)
	}
	var live int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+official+"`.image_members WHERE image_id=? AND deleted=0", image.ID).Scan(&live); err != nil || live != 0 {
		t.Fatal("members survived the image", live, err)
	}
}
