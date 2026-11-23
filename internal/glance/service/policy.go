package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/protect"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/policy"
)

// Policy is the oslo.policy file of glance-api.conf ([oslo_policy]
// policy_file), set once at startup. Without one (the default, and
// DevStack's) the original's default rules apply as written in this
// package's checks; with one, each action is decided by its rules, with
// the target the original builds (owner as project_id, visibility,
// member_id, the image's properties).
var Policy *policy.Enforcer

func overridden() bool { return Policy.Overridden() }

func credentials(who auth.Identity) policy.Credentials {
	return policy.Credentials{Roles: who.Roles, ProjectID: who.ProjectID, UserID: who.UserID, IsAdmin: who.HasRole("admin")}
}

func denied(action string) error {
	return model.Fail(model.Forbidden, "You are not authorized to complete "+action+" action.")
}

// Permit evaluates action against target when a policy file is in use;
// otherwise it returns def, the default rule's outcome.
func Permit(who auth.Identity, action string, target map[string]any, def error) error {
	if !overridden() {
		return def
	}
	if target == nil {
		target = map[string]any{}
	}
	if _, ok := target["project_id"]; !ok {
		target["project_id"] = who.ProjectID
	}
	if Policy.Allowed(action, target, credentials(who)) {
		return nil
	}
	return denied(action)
}

// imageTarget is the original's ImageTarget.
func (s *Service) imageTarget(ctx context.Context, who auth.Identity, image model.Image) map[string]any {
	t := map[string]any{}
	for k, v := range image.Properties {
		t[k] = v
	}
	t["id"], t["project_id"], t["owner"], t["visibility"], t["status"] = image.ID, image.Owner, image.Owner, image.Visibility, image.Status
	t["protected"] = fmt.Sprint(image.Protected)
	// member_id costs a query: resolved only if a rule reads it, once.
	t["member_id"] = policy.Lazy(sync.OnceValue(func() any {
		if member, err := s.member(ctx, who, image); err == nil && member {
			return who.ProjectID
		}
		return ""
	}))
	return t
}

// permitImage is Permit for an image the caller can see.
func (s *Service) permitImage(ctx context.Context, who auth.Identity, action string, image model.Image, def error) error {
	if !overridden() {
		return def
	}
	return Permit(who, action, s.imageTarget(ctx, who, image), def)
}

// visibilityTarget: the target of a create or visibility change.
func visibilityTarget(who auth.Identity, image model.Image) map[string]any {
	return map[string]any{"project_id": image.Owner, "owner": image.Owner, "visibility": image.Visibility, "member_id": ""}
}

// Protections is the property protection file ([DEFAULT]
// property_protection_file), nil when none is configured.
var Protections *protect.Rules

func reserved(key string) error {
	return model.Fail(model.Forbidden, "Attribute '"+key+"' is reserved.")
}

func protectedAllowed(who auth.Identity, key, op string) bool {
	return Protections.Allowed(key, op, who.Roles, credentials(who))
}

// readable hides the properties the caller may not read, as the
// original's ExtraPropertiesProxy.
func readable(who auth.Identity, image model.Image) model.Image {
	if Protections == nil {
		return image
	}
	props := map[string]string{}
	for k, v := range image.Properties {
		if protectedAllowed(who, k, "read") {
			props[k] = v
		}
	}
	image.Properties = props
	return image
}

// protectedChange checks a property change: a new property is create, a
// changed one update, a removed one delete; an unreadable property does not
// exist for the caller.
func protectedChange(who auth.Identity, before, after map[string]string) error {
	if Protections == nil {
		return nil
	}
	for k, v := range after {
		old, existed := before[k]
		if existed && old == v {
			continue // untouched
		}
		switch {
		case !existed || !protectedAllowed(who, k, "read"):
			if !protectedAllowed(who, k, "create") {
				return reserved(k)
			}
		case old != v && !protectedAllowed(who, k, "update"):
			return reserved(k)
		}
	}
	for k := range before {
		if _, kept := after[k]; !kept {
			if !protectedAllowed(who, k, "read") {
				return model.Fail(model.Conflict, "Property "+k+" does not exist.")
			}
			if !protectedAllowed(who, k, "delete") {
				return reserved(k)
			}
		}
	}
	return nil
}
