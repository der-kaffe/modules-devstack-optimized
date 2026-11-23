package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/api"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/metadefs"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/protect"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/quota"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/policy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("glance stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) (result error) {
	started := time.Now()
	// install-metadefs DIR: the original's etc/metadefs files, which DevStack
	// copies to metadata_source_path for glance-manage db load_metadefs.
	if len(args) == 2 && args[0] == "install-metadefs" {
		n, err := metadefs.Install(args[1])
		if err != nil {
			return errors.New("cannot install the metadata definition files")
		}
		logger.Info("metadata definition files installed", "dir", args[1], "files", n)
		return nil
	}
	if len(args) != 1 || (args[0] != "serve" && args[0] != "migrate" && args[0] != "store-scope" && args[0] != "store-init") {
		return errors.New("usage: glance serve | migrate | store-scope | store-init | install-metadefs DIR")
	}
	mode := args[0]
	c, err := config.Load(os.Getenv, mode)
	if err != nil {
		return err
	}
	// The original's own MySQL account, on its server, database glance_go_shared.
	db, err := mysql.Open(c.DatabaseDSN)
	if err != nil {
		return err
	}
	defer func() {
		if db.Close() != nil {
			result = errors.Join(result, errors.New("database pool close failed"))
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// As oslo.db at startup: max_retries attempts (-1: forever), retry_interval
	// apart, so a service started before MySQL (a reboot) waits for it.
	for attempt := 0; ; attempt++ {
		ping, cancelPing := context.WithTimeout(ctx, 10*time.Second)
		err = db.PingContext(ping)
		cancelPing()
		if err == nil {
			break
		}
		if ctx.Err() != nil || c.DBMaxRetries >= 0 && attempt >= c.DBMaxRetries {
			return errors.New("database connection failed")
		}
		logger.Warn("database not reachable; retrying", "attempt", attempt+1, "retry_interval_s", c.DBRetryInterval.Seconds())
		select {
		case <-ctx.Done():
			return errors.New("database connection failed")
		case <-time.After(c.DBRetryInterval):
		}
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if mode == "migrate" {
		if err = mysql.Migrate(startup, db); err != nil {
			return err
		}
		logger.Info("transfer bookkeeping migration verified")
		return nil
	}
	checks := startupChecks{ping: db.PingContext,
		schema: func(ctx context.Context) error { return mysql.VerifySchema(ctx, db) },
		scope:  func(ctx context.Context) (string, error) { return databaseScope(ctx, db) }}
	if err = verifyBackend(startup, checks); err != nil {
		return err
	}
	if mode == "store-scope" || mode == "store-init" {
		scope, err := checks.scope(startup)
		if err != nil {
			return err
		}
		if mode == "store-scope" {
			_, err = fmt.Fprintln(os.Stdout, scope)
			return err
		}
		if scope != c.Binary.Scope {
			return errors.New("SQL/store scope mismatch")
		}
		if startup.Err() != nil {
			return errors.New("store initialization cancelled")
		}
		if err = initStore(c.Binary, scope); err != nil {
			return errors.New("store initialization failed; inspect partial setup without automatic repair")
		}
		logger.Info("store explicitly initialized")
		return nil
	}
	r, err := mysql.New(db, c.DependencyTimeout, c.OfficialCatalog, c.Binary.OfficialStoreDir)
	if err != nil {
		return err
	}
	if err = r.VerifyOfficialCatalog(startup); err != nil {
		return err
	}
	adopted, err := r.AdoptOriginalUploads(startup)
	if err != nil {
		return errors.New("cannot adopt the original's uploads")
	}
	logger.Info("serving the official catalog", "database", c.OfficialCatalog, "store", c.Binary.OfficialStoreDir, "adopted_uploads", adopted)
	if err = r.PrepareStatements(startup); err != nil {
		// Correctness does not depend on it: every query still runs unprepared.
		logger.Warn("hot statements not prepared; continuing without reuse")
	}
	notifier, closeNotifier, err := notifierFor(c.Notifications, logger)
	if err != nil {
		return err
	}
	defer func() { // after HTTP and transfers stopped: deliver what is queued
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if closeNotifier(drain) != nil {
			logger.Warn("notifications not fully delivered before exit")
		}
	}()
	var binary *binaryRuntime
	var options []api.Option
	if c.Binary.Enabled {
		// Account/schema checks have their own 30s budget; recovery has its
		// configured finite budget and may hash large confirmed objects.
		binary, err = prepareBinary(ctx, c, r, checks.scope, logger)
		if err != nil {
			return err
		}
		defer func() {
			if binary.close() != nil {
				result = errors.Join(result, errors.New("store close failed"))
			}
		}()
		binary.owner.SetNotifier(notifier)
		options = append(options, api.WithBinaryTransfers(binary.owner))
		if len(c.Backends) > 0 {
			var list []api.StoreInfo
			for _, b := range c.Backends {
				list = append(list, api.StoreInfo{ID: b.Name, Type: b.Type, Description: b.Description, DataDir: b.Dir, Weight: b.Weight, Default: b.Name == c.DefaultBackend})
			}
			options = append(options, api.WithStores(list, binary.owner))
		}
	}
	// Reuse Keystone connections under concurrency (net/http keeps only 2 idle
	// per host by default; keystoneauth's requests pool keeps 10).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	client := &http.Client{Timeout: c.DependencyTimeout, Transport: transport}
	authenticator := auth.NewKeystone(c.KeystoneURL, client, c.Credential, auth.WithTokenCache(c.TokenCacheTime, 10000))
	model.Quotas.Members, model.Quotas.Properties, model.Quotas.Tags = c.MemberQuota, c.PropertyQuota, c.TagQuota
	overrides, err := policy.LoadFile(c.PolicyFile)
	if err != nil {
		return errors.New("cannot read the policy file")
	}
	if service.Policy, err = policy.New(overrides); err != nil {
		return err
	}
	if len(overrides) > 0 {
		logger.Info("policy file loaded", "file", c.PolicyFile, "rules", len(overrides))
	}
	if c.PropertyProtectionFile != "" {
		// As the original: a configured file that cannot be read stops the service.
		if service.Protections, err = protect.Load(c.PropertyProtectionFile, c.PropertyProtectionFormat, service.Policy); err != nil {
			return err
		}
		logger.Info("property protections loaded", "file", c.PropertyProtectionFile, "format", c.PropertyProtectionFormat)
	}
	catalog := service.New(r, time.Now)
	catalog.SetNotifier(notifier)
	if c.Limits.Enabled {
		// The original's use_keystone_limits: [oslo_limit] user and endpoint.
		limits, err := quota.New(c.Limits, client, r)
		if err != nil {
			return err
		}
		catalog.SetQuota(limits)
		if binary != nil {
			binary.owner.SetQuota(limits)
		}
		logger.Info("keystone limits enabled", "endpoint_id", c.Limits.EndpointID)
	}
	if binary != nil {
		// Interoperable import and tasks; recovered after the uploads were.
		importer, err := service.NewImporter(c.Import, catalog, binary.owner, &http.Client{Transport: transport})
		if err != nil {
			return err
		}
		importer.SetNotifier(notifier)
		defer importer.Close() // before the store closes (deferred earlier)
		recovering, cancelRecovery := context.WithTimeout(ctx, c.Binary.RecoveryTimeout)
		recovered, err := importer.Recover(recovering)
		cancelRecovery()
		if err != nil {
			return errors.New("import task recovery failed")
		}
		logger.Info("import methods enabled", "methods", importer.Methods(), "staging", c.Import.StagingDir, "recovered_tasks", recovered)
		options = append(options, api.WithImports(importer), api.WithLocationViews(c.ShowDirectURL, c.ShowMultipleLocations))
	}
	if binary != nil && c.Cache.Enabled {
		// The original's image cache (centralized_db) and its prefetcher.
		cacheCtx, cancelCache := context.WithTimeout(ctx, c.DependencyTimeout)
		cache, err := service.NewImageCache(cacheCtx, c.Cache, r, binary.owner, logger)
		cancelCache()
		if err != nil {
			return errors.New("image cache unavailable")
		}
		defer cache.Close() // before the store closes
		binary.owner.SetCache(cache)
		cache.Prefetch()
		logger.Info("image cache enabled", "dir", c.Cache.Dir, "prefetch_s", c.Cache.PrefetchInterval.Seconds())
		options = append(options, api.WithCache(cache))
	}
	handler, err := api.New(catalog, authenticator, c.PublicURL, c.KeystoneURL, c.MaxBodyBytes, logger, options...)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.New("startup cancelled before listener")
	}
	ln, err := net.Listen("tcp", c.ListenAddress)
	if err != nil {
		return errors.New("HTTP listener failed")
	}
	defer ln.Close()
	logger.Info("effective settings", effectiveSettings(c)...)
	logger.Info("image API listening", "address", c.ListenAddress, "binary_enabled", c.Binary.Enabled, "startup_ms", time.Since(started).Milliseconds())
	var owner *service.UploadCoordinator
	if binary != nil {
		owner = binary.owner
	}
	return serveHTTP(ctx, ln, c, handler, owner, logger)
}
