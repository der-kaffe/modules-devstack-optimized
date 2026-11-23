package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

type usageFake struct{ u model.Usage }

func (f usageFake) ProjectUsage(context.Context, string) (model.Usage, error) { return f.u, nil }

// keystoneFake answers like Keystone with the limits DevStack registers.
func keystoneFake(t *testing.T, projectLimits map[string]int64, tokens *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v3/auth/tokens":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			scope := body["auth"].(map[string]any)["scope"].(map[string]any)
			if scope["system"] == nil {
				t.Error("not a system-scoped token")
			}
			tokens.Add(1)
			w.Header().Set("X-Subject-Token", "limit-token")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"token":{"expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}}`))
		case r.Header.Get("X-Auth-Token") != "limit-token":
			w.WriteHeader(401)
		case r.URL.Path == "/v3/endpoints/ep1":
			_, _ = w.Write([]byte(`{"endpoint":{"service_id":"svc","region_id":"RegionOne"}}`))
		case r.URL.Path == "/v3/registered_limits":
			if r.URL.Query().Get("service_id") != "svc" || r.URL.Query().Get("region_id") != "RegionOne" {
				t.Error("registered limits not filtered by the endpoint", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"registered_limits":[{"resource_name":"image_size_total","default_limit":2000},{"resource_name":"image_stage_total","default_limit":2000},{"resource_name":"image_count_total","default_limit":100},{"resource_name":"image_count_uploading","default_limit":100}]}`))
		case r.URL.Path == "/v3/limits":
			out := []map[string]any{}
			for name, v := range projectLimits {
				out = append(out, map[string]any{"resource_name": name, "resource_limit": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"limits": out})
		default:
			w.WriteHeader(404)
		}
	}))
}

func enforcer(t *testing.T, url string, u model.Usage) *Enforcer {
	e, err := New(config.KeystoneLimits{Enabled: true, URL: url + "/v3", EndpointID: "ep1", Username: "glance", Password: "x", UserDomainName: "Default", SystemScope: "all"}, http.DefaultClient, usageFake{u})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestKeystoneLimitsLikeOsloLimit(t *testing.T) {
	var tokens atomic.Int32
	ks := keystoneFake(t, map[string]int64{"image_count_total": 3}, &tokens)
	defer ks.Close()
	ctx := context.Background()
	e := enforcer(t, ks.URL, model.Usage{CountTotal: 2, SizeTotal: 2000<<20 + 1, CountUploading: 0})
	if err := e.EnforceImageCount(ctx, "p"); err != nil {
		t.Fatal("third image refused under a project limit of 3", err)
	}
	e = enforcer(t, ks.URL, model.Usage{CountTotal: 3})
	var domain *model.Error
	err := e.EnforceImageCount(ctx, "p")
	if !errors.As(err, &domain) || domain.Kind != model.LimitExceeded || !strings.Contains(domain.Message, "Resource image_count_total is over limit of 3 due to current usage 3 and delta 1") {
		t.Fatal("project limit not enforced", err)
	}
	// 2001 MiB: over the registered 2000 (usage rounds down, as the original).
	e = enforcer(t, ks.URL, model.Usage{SizeTotal: 2001 << 20})
	if err = e.EnforceUpload(ctx, "p"); !errors.As(err, &domain) || !strings.Contains(domain.Message, "image_size_total") {
		t.Fatal("size limit not enforced", err)
	}
	e = enforcer(t, ks.URL, model.Usage{SizeTotal: 2000 << 20, CountUploading: 100})
	if err = e.EnforceUpload(ctx, "p"); err != nil {
		t.Fatal("usage equal to the limit refused with delta 0", err)
	}
	usage, err := e.Usage(ctx, "p")
	if err != nil || usage["image_size_total"] != (Limit{2000, 2000}) || usage["image_count_total"] != (Limit{3, 0}) || len(usage) != 4 {
		t.Fatal(usage, err)
	}
	if n := tokens.Load(); n != 4 { // one per enforcer: each caches its token
		t.Fatal("token not reused", n)
	}
}

func TestKeystoneUnavailable(t *testing.T) {
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer ks.Close()
	if err := enforcer(t, ks.URL, model.Usage{}).EnforceImageCount(context.Background(), "p"); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestUnlimited(t *testing.T) {
	var tokens atomic.Int32
	ks := keystoneFake(t, map[string]int64{"image_count_total": -1}, &tokens)
	defer ks.Close()
	if err := enforcer(t, ks.URL, model.Usage{CountTotal: 1 << 40}).EnforceImageCount(context.Background(), "p"); err != nil {
		t.Fatal("-1 is unlimited", err)
	}
}

// countingUsage counts the catalog's usage queries.
type countingUsage struct {
	u model.Usage
	n *atomic.Int32
}

func (c countingUsage) ProjectUsage(context.Context, string) (model.Usage, error) {
	c.n.Add(1)
	return c.u, nil
}

// requests counts the limit reads Keystone answers.
func countKeystone(ks *httptest.Server) (*http.Client, *atomic.Int32) {
	var n atomic.Int32
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v3/limits" || r.URL.Path == "/v3/registered_limits" {
			n.Add(1)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	return client, &n
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLimitsAreReusedForTheTTL(t *testing.T) {
	var tokens atomic.Int32
	ks := keystoneFake(t, map[string]int64{"image_count_total": 3}, &tokens)
	defer ks.Close()
	client, reads := countKeystone(ks)
	cfg := config.KeystoneLimits{Enabled: true, URL: ks.URL + "/v3", EndpointID: "ep1", Username: "glance", Password: "x", UserDomainName: "Default", SystemScope: "all", CacheTTL: 80 * time.Millisecond}
	var usages atomic.Int32
	e, err := New(cfg, client, countingUsage{model.Usage{CountTotal: 1}, &usages})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 5 {
		if err = e.EnforceImageCount(ctx, "p"); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 2 { // registered + project limits, once
		t.Fatal("Keystone limits read", reads.Load(), "times for 5 creations")
	}
	if usages.Load() != 5 {
		t.Fatal("usage must be read every time", usages.Load())
	}
	time.Sleep(120 * time.Millisecond)
	if err = e.EnforceImageCount(ctx, "p"); err != nil || reads.Load() != 4 {
		t.Fatal("limits not read again after the TTL", reads.Load(), err)
	}
	// A limit past its TTL is enforced; one inside it too.
	e2, _ := New(cfg, client, usageFake{model.Usage{CountTotal: 3}})
	if err = e2.EnforceImageCount(ctx, "p"); err == nil {
		t.Fatal("over the limit accepted")
	}
	if err = e2.EnforceImageCount(ctx, "p"); err == nil {
		t.Fatal("over the limit accepted from the cache")
	}
}

func TestZeroTTLAsksKeystoneEveryTime(t *testing.T) {
	var tokens atomic.Int32
	ks := keystoneFake(t, map[string]int64{}, &tokens)
	defer ks.Close()
	client, reads := countKeystone(ks)
	cfg := config.KeystoneLimits{Enabled: true, URL: ks.URL + "/v3", EndpointID: "ep1", Username: "glance", Password: "x", UserDomainName: "Default", SystemScope: "all"}
	e, _ := New(cfg, client, usageFake{})
	for range 3 {
		if err := e.EnforceImageCount(context.Background(), "p"); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 6 {
		t.Fatal("without a TTL every enforcement reads Keystone", reads.Load())
	}
}

func TestUnlimitedSkipsTheUsageQueryOnceCached(t *testing.T) {
	var tokens atomic.Int32
	ks := keystoneFake(t, map[string]int64{"image_count_total": -1}, &tokens)
	defer ks.Close()
	cfg := config.KeystoneLimits{Enabled: true, URL: ks.URL + "/v3", EndpointID: "ep1", Username: "glance", Password: "x", UserDomainName: "Default", SystemScope: "all", CacheTTL: time.Minute}
	var usages atomic.Int32
	e, _ := New(cfg, http.DefaultClient, countingUsage{model.Usage{CountTotal: 1 << 40}, &usages})
	for range 4 {
		if err := e.EnforceImageCount(context.Background(), "p"); err != nil {
			t.Fatal("-1 is unlimited", err)
		}
	}
	if usages.Load() != 1 { // the first call asks Keystone and the catalog together
		t.Fatal("usage queried", usages.Load(), "times for an unlimited resource")
	}
}
