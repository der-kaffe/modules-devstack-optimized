package service

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

// Image members, tags and actions with the original's default policies:
//   add_member, delete_member: admin or a member of the owner project
//   get_member(s):             admin, the owner or that member (reader)
//   modify_member:             admin or the member project itself
//   add/delete tag:            modify_image (admin or the owner, member role)
//   deactivate, reactivate:    admin or a member of the owner project

var errNoMembers = errors.New("image members are not available")

func (s *Service) memberRepo() (repository.Members, error) {
	m, ok := s.repo.(repository.Members)
	if !ok {
		return nil, errNoMembers
	}
	return m, nil
}

// member reports whether who's project is a member of a shared image (any
// status), as the original's is_image_visible.
func (s *Service) member(ctx context.Context, who auth.Identity, image model.Image) (bool, error) {
	if image.Visibility != "shared" || image.Owner == who.ProjectID {
		return false, nil
	}
	repo, err := s.memberRepo()
	if err != nil {
		return false, nil
	}
	_, err = repo.GetMember(ctx, image.ID, who.ProjectID)
	if errors.Is(err, repository.ErrMemberNotFound) {
		return false, nil
	}
	return err == nil, err
}

// visible applies the read policy of one image: admin, owner, public,
// community, or a member of a shared image.
func (s *Service) visible(ctx context.Context, who auth.Identity, image model.Image) error {
	if who.HasRole("admin") || image.Owner == who.ProjectID || image.Visibility == "public" || image.Visibility == "community" {
		return nil
	}
	ok, err := s.member(ctx, who, image)
	if err != nil {
		return err
	}
	if !ok {
		return notFound(image.ID)
	}
	return nil
}

func memberNotFound() error { return model.Fail(model.NotFound, "Image member not found.") }

func memberError(err error, id string) error {
	switch {
	case errors.Is(err, repository.ErrMemberNotFound):
		return memberNotFound()
	case errors.Is(err, repository.ErrMemberDuplicate):
		return model.Fail(model.Conflict, "The image already has this member.") // CreateMember names it
	}
	return mutationError(err, id)
}

func (s *Service) memberStart(who auth.Identity, imageID string) (repository.Members, string, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, "", err
	}
	id, err := model.CanonicalID(imageID)
	if err != nil {
		return nil, "", err
	}
	repo, err := s.memberRepo()
	if err != nil {
		return nil, "", err
	}
	return repo, id, nil
}

func (s *Service) ListMembers(ctx context.Context, who auth.Identity, imageID string) ([]model.Member, error) {
	repo, id, err := s.memberStart(who, imageID)
	if err != nil {
		return nil, err
	}
	image, err := s.Get(ctx, who, id)
	if err != nil {
		return nil, err
	}
	if image.Visibility != "shared" {
		return nil, model.Fail(model.Forbidden, "Only shared images have members.")
	}
	if err = s.permitImage(ctx, who, "get_members", image, nil); err != nil {
		return nil, err
	}
	members, err := repo.ListMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	if who.HasRole("admin") || image.Owner == who.ProjectID {
		return members, nil
	}
	// A member sees only its own membership.
	own := []model.Member{}
	for _, m := range members {
		if m.MemberID == who.ProjectID {
			own = append(own, m)
		}
	}
	return own, nil
}

func (s *Service) GetMember(ctx context.Context, who auth.Identity, imageID, memberID string) (model.Member, error) {
	repo, id, err := s.memberStart(who, imageID)
	if err != nil {
		return model.Member{}, err
	}
	image, err := s.Get(ctx, who, id)
	if err != nil {
		return model.Member{}, err
	}
	if overridden() {
		target := s.imageTarget(ctx, who, image)
		target["member_id"] = memberID
		if err = Permit(who, "get_member", target, nil); err != nil {
			return model.Member{}, err
		}
	} else if !who.HasRole("admin") && image.Owner != who.ProjectID && memberID != who.ProjectID {
		return model.Member{}, memberNotFound()
	}
	m, err := repo.GetMember(ctx, id, memberID)
	return m, memberError(err, id)
}

func ownerPolicy(who auth.Identity, image model.Image, action ...string) error {
	if overridden() && len(action) > 0 {
		return Permit(who, action[0], visibilityTarget(who, image), nil)
	}
	if who.HasRole("admin") {
		return nil
	}
	if image.Owner != who.ProjectID || !who.HasRole("member") {
		return model.Fail(model.Forbidden, "Only the image owner can manage its members.")
	}
	return nil
}

func (s *Service) CreateMember(ctx context.Context, who auth.Identity, imageID, memberID string) (model.Member, error) {
	repo, id, err := s.memberStart(who, imageID)
	if err != nil {
		return model.Member{}, err
	}
	if err = model.ValidMemberID(memberID); err != nil {
		return model.Member{}, err
	}
	m, err := repo.CreateMember(ctx, id, memberID, s.now(), func(image model.Image, count int) error {
		if err := s.visible(ctx, who, image); err != nil {
			return err
		}
		if err := ownerPolicy(who, image, "add_member"); err != nil {
			return err
		}
		if image.Visibility != "shared" {
			return model.Fail(model.Forbidden, "Only shared images have members.")
		}
		if model.Over(count+1, model.Quotas.Members) {
			return model.Fail(model.LimitExceeded, "The limit of image members has been exceeded.")
		}
		return nil
	})
	if errors.Is(err, repository.ErrMemberDuplicate) {
		return model.Member{}, model.Fail(model.Conflict, "Member "+memberID+" is duplicated for image "+id)
	}
	if err != nil {
		return model.Member{}, memberError(err, id)
	}
	s.notify("image.member.create", notify.MemberPayload(m, false, time.Time{}))
	return m, nil
}

func (s *Service) UpdateMember(ctx context.Context, who auth.Identity, imageID, memberID, status string) (model.Member, error) {
	repo, id, err := s.memberStart(who, imageID)
	if err != nil {
		return model.Member{}, err
	}
	if !model.MemberStatus(status) {
		return model.Member{}, model.Fail(model.Invalid, `Incorrect request: Status must be "pending", "accepted" or "rejected".`)
	}
	m, err := repo.UpdateMember(ctx, id, memberID, status, s.now(), func(image model.Image, m model.Member) error {
		if err := s.visible(ctx, who, image); err != nil {
			return err
		}
		if overridden() {
			target := visibilityTarget(who, image)
			target["member_id"] = memberID
			return Permit(who, "modify_member", target, nil)
		}
		if !who.HasRole("admin") && (who.ProjectID != memberID || !who.HasRole("member")) {
			return model.Fail(model.Forbidden, "Only the member project can change its membership status.")
		}
		return nil
	})
	if err != nil {
		return model.Member{}, memberError(err, id)
	}
	s.notify("image.member.update", notify.MemberPayload(m, false, time.Time{}))
	return m, nil
}

func (s *Service) DeleteMember(ctx context.Context, who auth.Identity, imageID, memberID string) error {
	repo, id, err := s.memberStart(who, imageID)
	if err != nil {
		return err
	}
	now := s.now()
	m, err := repo.DeleteMember(ctx, id, memberID, now, func(image model.Image, _ model.Member) error {
		if err := s.visible(ctx, who, image); err != nil {
			return err
		}
		return ownerPolicy(who, image, "delete_member")
	})
	if err != nil {
		return memberError(err, id)
	}
	s.notify("image.member.delete", notify.MemberPayload(m, true, now))
	return nil
}

// AddTag and RemoveTag are the original's /v2/images/{id}/tags/{tag}.
func (s *Service) AddTag(ctx context.Context, who auth.Identity, imageID, tag string) error {
	if !utf8.ValidString(tag) || utf8.RuneCountInString(tag) > 255 {
		return model.Fail(model.Invalid, "Tag exceeds 255 characters.")
	}
	return s.changeTags(ctx, who, imageID, func(image *model.Image) error {
		for _, t := range image.Tags {
			if t == tag {
				return nil
			}
		}
		if model.Over(len(image.Tags)+1, model.Quotas.Tags) {
			return model.Fail(model.LimitExceeded, "Image tag quota exceeded.")
		}
		image.Tags = append(image.Tags, tag)
		return nil
	})
}

func (s *Service) RemoveTag(ctx context.Context, who auth.Identity, imageID, tag string) error {
	return s.changeTags(ctx, who, imageID, func(image *model.Image) error {
		for n, t := range image.Tags {
			if t == tag {
				image.Tags = append(image.Tags[:n:n], image.Tags[n+1:]...)
				return nil
			}
		}
		return model.Fail(model.NotFound, "The resource could not be found.")
	})
}

func (s *Service) changeTags(ctx context.Context, who auth.Identity, imageID string, change func(*model.Image) error) error {
	return s.updateImage(ctx, who, imageID, func(image model.Image) (model.Image, error) {
		if err := s.writable(ctx, who, image); err != nil {
			return model.Image{}, err
		}
		image.Tags = append([]string(nil), image.Tags...)
		if err := change(&image); err != nil {
			return model.Image{}, err
		}
		image.UpdatedAt = s.now().UTC()
		return image, nil
	})
}

// Deactivate and Reactivate are the original's image actions.
func (s *Service) Deactivate(ctx context.Context, who auth.Identity, imageID string) error {
	return s.action(ctx, who, imageID, "deactivate", "active", "deactivated")
}

func (s *Service) Reactivate(ctx context.Context, who auth.Identity, imageID string) error {
	return s.action(ctx, who, imageID, "reactivate", "deactivated", "active")
}

func (s *Service) action(ctx context.Context, who auth.Identity, imageID, name, from, to string) error {
	return s.updateImage(ctx, who, imageID, func(image model.Image) (model.Image, error) {
		if err := s.visible(ctx, who, image); err != nil {
			return model.Image{}, err
		}
		// The original's default: admin, or a member of the owner project.
		if overridden() {
			if err := s.permitImage(ctx, who, name, image, nil); err != nil {
				return model.Image{}, err
			}
		} else if !who.HasRole("admin") && (!who.HasRole("member") || image.Owner != who.ProjectID) {
			return model.Image{}, model.Fail(model.Forbidden, "You are not authorized to complete "+name+" action.")
		}
		switch image.Status {
		case to: // already there: no change, as the original
			return image, nil
		case from:
			image.Status, image.UpdatedAt = to, s.now().UTC()
			return image, nil
		}
		return model.Image{}, model.Fail(model.Forbidden, "Not allowed to "+name+" image in status '"+image.Status+"'")
	})
}

func (s *Service) updateImage(ctx context.Context, who auth.Identity, imageID string, change func(model.Image) (model.Image, error)) error {
	if err := who.Check(s.now()); err != nil {
		return err
	}
	id, err := model.CanonicalID(imageID)
	if err != nil {
		return err
	}
	image, err := s.repo.Update(ctx, id, change)
	if err != nil {
		return mutationError(err, id)
	}
	s.notify("image.update", notify.ImagePayload(image))
	return nil
}

// writable is the original's modify_image for one locked image: 404 when
// it is not visible, 403 when visible but not the caller's.
func (s *Service) writable(ctx context.Context, who auth.Identity, image model.Image, action ...string) error {
	if err := s.visible(ctx, who, image); err != nil {
		return err
	}
	if overridden() {
		name := "modify_image"
		if len(action) > 0 {
			name = action[0]
		}
		return s.permitImage(ctx, who, name, image, nil)
	}
	if !who.HasRole("member") && !who.HasRole("admin") {
		return model.Fail(model.Forbidden, "Modifying images requires member or admin role.")
	}
	if !who.HasRole("admin") && image.Owner != who.ProjectID {
		return model.Fail(model.Forbidden, "You are not authorized to complete modify_image action.")
	}
	return nil
}
