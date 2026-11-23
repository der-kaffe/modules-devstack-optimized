package service

import (
	"context"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/quota"
)

// QuotaEnforcer is the original's use_keystone_limits (internal/glance/quota).
type QuotaEnforcer interface {
	EnforceImageCount(ctx context.Context, project string) error
	EnforceUpload(ctx context.Context, project string) error
	EnforceStage(ctx context.Context, project string) error
	Usage(ctx context.Context, project string) (map[string]quota.Limit, error)
}

// SetQuota enables Keystone limits; nil (the default) disables them, as
// use_keystone_limits = False.
func (s *Service) SetQuota(q QuotaEnforcer) { s.quota = q }

// SetQuota enables the upload's limits (image_size_total, image_count_uploading).
func (u *UploadCoordinator) SetQuota(q QuotaEnforcer) { u.quota = q }

// Usage is /v2/info/usage: empty without Keystone limits, as the original.
func (s *Service) Usage(ctx context.Context, who auth.Identity) (map[string]quota.Limit, error) {
	if err := who.Check(s.now()); err != nil {
		return nil, err
	}
	if s.quota == nil {
		return map[string]quota.Limit{}, nil
	}
	return s.quota.Usage(ctx, who.ProjectID)
}
