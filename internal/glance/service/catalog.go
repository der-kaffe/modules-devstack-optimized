package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
)

func (s *Service) List(ctx context.Context, who auth.Identity, options model.ListOptions) ([]model.Image, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, err
	}
	if err := Permit(who, "get_images", map[string]any{"project_id": who.ProjectID}, nil); err != nil {
		return nil, err
	}
	if !overridden() && !who.HasRole("reader") && !who.HasRole("admin") {
		return nil, model.Fail(model.Forbidden, "Listing images requires reader or admin role.")
	}
	options.ProjectID, options.Admin = who.ProjectID, who.HasRole("admin")
	options, err := options.Validate()
	if err != nil {
		return nil, err
	}
	images, err := s.repo.List(ctx, options)
	if Protections != nil {
		for n := range images {
			images[n] = readable(who, images[n])
		}
	}
	return images, err
}

// writePolicy is the default modify/upload/delete rule; with a policy file
// the action's rule decides once the image is visible.
func writePolicy(who auth.Identity, image model.Image, action ...string) error {
	if overridden() && len(action) > 0 {
		if !who.HasRole("admin") && image.Owner != who.ProjectID && image.Visibility != "public" && image.Visibility != "community" && image.Visibility != "shared" {
			return notFound(image.ID)
		}
		target := visibilityTarget(who, image)
		target["id"] = image.ID
		return Permit(who, action[0], target, nil)
	}
	if !who.HasRole("member") && !who.HasRole("admin") {
		return model.Fail(model.Forbidden, "Modifying images requires member or admin role.")
	}
	if !who.HasRole("admin") && image.Owner != who.ProjectID {
		if image.Visibility != "public" && image.Visibility != "community" {
			return notFound(image.ID)
		}
		return model.Fail(model.Forbidden, "Only the owner or an administrator may modify this image.")
	}
	return nil
}

func mutationError(err error, id string) error {
	if errors.Is(err, repository.ErrNotFound) {
		return notFound(id)
	}
	if errors.Is(err, repository.ErrConflict) {
		return model.Fail(model.Conflict, "Concurrent metadata update; retry the request.")
	}
	return err
}

func (s *Service) Patch(ctx context.Context, who auth.Identity, id string, ops []model.PatchOperation) (model.Image, error) {
	if err := who.Check(s.now()); err != nil {
		return model.Image{}, err
	}
	if !overridden() && !who.HasRole("member") && !who.HasRole("admin") {
		return model.Image{}, model.Fail(model.Forbidden, "Modifying images requires member or admin role.")
	}
	id, err := model.CanonicalID(id)
	if err != nil {
		return model.Image{}, err
	}
	image, err := s.repo.Update(ctx, id, func(current model.Image) (model.Image, error) {
		if err := s.writable(ctx, who, current); err != nil {
			return model.Image{}, err
		}
		for _, op := range ops {
			var visibility string
			if !overridden() && op.Key == "visibility" && op.Op != "remove" && !who.HasRole("admin") && json.Unmarshal(op.Value, &visibility) == nil && visibility == "public" {
				return model.Image{}, model.Fail(model.Forbidden, "Publishing images requires admin role.")
			}
		}
		changed, err := model.ApplyPatch(current, ops, s.now())
		if err != nil {
			return model.Image{}, err
		}
		if err = protectedChange(who, current.Properties, changed.Properties); err != nil {
			return model.Image{}, err
		}
		// Check the decoded value, not just its possible JSON spellings/escapes.
		if overridden() && changed.Visibility != current.Visibility {
			if action := map[string]string{"public": "publicize_image", "community": "communitize_image"}[changed.Visibility]; action != "" {
				if err := Permit(who, action, visibilityTarget(who, changed), nil); err != nil {
					return model.Image{}, err
				}
			}
		} else if changed.Visibility == "public" && current.Visibility != "public" && !who.HasRole("admin") {
			return model.Image{}, model.Fail(model.Forbidden, "Publishing images requires admin role.")
		}
		return changed, nil
	})
	if err == nil {
		s.notify("image.update", notify.ImagePayload(image))
	}
	return readable(who, image), mutationError(err, id)
}

func (s *Service) Delete(ctx context.Context, who auth.Identity, id string) error {
	if err := who.Check(s.now()); err != nil {
		return err
	}
	if !overridden() && !who.HasRole("member") && !who.HasRole("admin") {
		return model.Fail(model.Forbidden, "Deleting images requires member or admin role.")
	}
	id, err := model.CanonicalID(id)
	if err != nil {
		return err
	}
	var removed model.Image
	err = s.repo.Delete(ctx, id, s.now(), func(current model.Image) error {
		removed = current
		if err := s.writable(ctx, who, current, "delete_image"); err != nil {
			return err
		}
		if current.Protected {
			return model.Fail(model.Forbidden, "Protected images cannot be deleted.")
		}
		if current.Status != "queued" || current.Size != nil || current.VirtualSize != nil || current.Checksum != nil || current.HashAlgorithm != nil || current.HashValue != nil {
			return model.Fail(model.Conflict, "Only metadata without binary content can be deleted in B20.")
		}
		return nil
	})
	if err == nil {
		s.notify("image.delete", notify.DeletePayload(removed, s.now()))
	}
	return mutationError(err, id)
}
