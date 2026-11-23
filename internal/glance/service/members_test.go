package service

import (
	"context"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

// memberStub adds the image_members table to repositoryStub.
type memberStub struct {
	*repositoryStub
	members map[string]model.Member
}

func (r *memberStub) ListMembers(context.Context, string) ([]model.Member, error) {
	out := []model.Member{}
	for _, m := range r.members {
		out = append(out, m)
	}
	return out, nil
}
func (r *memberStub) GetMember(_ context.Context, _, id string) (model.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return m, repository.ErrMemberNotFound
	}
	return m, nil
}
func (r *memberStub) CreateMember(_ context.Context, image, id string, now time.Time, check func(model.Image, int) error) (model.Member, error) {
	if err := check(r.image, len(r.members)); err != nil {
		return model.Member{}, err
	}
	if _, ok := r.members[id]; ok {
		return model.Member{}, repository.ErrMemberDuplicate
	}
	m := model.Member{ImageID: image, MemberID: id, Status: "pending", CreatedAt: now, UpdatedAt: now}
	r.members[id] = m
	return m, nil
}
func (r *memberStub) UpdateMember(_ context.Context, _, id, status string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return m, repository.ErrMemberNotFound
	}
	if err := check(r.image, m); err != nil {
		return model.Member{}, err
	}
	m.Status, m.UpdatedAt = status, now
	r.members[id] = m
	return m, nil
}
func (r *memberStub) DeleteMember(_ context.Context, _, id string, _ time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return m, repository.ErrMemberNotFound
	}
	if err := check(r.image, m); err != nil {
		return model.Member{}, err
	}
	delete(r.members, id)
	return m, nil
}

const sharedID = "11111111-2222-4333-8444-555555555555"

func sharedFixture(visibility, status string) (*Service, *memberStub) {
	repo := &memberStub{repositoryStub: &repositoryStub{image: model.Image{ID: sharedID, Owner: "owner", Status: status, Visibility: visibility, Tags: []string{"a"}}}, members: map[string]model.Member{}}
	return New(repo, time.Now), repo
}

func project(id string, roles ...string) auth.Identity {
	return auth.Identity{UserID: "u", ProjectID: id, Roles: roles, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestMembersFollowTheOriginalPolicies(t *testing.T) {
	ctx := context.Background()
	owner, guest, other, admin := project("owner", "member", "reader"), project("guest", "member", "reader"), project("other", "member", "reader"), project("x", "admin")
	s, repo := sharedFixture("shared", "active")

	// A non-member does not see the image; a member sees it with any status.
	if _, err := s.Get(ctx, guest, sharedID); domainKind(err) != model.NotFound {
		t.Fatal("non-member saw a shared image", err)
	}
	if _, err := s.CreateMember(ctx, guest, sharedID, "guest"); domainKind(err) != model.NotFound {
		t.Fatal("non-member added itself", err)
	}
	m, err := s.CreateMember(ctx, owner, sharedID, "guest")
	if err != nil || m.Status != "pending" {
		t.Fatal(m, err)
	}
	if _, err = s.CreateMember(ctx, owner, sharedID, "guest"); domainKind(err) != model.Conflict {
		t.Fatal("duplicate member accepted", err)
	}
	if _, err = s.Get(ctx, guest, sharedID); err != nil {
		t.Fatal("pending member cannot see the image", err)
	}
	// Only the member changes its status; the owner cannot.
	if _, err = s.UpdateMember(ctx, owner, sharedID, "guest", "accepted"); domainKind(err) != model.Forbidden {
		t.Fatal("owner accepted for the member", err)
	}
	if _, err = s.UpdateMember(ctx, guest, sharedID, "guest", "maybe"); domainKind(err) != model.Invalid {
		t.Fatal("invalid status accepted", err)
	}
	if m, err = s.UpdateMember(ctx, guest, sharedID, "guest", "accepted"); err != nil || m.Status != "accepted" {
		t.Fatal(m, err)
	}
	// A member lists only its own membership, sees but cannot modify.
	if _, err = s.CreateMember(ctx, owner, sharedID, "third"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListMembers(ctx, guest, sharedID); err != nil || len(list) != 1 || list[0].MemberID != "guest" {
		t.Fatal("member listed others", list, err)
	}
	if list, err := s.ListMembers(ctx, owner, sharedID); err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	if _, err = s.GetMember(ctx, guest, sharedID, "third"); domainKind(err) != model.NotFound {
		t.Fatal("member read another membership", err)
	}
	if _, err = s.Patch(ctx, guest, sharedID, []model.PatchOperation{operation("replace", "name", `"x"`)}); domainKind(err) != model.Forbidden {
		t.Fatal("member modified the image", err)
	}
	if err = s.DeleteMember(ctx, guest, sharedID, "guest"); domainKind(err) != model.Forbidden {
		t.Fatal("member removed a membership", err)
	}
	if err = s.DeleteMember(ctx, admin, sharedID, "third"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetMember(ctx, other, sharedID, "guest"); domainKind(err) != model.NotFound {
		t.Fatal("non-member read a membership", err)
	}
	if len(repo.members) != 1 {
		t.Fatal(repo.members)
	}
	// Only shared images have members.
	private, _ := sharedFixture("private", "active")
	if _, err = private.CreateMember(ctx, owner, sharedID, "guest"); domainKind(err) != model.Forbidden {
		t.Fatal("member added to a private image", err)
	}
}

func TestMemberQuota(t *testing.T) {
	s, repo := sharedFixture("shared", "queued")
	for n := 0; n < model.MaxImageMembers; n++ {
		repo.members[string(rune('A'+n%26))+string(rune('a'+n/26))] = model.Member{}
	}
	if _, err := s.CreateMember(context.Background(), project("owner", "member"), sharedID, "new"); domainKind(err) != model.LimitExceeded {
		t.Fatal("member quota not enforced", err)
	}
}

func TestTagsByPath(t *testing.T) {
	ctx := context.Background()
	owner := project("owner", "member", "reader")
	s, repo := sharedFixture("private", "queued")
	for _, tag := range []string{"b", "a", "b"} {
		if err := s.AddTag(ctx, owner, sharedID, tag); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.image.Tags) != 2 {
		t.Fatal("duplicate tag stored", repo.image.Tags)
	}
	if err := s.RemoveTag(ctx, owner, sharedID, "a"); err != nil || len(repo.image.Tags) != 1 || repo.image.Tags[0] != "b" {
		t.Fatal(repo.image.Tags, err)
	}
	if err := s.RemoveTag(ctx, owner, sharedID, "a"); domainKind(err) != model.NotFound {
		t.Fatal("missing tag removed", err)
	}
	if err := s.AddTag(ctx, project("other", "member"), sharedID, "c"); domainKind(err) != model.NotFound {
		t.Fatal("other project tagged a private image", err)
	}
	long := make([]rune, 256)
	for n := range long {
		long[n] = 'x'
	}
	if err := s.AddTag(ctx, owner, sharedID, string(long)); domainKind(err) != model.Invalid {
		t.Fatal("long tag accepted", err)
	}
	repo.image.Tags = make([]string, 128)
	for n := range repo.image.Tags {
		repo.image.Tags[n] = string(rune(0x100 + n))
	}
	if err := s.AddTag(ctx, owner, sharedID, "z"); domainKind(err) != model.LimitExceeded {
		t.Fatal("tag quota not enforced", err)
	}
}

func TestDeactivateReactivate(t *testing.T) {
	ctx := context.Background()
	admin, owner := project("x", "admin"), project("owner", "member", "reader")
	s, repo := sharedFixture("private", "active")
	if err := s.Deactivate(ctx, project("other", "member", "reader"), sharedID); domainKind(err) != model.NotFound {
		t.Fatal("another project deactivated", err)
	}
	if err := s.Deactivate(ctx, owner, sharedID); err != nil || repo.image.Status != "deactivated" {
		t.Fatal("owner member could not deactivate (original default)", err)
	}
	repo.image.Status = "active"
	for range 2 { // the second call is a no-op, as the original
		if err := s.Deactivate(ctx, admin, sharedID); err != nil || repo.image.Status != "deactivated" {
			t.Fatal(repo.image.Status, err)
		}
	}
	for range 2 {
		if err := s.Reactivate(ctx, admin, sharedID); err != nil || repo.image.Status != "active" {
			t.Fatal(repo.image.Status, err)
		}
	}
	repo.image.Status = "queued"
	if err := s.Deactivate(ctx, admin, sharedID); domainKind(err) != model.Forbidden {
		t.Fatal("queued image deactivated", err)
	}
	if err := s.Reactivate(ctx, admin, sharedID); domainKind(err) != model.Forbidden {
		t.Fatal("queued image reactivated", err)
	}
}
