package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type compositeToken struct {
	user, project string
	roles         []string
	expires       time.Time
	revoked       bool
	status        int
	extra         map[string]any
}

type validationCall struct {
	token        string
	allowExpired bool
}

type validationTrace struct {
	mu    sync.Mutex
	calls []validationCall
}

func (v *validationTrace) snapshot() []validationCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]validationCall(nil), v.calls...)
}

// This independent protocol fixture models the inspected Keystone expiry and
// revocation contract. 48h is the observed server policy, NOT a Go auth default.
// These tests do not issue, revoke or validate tokens on the real laboratory.
func compositeValidator(t testing.TB, tokens map[string]compositeToken) (*Keystone, *validationTrace) {
	t.Helper()
	trace := &validationTrace{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/auth/tokens" {
			t.Error("unexpected Keystone path")
		}
		if r.Method == http.MethodPost {
			w.Header().Set("X-Subject-Token", "own-validator-token")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(time.Hour), "service-project", "service"))
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("X-Auth-Token") != "own-validator-token" || r.Header.Get("X-Service-Token") != "" || !r.URL.Query().Has("nocatalog") {
			t.Error("credentials or validation protocol changed")
		}
		subject := r.Header.Get("X-Subject-Token")
		allow := r.URL.Query().Get("allow_expired") == "true"
		trace.mu.Lock()
		trace.calls = append(trace.calls, validationCall{subject, allow})
		trace.mu.Unlock()
		f, exists := tokens[subject]
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if !exists || f.revoked || (!time.Now().Before(f.expires) && (!allow || !time.Now().Before(f.expires.Add(48*time.Hour)))) {
			w.WriteHeader(404)
			return
		}
		body := tokenBody(f.expires, f.project, f.roles...)
		claims := body["token"].(map[string]any)
		claims["user"] = map[string]string{"id": f.user}
		for k, v := range f.extra {
			claims[k] = v
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return NewKeystone(s.URL+"/v3", s.Client(), fixtureCredential()), trace
}

func TestCompositeTokenMatrix(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	validUser := compositeToken{user: "original-user", project: "tenant-project", roles: []string{"reader", "member"}, expires: now.Add(time.Hour)}
	validService := compositeToken{user: "nova-user", project: "service-project", roles: []string{"service", "admin"}, expires: now.Add(30 * time.Minute)}
	for _, tc := range []struct {
		name          string
		user, service compositeToken
		userToken     string
		serviceToken  string
		want          error
		validated     int
	}{
		{"valid different projects", validUser, validService, "user", "nova", nil, 2},
		{"valid same project", compositeToken{user: validUser.user, project: "service-project", roles: validUser.roles, expires: validUser.expires}, validService, "user", "nova", nil, 2},
		{"missing user", validUser, validService, "", "nova", ErrInvalidToken, 0},
		{"missing service", validUser, validService, "user", "", ErrInvalidToken, 0},
		{"neither token", validUser, validService, "", "", ErrInvalidToken, 0},
		{"invalid user", validUser, validService, "unknown", "nova", ErrInvalidToken, 2},
		{"invalid service", validUser, validService, "user", "unknown", ErrInvalidToken, 1},
		{"combined user headers", validUser, validService, "user,other", "nova", ErrInvalidToken, 0},
		{"combined service headers", validUser, validService, "user", "nova,other", ErrInvalidToken, 0},
		{"expired user allowed by server", compositeToken{user: validUser.user, project: validUser.project, roles: validUser.roles, expires: now.Add(-time.Hour)}, validService, "user", "nova", nil, 2},
		{"expired user outside server window", compositeToken{user: validUser.user, project: validUser.project, roles: validUser.roles, expires: now.Add(-49 * time.Hour)}, validService, "user", "nova", ErrInvalidToken, 2},
		{"expired service", validUser, compositeToken{user: "nova", project: "service-project", roles: []string{"service"}, expires: now.Add(-time.Minute)}, "user", "nova", ErrInvalidToken, 1},
		{"revoked user", compositeToken{user: validUser.user, project: validUser.project, roles: validUser.roles, expires: validUser.expires, revoked: true}, validService, "user", "nova", ErrInvalidToken, 2},
		{"revoked expired user", compositeToken{user: validUser.user, project: validUser.project, roles: validUser.roles, expires: now.Add(-time.Minute), revoked: true}, validService, "user", "nova", ErrInvalidToken, 2},
		{"revoked service", validUser, compositeToken{user: validService.user, project: validService.project, roles: validService.roles, expires: validService.expires, revoked: true}, "user", "nova", ErrInvalidToken, 1},
		{"service without role", validUser, compositeToken{user: "nova", project: "service-project", roles: []string{"admin"}, expires: validService.expires}, "user", "nova", ErrForbidden, 1},
		{"service wrong project", validUser, compositeToken{user: "nova", project: "other-project", roles: []string{"service"}, expires: validService.expires}, "user", "nova", ErrForbidden, 1},
		{"service unscoped", validUser, compositeToken{user: "nova", roles: []string{"service"}, expires: validService.expires}, "user", "nova", ErrForbidden, 1},
		{"user unscoped", compositeToken{user: "user", roles: validUser.roles, expires: validUser.expires}, validService, "user", "nova", ErrForbidden, 2},
		{"user reader only", compositeToken{user: validUser.user, project: validUser.project, roles: []string{"reader"}, expires: validUser.expires}, validService, "user", "nova", nil, 2},
		{"service dependency outage", validUser, compositeToken{status: 503}, "user", "nova", ErrUnavailable, 1},
		{"user validation forbidden", compositeToken{status: 403}, validService, "user", "nova", ErrUnavailable, 2},
		{"user validation outage", compositeToken{status: 500}, validService, "user", "nova", ErrUnavailable, 2},
		{"service access rules unsupported", validUser, compositeToken{user: "nova", project: "service-project", roles: []string{"service"}, expires: validService.expires, extra: map[string]any{"access_rules": []any{map[string]string{"service": "image"}}}}, "user", "nova", ErrForbidden, 1},
		{"user binding unsupported", compositeToken{user: validUser.user, project: validUser.project, roles: validUser.roles, expires: validUser.expires, extra: map[string]any{"bind": map[string]string{"kerberos": "principal"}}}, validService, "user", "nova", ErrForbidden, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, trace := compositeValidator(t, map[string]compositeToken{"user": tc.user, "nova": tc.service})
			who, err := k.AuthenticateComposite(context.Background(), tc.userToken, tc.serviceToken)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			calls := trace.snapshot()
			if len(calls) != tc.validated {
				t.Fatalf("validation count: %v", calls)
			}
			for i, c := range calls {
				if c.allowExpired != (i == 1) {
					t.Fatalf("wrong expiry allowance/order: %v", calls)
				}
			}
			if err == nil {
				if who.UserID != tc.user.user || who.ProjectID != tc.user.project || !reflect.DeepEqual(who.Roles, tc.user.roles) || !who.ExpiresAt.Equal(tc.user.expires) || who.Check(now) != nil {
					t.Fatal("original identity/expiry changed")
				}
				if !errors.Is(who.Check(tc.service.expires), ErrInvalidToken) {
					t.Fatal("authorization outlived the service token")
				}
			}
		})
	}
}

func TestCompositeDoesNotCacheClientsOrLeakAllowance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	u := compositeToken{user: "user", project: "tenant", roles: []string{"reader"}, expires: now.Add(-time.Minute)}
	s := compositeToken{user: "nova", project: "service-project", roles: []string{"service"}, expires: now.Add(time.Hour)}
	k, trace := compositeValidator(t, map[string]compositeToken{"user": u, "nova": s})
	for n := 0; n < 2; n++ {
		if _, err := k.AuthenticateComposite(context.Background(), "user", "nova"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := k.Authenticate(context.Background(), "user"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired allowance leaked into single-token authentication")
	}
	calls := trace.snapshot()
	if len(calls) != 5 || calls[4].allowExpired {
		t.Fatal("client validation cached or expiry allowance leaked")
	}
	// JSON or client identity headers cannot construct the private allowance.
	var untrusted Identity
	if json.Unmarshal([]byte(`{"UserID":"user","ProjectID":"tenant","ExpiresAt":"2000-01-01T00:00:00Z","serviceExpiresAt":"2999-01-01T00:00:00Z"}`), &untrusted) != nil || !errors.Is(untrusted.Check(now), ErrInvalidToken) {
		t.Fatal("untrusted identity constructed an expiry allowance")
	}
}

func TestCompositePreservesSingleTokenFailureIdentity(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.Header().Set("X-Subject-Token", "validator")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(time.Hour), "s", "service"))
			return
		}
		// Even an upstream 200 must not expose an expired user as a usable result.
		_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(-time.Minute), "tenant", "reader"))
	}))
	defer s.Close()
	who, err := NewKeystone(s.URL+"/v3", s.Client(), fixtureCredential()).Authenticate(context.Background(), "expired")
	if !errors.Is(err, ErrInvalidToken) || who.UserID != "" || who.ProjectID != "" || len(who.Roles) != 0 || !who.ExpiresAt.IsZero() {
		t.Fatal("failed single-token validation returned an identity")
	}
}

func TestCompositeConcurrentIdentityIsolation(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	k, trace := compositeValidator(t, map[string]compositeToken{
		"first":  {user: "first", project: "tenant-a", roles: []string{"reader"}, expires: expires},
		"second": {user: "second", project: "tenant-b", roles: []string{"member"}, expires: expires},
		"nova":   {user: "nova", project: "service-project", roles: []string{"service", "admin"}, expires: expires},
	})
	var wg sync.WaitGroup
	for _, token := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 3; n++ {
				who, err := k.AuthenticateComposite(context.Background(), token, "nova")
				if err != nil || who.UserID != token || who.HasRole("admin") || (token == "first" && who.ProjectID != "tenant-a") || (token == "second" && who.ProjectID != "tenant-b") {
					t.Error("concurrent client identity leaked")
				}
			}
		}()
	}
	wg.Wait()
	if len(trace.snapshot()) != 12 {
		t.Fatal("client/service token validation was cached")
	}
}

func TestCompositeCancellationAndMalformedResponses(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	k, trace := compositeValidator(t, map[string]compositeToken{"nova": {user: "nova", project: "service-project", roles: []string{"service"}, expires: expires}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := k.AuthenticateComposite(ctx, "user", "nova"); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) || len(trace.snapshot()) != 0 {
		t.Fatal("cancelled authentication performed validation")
	}
	for _, claims := range []map[string]any{
		{"user": map[string]string{"id": ""}}, {"expires_at": nil}, {"roles": []any{map[string]string{"name": ""}}},
	} {
		k, _ := compositeValidator(t, map[string]compositeToken{
			"nova": {user: "nova", project: "service-project", roles: []string{"service"}, expires: expires},
			"user": {user: "user", project: "tenant", roles: []string{"reader"}, expires: expires, extra: claims},
		})
		if _, err := k.AuthenticateComposite(context.Background(), "user", "nova"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("malformed user claims accepted")
		}
	}
}

func TestCompositeRechecksRevocation(t *testing.T) {
	for _, revokedSubject := range []string{"user", "nova"} {
		t.Run(revokedSubject, func(t *testing.T) {
			var revoked atomic.Bool
			var serviceChecks, userChecks atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					w.Header().Set("X-Subject-Token", "validator")
					w.WriteHeader(201)
					_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(time.Hour), "s", "service"))
					return
				}
				subject := r.Header.Get("X-Subject-Token")
				if subject == "nova" {
					serviceChecks.Add(1)
				} else {
					userChecks.Add(1)
				}
				if revoked.Load() && subject == revokedSubject {
					w.WriteHeader(404)
					return
				}
				project, roles := "tenant", []string{"reader"}
				if subject == "nova" {
					project, roles = "s", []string{"service", "admin"}
				}
				_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(time.Hour), project, roles...))
			}))
			defer s.Close()
			k := NewKeystone(s.URL+"/v3", s.Client(), fixtureCredential())
			if _, err := k.AuthenticateComposite(context.Background(), "user", "nova"); err != nil {
				t.Fatal(err)
			}
			revoked.Store(true)
			if _, err := k.AuthenticateComposite(context.Background(), "user", "nova"); !errors.Is(err, ErrInvalidToken) {
				t.Fatal("revoked credential reused from cache")
			}
			wantUserChecks := int32(2)
			if revokedSubject == "nova" {
				wantUserChecks = 1
			}
			if serviceChecks.Load() != 2 || userChecks.Load() != wantUserChecks {
				t.Fatal("revocation validation order changed")
			}
		})
	}
}

func TestCompositeRejectsExpiredServiceEvenIfUpstreamReturns200(t *testing.T) {
	var validations atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.Header().Set("X-Subject-Token", "validator")
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(time.Hour), "s", "service"))
			return
		}
		validations.Add(1)
		if r.URL.Query().Has("allow_expired") || r.Header.Get("X-Subject-Token") != "nova" {
			t.Error("expired service reached user validation")
		}
		_ = json.NewEncoder(w).Encode(tokenBody(time.Now().Add(-time.Minute), "s", "service"))
	}))
	defer s.Close()
	if _, err := NewKeystone(s.URL+"/v3", s.Client(), fixtureCredential()).AuthenticateComposite(context.Background(), "user", "nova"); !errors.Is(err, ErrInvalidToken) || validations.Load() != 1 {
		t.Fatal("expired service was accepted or user was queried")
	}
}
