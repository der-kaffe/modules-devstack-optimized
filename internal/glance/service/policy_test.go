package service

import (
	"context"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/policy"
)

// With a policy file the action's rule decides, granting or refusing
// beyond the defaults; without one the defaults apply unchanged.
func TestPolicyFileOverridesDefaults(t *testing.T) {
	ctx := context.Background()
	defer func() { Policy = nil }()
	owner := project("owner", "member", "reader")
	s, repo := sharedFixture("private", "active")
	public := "public"
	if _, err := s.Create(ctx, owner, model.CreateInput{Visibility: &public}); domainKind(err) != model.Forbidden {
		t.Fatal("member published without a policy file", err)
	}
	var err error
	if Policy, err = policy.New(map[string]string{"publicize_image": "role:member", "get_image": "!", "deactivate": "role:admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, owner, model.CreateInput{Visibility: &public}); err != nil {
		t.Fatal("publicize_image override not applied", err)
	}
	repo.image = model.Image{ID: sharedID, Owner: "owner", Status: "active", Visibility: "private"}
	if _, err = s.Get(ctx, owner, sharedID); domainKind(err) != model.Forbidden {
		t.Fatal("get_image ! not applied", err)
	}
	if err = s.Deactivate(ctx, owner, sharedID); domainKind(err) != model.Forbidden {
		t.Fatal("deactivate override not applied", err)
	}
	if _, err = s.Get(ctx, project("other", "admin"), sharedID); domainKind(err) != model.Forbidden {
		t.Fatal("! applies to admin too", err)
	}
}
