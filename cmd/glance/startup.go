package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

type binaryRepository interface {
	repository.Repository
	repository.Transfers
	repository.ImageDeletion
}

// Private startup seams test ordering/failures; production always supplies the
// real MySQL guards and Filesystem. No flag selects a fake repository or auth.
type startupChecks struct {
	ping, schema func(context.Context) error
	scope        func(context.Context) (string, error)
}

func verifyBackend(ctx context.Context, checks startupChecks) error {
	for _, step := range []struct {
		run     func(context.Context) error
		message string
	}{
		{checks.ping, "database connection failed"}, {checks.schema, "schema verification failed; run explicit migrations"},
	} {
		if err := step.run(ctx); err != nil {
			return errors.New(step.message)
		}
	}
	return ctx.Err()
}

type binaryRuntime struct {
	owner *service.UploadCoordinator
	fs    store.Backend
}

func prepareBinary(ctx context.Context, c config.Config, r binaryRepository, scope func(context.Context) (string, error), logger *slog.Logger) (_ *binaryRuntime, result error) {
	started := time.Now()
	identityCtx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
	expected, err := scope(identityCtx)
	cancel()
	if err != nil || expected != c.Binary.Scope {
		return nil, errors.New("SQL/store scope mismatch or identity unavailable")
	}
	fs, err := openStore(c.Binary, expected)
	if err != nil {
		return nil, errors.New("cannot acquire initialized exclusive store")
	}
	if rd, ok := r.(interface{ SetDelayedDelete(bool) }); ok {
		rd.SetDelayedDelete(c.DelayedDelete)
	}
	// Multi-store: the other enabled_backends' directories, in the store and
	// in the repository's locations, before recovery reads them.
	var names []string
	if len(c.Backends) > 0 {
		fsys, ok := fs.(*store.Filesystem)
		if !ok {
			_ = fs.Close()
			return nil, errors.New("multi-store needs the filesystem store")
		}
		dirs := map[string]string{}
		for _, b := range c.Backends {
			dirs[b.Name], names = b.Dir, append(names, b.Name)
			if b.Name != c.DefaultBackend {
				if err = fsys.AddBackend(b.Name, b.Dir); err != nil {
					_ = fs.Close()
					return nil, errors.New("cannot open store " + b.Name)
				}
			}
		}
		if rb, ok := r.(interface {
			SetBackends(string, map[string]string)
		}); ok {
			rb.SetBackends(c.DefaultBackend, dirs)
		}
	}
	owner, err := service.NewUploadCoordinator(r, r, fs, service.UploadLimits{MaxBytes: c.Binary.MaxUploadBytes, MaxConcurrent: c.Binary.MaxConcurrent, MaxDownloadStreams: c.Binary.MaxDownloadStreams, ResolutionTimeout: c.Binary.ResolutionTimeout, RecoveryTimeout: c.Binary.RecoveryTimeout,
		Inspect: true, RequireFormat: c.ImageFormat.RequireMatch, GPTNonfatalCheck: c.ImageFormat.GPTNonfatal,
		StorageQuota: c.UserStorageQuota})
	if err == nil && len(names) > 0 {
		owner.SetBackends(c.DefaultBackend, names)
	}
	if err != nil {
		_ = fs.Close()
		return nil, errors.New("cannot construct transfer owner")
	}
	runtime := &binaryRuntime{owner: owner, fs: fs}
	defer func() {
		if result != nil {
			_ = runtime.close()
		}
	}()
	report, err := owner.RecoverUploads(ctx)
	if err != nil {
		return nil, errors.New("startup recovery failed; evidence retained and listener disabled")
	}
	logger.Info("store recovery completed", "duration_ms", time.Since(started).Milliseconds(), "examined", report.Examined, "cleaned", report.Cleaned, "kept", report.Kept)
	return runtime, nil
}

func (b *binaryRuntime) close() error {
	if b == nil {
		return nil
	}
	b.owner.StopAdmission()
	// No filesystem close/release if anything can still use it. This wait may
	// outlast grace for uninterruptible kernel I/O; that is not a clean shutdown.
	if err := b.owner.WaitIdle(context.Background()); err != nil {
		return err
	}
	if err := b.owner.Close(); err != nil {
		return err
	}
	return b.fs.Close()
}
