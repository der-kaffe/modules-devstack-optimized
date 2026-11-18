package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Each test pins one property of keystonemiddleware's TokenCache as used by
// official Glance (token_cache_time=300 by default).

func cachedValidator(t testing.TB, tokens map[string]compositeToken, ttl time.Duration) (*Keystone, *validationTrace) {
	t.Helper()
	k, trace := compositeValidator(t, tokens)
	WithTokenCache(ttl, 100)(k)
	return k, trace
}

func TestTokenCacheReusesSuccessfulValidation(t *testing.T) {
	now := time.Now()
	k, trace := cachedValidator(t, map[string]compositeToken{
		"user": {user: "u", project: "p", roles: []string{"reader"}, expires: now.Add(time.Hour)},
	}, 5*time.Minute)
	for n := 0; n < 5; n++ {
		identity, err := k.Authenticate(context.Background(), "user")
		if err != nil || identity.UserID != "u" || !identity.HasRole("reader") {
			t.Fatal(identity, err)
		}
	}
	if calls := trace.snapshot(); len(calls) != 1 {
		t.Fatalf("validated %d times, want 1 (cache hit)", len(calls))
	}
}

func TestTokenCacheEntryLivesTokenCacheTime(t *testing.T) {
	now := time.Now()
	k, trace := cachedValidator(t, map[string]compositeToken{
		"user": {user: "u", project: "p", roles: []string{"reader"}, expires: now.Add(time.Hour)},
	}, 5*time.Minute)
	clock := now
	k.cache.now = func() time.Time { return clock }
	if _, err := k.Authenticate(context.Background(), "user"); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(5*time.Minute - time.Second)
	if _, err := k.Authenticate(context.Background(), "user"); err != nil || len(trace.snapshot()) != 1 {
		t.Fatal("entry must still be fresh", err)
	}
	clock = now.Add(5 * time.Minute)
	if _, err := k.Authenticate(context.Background(), "user"); err != nil || len(trace.snapshot()) != 2 {
		t.Fatal("entry older than token_cache_time must be revalidated", err)
	}
}

func TestTokenCacheNeverStoresFailures(t *testing.T) {
	k, trace := cachedValidator(t, map[string]compositeToken{}, 5*time.Minute)
	for n := 0; n < 3; n++ {
		if _, err := k.Authenticate(context.Background(), "unknown"); !errors.Is(err, ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	if calls := trace.snapshot(); len(calls) != 3 {
		t.Fatalf("rejections were cached: %d calls", len(calls))
	}
}

// As _validate_token: expiry is checked on each use, also from the cache.
func TestTokenCacheChecksExpiryOnEveryUse(t *testing.T) {
	now := time.Now()
	k, trace := cachedValidator(t, map[string]compositeToken{
		"user": {user: "u", project: "p", roles: []string{"reader"}, expires: now.Add(2 * time.Second)},
	}, 5*time.Minute)
	if _, err := k.Authenticate(context.Background(), "user"); err != nil {
		t.Fatal(err)
	}
	entry, ok := k.cache.get("user")
	if !ok {
		t.Fatal("not cached")
	}
	entry.ExpiresAt = now.Add(-time.Second)
	k.cache.put("user", entry) // simulate time passing beyond the token expiry
	if _, err := k.Authenticate(context.Background(), "user"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired token accepted from cache", err)
	}
	if len(trace.snapshot()) != 1 {
		t.Fatal("expiry must be decided locally on a cache hit")
	}
}

// Data fetched with allow_expired for a composite request is cached by token
// (as in the middleware) but never authorizes a plain request.
func TestTokenCacheDoesNotLeakExpiredAllowance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	k, trace := cachedValidator(t, map[string]compositeToken{
		"user": {user: "user", project: "tenant", roles: []string{"reader"}, expires: now.Add(-time.Minute)},
		"nova": {user: "nova", project: "service-project", roles: []string{"service"}, expires: now.Add(time.Hour)},
	}, 5*time.Minute)
	for n := 0; n < 2; n++ {
		if _, err := k.AuthenticateComposite(context.Background(), "user", "nova"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := k.Authenticate(context.Background(), "user"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("expired allowance leaked into single-token authentication")
	}
	if calls := trace.snapshot(); len(calls) != 2 {
		t.Fatalf("composite tokens must be validated once each and then cached: %d calls", len(calls))
	}
}

func TestTokenCacheKeepsServiceProjectRule(t *testing.T) {
	now := time.Now()
	k, _ := cachedValidator(t, map[string]compositeToken{
		"user":  {user: "u", project: "p", roles: []string{"reader"}, expires: now.Add(time.Hour)},
		"other": {user: "x", project: "another-project", roles: []string{"service"}, expires: now.Add(time.Hour)},
	}, 5*time.Minute)
	for n := 0; n < 2; n++ { // second round is served from the cache
		if _, err := k.AuthenticateComposite(context.Background(), "user", "other"); !errors.Is(err, ErrForbidden) {
			t.Fatal("service token from another project accepted", err)
		}
	}
}

func TestTokenCacheIsBoundedAndHoldsNoTokens(t *testing.T) {
	c := newTokenCache(time.Minute, 3)
	for n := 0; n < 10; n++ {
		c.put(fmt.Sprintf("secret-token-%d", n), Identity{UserID: "u", ProjectID: "p", ExpiresAt: time.Now().Add(time.Hour), Roles: []string{"reader"}})
	}
	if c.order.Len() != 3 || len(c.entries) != 3 {
		t.Fatalf("cache not bounded: %d", c.order.Len())
	}
	if _, ok := c.get("secret-token-0"); ok {
		t.Fatal("least recently used entry kept")
	}
	if _, ok := c.get("secret-token-9"); !ok {
		t.Fatal("most recent entry evicted")
	}
	for e := c.order.Front(); e != nil; e = e.Next() {
		if fmt.Sprint(e.Value) == "secret-token-9" {
			t.Fatal("raw token stored")
		}
	}
	got, _ := c.get("secret-token-9")
	got.Roles[0] = "admin"
	if again, _ := c.get("secret-token-9"); again.HasRole("admin") {
		t.Fatal("callers can mutate cached identities")
	}
}

func TestTokenCacheDisabledValidatesEveryCall(t *testing.T) {
	now := time.Now()
	k, trace := cachedValidator(t, map[string]compositeToken{
		"user": {user: "u", project: "p", roles: []string{"reader"}, expires: now.Add(time.Hour)},
	}, 0)
	for n := 0; n < 3; n++ {
		if _, err := k.Authenticate(context.Background(), "user"); err != nil {
			t.Fatal(err)
		}
	}
	if k.cache != nil || len(trace.snapshot()) != 3 {
		t.Fatal("disabled cache must validate every call")
	}
}

func TestTokenCacheConcurrentUse(t *testing.T) {
	now := time.Now()
	tokens := map[string]compositeToken{}
	for n := 0; n < 8; n++ {
		tokens[fmt.Sprint("t", n)] = compositeToken{user: fmt.Sprint("u", n), project: "p", roles: []string{"reader"}, expires: now.Add(time.Hour)}
	}
	k, _ := cachedValidator(t, tokens, 5*time.Minute)
	var wg sync.WaitGroup
	for n := 0; n < 64; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			token := fmt.Sprint("t", n%8)
			identity, err := k.Authenticate(context.Background(), token)
			if err != nil || identity.UserID != fmt.Sprint("u", n%8) {
				t.Error("identity mixed between tokens", identity.UserID, err)
			}
		}(n)
	}
	wg.Wait()
}
