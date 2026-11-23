// Package quota enforces the original's Keystone unified limits
// (use_keystone_limits, glance/quota/keystone.py over oslo.limit): the
// registered and project limits of the Glance endpoint, in Keystone, against
// the project's usage, computed from the catalog.
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// The original's resources (QUOTA_IMAGE_*); size limits are MiB.
const (
	ImageSizeTotal      = "image_size_total"
	ImageStageTotal     = "image_stage_total"
	ImageCountTotal     = "image_count_total"
	ImageCountUploading = "image_count_uploading"
)

var Resources = []string{ImageSizeTotal, ImageStageTotal, ImageCountTotal, ImageCountUploading}

// UsageReader computes a project's usage (the MySQL repository).
type UsageReader interface {
	ProjectUsage(ctx context.Context, project string) (model.Usage, error)
}

// Limit is one resource of /v2/info/usage.
type Limit struct {
	Limit int64 `json:"limit"`
	Usage int64 `json:"usage"`
}

type Enforcer struct {
	cfg    config.KeystoneLimits
	client *http.Client
	usage  UsageReader

	mu              sync.Mutex
	token           string
	expires         time.Time
	service, region string // of the endpoint; fixed, looked up once
	cache           map[string]cachedLimits
}

// cachedLimits are a project's limits as Keystone gave them, reused until
// the TTL ([oslo_limit] cache, GLANCE_GO_LIMITS_CACHE_SECONDS). Read-only.
type cachedLimits struct {
	values map[string]int64
	until  time.Time
}

const maxCachedProjects = 1024

func (e *Enforcer) cachedLimits(project string) (map[string]int64, bool) {
	if e.cfg.CacheTTL <= 0 {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.cache[project]
	if !ok || time.Now().After(c.until) {
		return nil, false
	}
	return c.values, true
}

func (e *Enforcer) storeLimits(project string, values map[string]int64) {
	if e.cfg.CacheTTL <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cache == nil || len(e.cache) >= maxCachedProjects {
		e.cache = map[string]cachedLimits{}
	}
	e.cache[project] = cachedLimits{values: values, until: time.Now().Add(e.cfg.CacheTTL)}
}

func New(cfg config.KeystoneLimits, client *http.Client, usage UsageReader) (*Enforcer, error) {
	if !cfg.Enabled || cfg.EndpointID == "" || client == nil || usage == nil {
		return nil, errors.New("invalid Keystone limits configuration")
	}
	return &Enforcer{cfg: cfg, client: client, usage: usage}, nil
}

// mib: bytes // units.Mi, as the original rounds usage down.
func mib(bytes int64) int64 { return bytes >> 20 }

func usageOf(u model.Usage) map[string]int64 {
	return map[string]int64{ImageSizeTotal: mib(u.SizeTotal), ImageStageTotal: mib(u.StageTotal), ImageCountTotal: u.CountTotal, ImageCountUploading: u.CountUploading}
}

// EnforceImageCount is enforce_image_count_total: creating one more image.
func (e *Enforcer) EnforceImageCount(ctx context.Context, project string) error {
	return e.enforce(ctx, project, map[string]int64{ImageCountTotal: 1})
}

// EnforceUpload is the original's check before an upload:
// enforce_image_size_total and enforce_image_count_uploading (delta 0).
func (e *Enforcer) EnforceUpload(ctx context.Context, project string) error {
	return e.enforce(ctx, project, map[string]int64{ImageSizeTotal: 0, ImageCountUploading: 0})
}

// EnforceStage is enforce_image_staging_total (stage and import).
func (e *Enforcer) EnforceStage(ctx context.Context, project string) error {
	return e.enforce(ctx, project, map[string]int64{ImageStageTotal: 0, ImageCountUploading: 0})
}

func (e *Enforcer) enforce(ctx context.Context, project string, deltas map[string]int64) error {
	names := make([]string, 0, len(deltas))
	for name := range deltas {
		names = append(names, name)
	}
	sort.Strings(names)
	// With the limits at hand (cache), a resource without a limit needs no
	// usage query; the first request, or an expired entry, asks Keystone and
	// the catalog at once, as before.
	var limits, usage map[string]int64
	if cached, ok := e.cachedLimits(project); ok {
		limits = cached
		limited := false
		for _, name := range names {
			limited = limited || limits[name] >= 0
		}
		if !limited {
			return nil
		}
		u, err := e.usage.ProjectUsage(ctx, project)
		if err != nil {
			return err
		}
		usage = usageOf(u)
	} else {
		var err error
		if limits, usage, err = e.read(ctx, project); err != nil {
			return err
		}
	}
	var over []string
	for _, name := range names {
		if limits[name] >= 0 && usage[name]+deltas[name] > limits[name] {
			over = append(over, fmt.Sprintf("Resource %s is over limit of %d due to current usage %d and delta %d", name, limits[name], usage[name], deltas[name]))
		}
	}
	if len(over) > 0 {
		return model.Fail(model.LimitExceeded, "Project "+project+" is over a limit for ["+strings.Join(over, ", ")+"]")
	}
	return nil
}

// Usage is /v2/info/usage.
func (e *Enforcer) Usage(ctx context.Context, project string) (map[string]Limit, error) {
	limits, usage, err := e.read(ctx, project)
	if err != nil {
		return nil, err
	}
	out := map[string]Limit{}
	for _, name := range Resources {
		out[name] = Limit{Limit: limits[name], Usage: usage[name]}
	}
	return out, nil
}

// read gets limits (Keystone) and usage (catalog) concurrently.
func (e *Enforcer) read(ctx context.Context, project string) (map[string]int64, map[string]int64, error) {
	type result struct {
		u   model.Usage
		err error
	}
	done := make(chan result, 1)
	go func() {
		u, err := e.usage.ProjectUsage(ctx, project)
		done <- result{u, err}
	}()
	limits, err := e.limits(ctx, project)
	if err == nil {
		e.storeLimits(project, limits)
	}
	r := <-done
	if err != nil {
		return nil, nil, err
	}
	if r.err != nil {
		return nil, nil, r.err
	}
	return limits, usageOf(r.u), nil
}

// limits: a project limit overrides the registered (default) limit; a
// resource with neither is 0, as oslo.limit.
func (e *Enforcer) limits(ctx context.Context, project string) (map[string]int64, error) {
	service, region, err := e.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"service_id": {service}}
	if region != "" {
		q.Set("region_id", region)
	}
	var registered struct {
		Limits []struct {
			Name  string `json:"resource_name"`
			Limit int64  `json:"default_limit"`
		} `json:"registered_limits"`
	}
	var projects struct {
		Limits []struct {
			Name  string `json:"resource_name"`
			Limit int64  `json:"resource_limit"`
		} `json:"limits"`
	}
	pq := url.Values{"service_id": {service}, "project_id": {project}}
	if region != "" {
		pq.Set("region_id", region)
	}
	errs := make(chan error, 1)
	go func() { errs <- e.get(ctx, "/registered_limits?"+q.Encode(), &registered) }()
	err = e.get(ctx, "/limits?"+pq.Encode(), &projects)
	if err = errors.Join(err, <-errs); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, name := range Resources {
		out[name] = 0
	}
	for _, l := range registered.Limits {
		out[l.Name] = l.Limit
	}
	for _, l := range projects.Limits {
		out[l.Name] = l.Limit
	}
	return out, nil
}

func (e *Enforcer) endpoint(ctx context.Context) (string, string, error) {
	e.mu.Lock()
	service, region := e.service, e.region
	e.mu.Unlock()
	if service != "" {
		return service, region, nil
	}
	var out struct {
		Endpoint struct {
			Service string `json:"service_id"`
			Region  string `json:"region_id"`
		} `json:"endpoint"`
	}
	if err := e.get(ctx, "/endpoints/"+url.PathEscape(e.cfg.EndpointID), &out); err != nil {
		return "", "", err
	}
	if out.Endpoint.Service == "" {
		return "", "", auth.ErrUnavailable
	}
	if e.cfg.Region != "" {
		out.Endpoint.Region = e.cfg.Region
	}
	e.mu.Lock()
	e.service, e.region = out.Endpoint.Service, out.Endpoint.Region
	e.mu.Unlock()
	return out.Endpoint.Service, out.Endpoint.Region, nil
}

func (e *Enforcer) get(ctx context.Context, path string, dst any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := e.serviceToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.URL+path, nil)
		if err != nil {
			return auth.ErrUnavailable
		}
		req.Header.Set("X-Auth-Token", token)
		req.Header.Set("Accept", "application/json")
		res, err := e.client.Do(req)
		if err != nil {
			return errors.Join(auth.ErrUnavailable, ctx.Err())
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue // expired or revoked: one new token
		}
		if err != nil || res.StatusCode != http.StatusOK || json.Unmarshal(body, dst) != nil {
			return auth.ErrUnavailable
		}
		return nil
	}
	return auth.ErrUnavailable
}

// serviceToken: the [oslo_limit] user's token, reused until 30 s before it
// expires (keystoneauth's session does the same).
func (e *Enforcer) serviceToken(ctx context.Context, renew bool) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !renew && e.token != "" && time.Now().Add(30*time.Second).Before(e.expires) {
		return e.token, nil
	}
	scope := map[string]any{"system": map[string]bool{"all": true}}
	if e.cfg.SystemScope == "" {
		scope = map[string]any{"project": map[string]any{"name": e.cfg.ProjectName, "domain": map[string]string{"name": e.cfg.ProjectDomainName}}}
	}
	body, _ := json.Marshal(map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{"user": map[string]any{
			"name": e.cfg.Username, "password": e.cfg.Password, "domain": map[string]string{"name": e.cfg.UserDomainName}}}},
		"scope": scope}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return "", auth.ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(req)
	if err != nil {
		return "", errors.Join(auth.ErrUnavailable, ctx.Err())
	}
	defer res.Body.Close()
	var out struct {
		Token struct {
			Expires time.Time `json:"expires_at"`
		} `json:"token"`
	}
	token := res.Header.Get("X-Subject-Token")
	if res.StatusCode != http.StatusCreated || token == "" || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out) != nil {
		return "", auth.ErrUnavailable
	}
	e.token, e.expires = token, out.Token.Expires
	return token, nil
}
