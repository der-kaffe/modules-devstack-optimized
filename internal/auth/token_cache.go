package auth

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"
)

// tokenCache mirrors keystonemiddleware's auth_token TokenCache, which official
// Glance uses: a validated token's data is reused for token_cache_time seconds
// (default 300), keyed by SHA-256 of the token, and only successful validations
// are stored. As in the middleware, expiry is still checked on every use
// (_validate_token), so data fetched with allow_expired never grants access to
// a plain request. Memory is bounded (LRU) instead of memcached; tokens
// themselves are never stored.
type tokenCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]*list.Element
	order   *list.List // front: most recently used
}

type cachedToken struct {
	key      [sha256.Size]byte
	identity Identity
	stored   time.Time
}

func newTokenCache(ttl time.Duration, max int) *tokenCache {
	return &tokenCache{ttl: ttl, max: max, now: time.Now, entries: map[[sha256.Size]byte]*list.Element{}, order: list.New()}
}

func (c *tokenCache) get(token string) (Identity, bool) {
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return Identity{}, false
	}
	entry := element.Value.(*cachedToken)
	if c.now().Sub(entry.stored) >= c.ttl {
		c.order.Remove(element)
		delete(c.entries, key)
		return Identity{}, false
	}
	c.order.MoveToFront(element)
	return entry.identity.clone(), true
}

func (c *tokenCache) put(token string, identity Identity) {
	key := sha256.Sum256([]byte(token))
	identity = identity.clone()
	identity.serviceExpiresAt = time.Time{} // only per-request composite validation grants it
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value = &cachedToken{key: key, identity: identity, stored: c.now()}
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(&cachedToken{key: key, identity: identity, stored: c.now()})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cachedToken).key)
	}
}

func (i Identity) clone() Identity {
	i.Roles = append([]string(nil), i.Roles...)
	return i
}
