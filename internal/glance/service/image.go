package service

import (
	"context"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

type Service struct {
	repo     repository.Repository
	now      func() time.Time
	notifier notify.Notifier
	quota    QuotaEnforcer
}

// SetNotifier makes the catalog emit image.create/update/delete as official
// Glance does after each successful repository change. nil means noop.
func (s *Service) SetNotifier(n notify.Notifier) { s.notifier = notify.OrNoop(n) }

func (s *Service) notify(eventType string, payload any) {
	if s.notifier != nil {
		s.notifier.Notify(eventType, notify.Info, payload)
	}
}

func New(repo repository.Repository, now func() time.Time) *Service {
	return &Service{repo: repo, now: now}
}

func (s *Service) Create(ctx context.Context, identity auth.Identity, input model.CreateInput) (model.Image, error) {
	if err := identity.Check(s.now()); err != nil {
		return model.Image{}, err
	}
	if !overridden() && !identity.HasRole("admin") && !identity.HasRole("member") {
		return model.Image{}, model.Fail(model.Forbidden, "Creating images requires member or admin role.")
	}
	image, err := model.New(input, identity.ProjectID, s.now())
	if err != nil {
		return model.Image{}, err
	}
	if err = protectedChange(identity, map[string]string{}, image.Properties); err != nil {
		return model.Image{}, err
	}
	if overridden() {
		if err = Permit(identity, "add_image", visibilityTarget(identity, image), nil); err != nil {
			return model.Image{}, err
		}
		if action := map[string]string{"public": "publicize_image", "community": "communitize_image"}[image.Visibility]; action != "" {
			if err = Permit(identity, action, visibilityTarget(identity, image), nil); err != nil {
				return model.Image{}, err
			}
		}
	} else if image.Visibility == "public" && !identity.HasRole("admin") {
		return model.Image{}, model.Fail(model.Forbidden, "Publishing images requires admin role.")
	}
	if s.quota != nil {
		if err = s.quota.EnforceImageCount(ctx, identity.ProjectID); err != nil {
			return model.Image{}, err
		}
	}
	if err = s.repo.Create(ctx, image); errors.Is(err, repository.ErrDuplicate) {
		return model.Image{}, model.Fail(model.Conflict, "An image with this ID already exists.")
	}
	if err != nil {
		return model.Image{}, err
	}
	s.notify("image.create", notify.ImagePayload(image))
	return readable(identity, image), nil
}

func (s *Service) Get(ctx context.Context, identity auth.Identity, value string) (model.Image, error) {
	if err := identity.Check(s.now()); err != nil {
		return model.Image{}, err
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return model.Image{}, err
	}
	if !overridden() && !identity.HasRole("admin") && !identity.HasRole("reader") {
		return model.Image{}, model.Fail(model.Forbidden, "Reading images requires reader or admin role.")
	}
	image, err := s.repo.Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return model.Image{}, notFound(id)
	}
	if err != nil {
		return model.Image{}, err
	}
	if err = s.visible(ctx, identity, image); err != nil {
		return model.Image{}, err
	}
	if err = s.permitImage(ctx, identity, "get_image", image, nil); err != nil {
		return model.Image{}, err
	}
	return readable(identity, image), nil
}

func notFound(id string) error { return model.Fail(model.NotFound, "No image found with ID "+id) }
