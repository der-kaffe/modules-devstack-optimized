package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
)

// Keystone uses a non-admin application credential with the service role.
// Its own service token is always cached. Incoming tokens are validated on
// every call unless WithTokenCache is given (Glance does, like official Glance's
// keystonemiddleware; Placement does not).
type Keystone struct {
	url        string
	client     *http.Client
	credential config.ServiceCredential
	mu         sync.Mutex
	token      validatorToken
	refresh    *tokenRefresh
	cache      *tokenCache
}

// KeystoneOption configures optional validator behavior.
type KeystoneOption func(*Keystone)

// WithTokenCache reuses successful validations of incoming tokens for ttl, as
// keystonemiddleware's token_cache_time does. ttl <= 0 keeps validating always.
func WithTokenCache(ttl time.Duration, maxEntries int) KeystoneOption {
	return func(k *Keystone) {
		if ttl > 0 && maxEntries > 0 {
			k.cache = newTokenCache(ttl, maxEntries)
		}
	}
}

type tokenRefresh struct {
	done  chan struct{}
	token validatorToken
	err   error
}

// Keep the validator's project bound to the exact cached token generation.
type validatorToken struct {
	value, projectID string
	expires          time.Time
}

func NewKeystone(url string, client *http.Client, credential config.ServiceCredential, options ...KeystoneOption) *Keystone {
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	k := &Keystone{url: strings.TrimRight(url, "/") + "/auth/tokens", client: &copyClient, credential: credential}
	for _, option := range options {
		option(k)
	}
	return k
}

type tokenResponse struct {
	Token struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
		Roles []struct {
			Name string `json:"name"`
		} `json:"roles"`
		ExpiresAt   time.Time                  `json:"expires_at"`
		AccessRules []json.RawMessage          `json:"access_rules"`
		Bind        map[string]json.RawMessage `json:"bind"`
	} `json:"token"`
}

func decodeIdentity(body io.Reader) (Identity, error) {
	data, err := io.ReadAll(io.LimitReader(body, 1048577))
	if err != nil || len(data) > 1048576 {
		return Identity{}, ErrUnavailable
	}
	var decoded tokenResponse
	if json.Unmarshal(data, &decoded) != nil {
		return Identity{}, ErrUnavailable
	}
	// No access-rule or token-binding enforcement is implemented here. Never
	// turn an unsupported restriction into an unrestricted identity.
	if len(decoded.Token.AccessRules) != 0 || len(decoded.Token.Bind) != 0 {
		return Identity{}, ErrForbidden
	}
	identity := Identity{UserID: decoded.Token.User.ID, ProjectID: decoded.Token.Project.ID, ExpiresAt: decoded.Token.ExpiresAt}
	for _, role := range decoded.Token.Roles {
		if role.Name == "" {
			return Identity{}, ErrUnavailable
		}
		identity.Roles = append(identity.Roles, role.Name)
	}
	// Expiry is checked by the caller: only a validated composite request may
	// ask Keystone to apply its configured expired-token window.
	if identity.UserID == "" || identity.ExpiresAt.IsZero() {
		return Identity{}, ErrUnavailable
	}
	if identity.ProjectID == "" {
		return Identity{}, ErrForbidden
	}
	return identity, nil
}

func readIdentity(body io.Reader) (Identity, error) {
	identity, err := decodeIdentity(body)
	if err != nil {
		return Identity{}, err
	}
	if err := identity.Check(time.Now()); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func validHeaderToken(token string) bool {
	if token == "" || len(token) > 16384 {
		return false
	}
	for _, c := range token {
		if c <= 32 || c >= 127 || c == ',' {
			return false
		}
	}
	return true
}

func (k *Keystone) serviceToken(ctx context.Context) (validatorToken, error) {
	if err := ctx.Err(); err != nil {
		return validatorToken{}, errors.Join(ErrUnavailable, err)
	}
	k.mu.Lock()
	if k.token.value != "" && time.Now().Add(30*time.Second).Before(k.token.expires) {
		token := k.token
		k.mu.Unlock()
		return token, nil
	}
	if flight := k.refresh; flight != nil {
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return validatorToken{}, errors.Join(ErrUnavailable, ctx.Err())
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return validatorToken{}, errors.Join(ErrUnavailable, err)
			}
			return flight.token, flight.err
		}
	}
	flight := &tokenRefresh{done: make(chan struct{})}
	k.refresh = flight
	k.mu.Unlock()
	token, err := k.fetchServiceToken(ctx)
	if ctx.Err() != nil {
		err = errors.Join(ErrUnavailable, ctx.Err())
	}
	k.mu.Lock()
	if err == nil {
		k.token = token
	}
	flight.token, flight.err = token, err
	k.refresh = nil
	close(flight.done)
	k.mu.Unlock()
	return token, err
}

func (k *Keystone) fetchServiceToken(ctx context.Context) (validatorToken, error) {
	if k.credential.ID == "" || k.credential.Secret == "" {
		return validatorToken{}, ErrUnavailable
	}
	body := map[string]any{"auth": map[string]any{"identity": map[string]any{"methods": []string{"application_credential"}, "application_credential": map[string]string{"id": k.credential.ID, "secret": k.credential.Secret}}}}
	encoded, err := json.Marshal(body)
	if err != nil {
		return validatorToken{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.url, bytes.NewReader(encoded))
	if err != nil {
		return validatorToken{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := k.client.Do(req)
	if err != nil {
		return validatorToken{}, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return validatorToken{}, ErrUnavailable
	}
	identity, err := readIdentity(response.Body)
	if err != nil || !identity.HasRole("service") || identity.HasRole("admin") {
		return validatorToken{}, ErrUnavailable
	}
	token := response.Header.Get("X-Subject-Token")
	if !validHeaderToken(token) {
		return validatorToken{}, ErrUnavailable
	}
	return validatorToken{value: token, projectID: identity.ProjectID, expires: identity.ExpiresAt}, nil
}

func (k *Keystone) invalidate(token string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token.value == token {
		k.token = validatorToken{}
	}
}

func (k *Keystone) Authenticate(ctx context.Context, token string) (Identity, error) {
	identity, _, err := k.validate(ctx, token, false)
	return identity, err
}

// AuthenticateComposite follows the installed middleware's ordering: validate
// the service without an expiry allowance, then validate the user with Keystone's
// allow_expired=true. Keystone, not a local duration, enforces expiry and revocation.
// Additionally restrict callers to the validator's service project. The user's
// project need not be that project, and the service's admin role is never delegated.
func (k *Keystone) AuthenticateComposite(ctx context.Context, userToken, serviceToken string) (Identity, error) {
	if !validHeaderToken(userToken) || !validHeaderToken(serviceToken) {
		return Identity{}, ErrInvalidToken
	}
	service, validatorProject, err := k.validate(ctx, serviceToken, false)
	if err != nil {
		return Identity{}, err
	}
	if !service.HasRole("service") || service.ProjectID != validatorProject {
		return Identity{}, ErrForbidden
	}
	user, _, err := k.validate(ctx, userToken, true)
	if err != nil {
		return Identity{}, err
	}
	user.serviceExpiresAt = service.ExpiresAt
	if err := user.Check(time.Now()); err != nil {
		return Identity{}, err
	}
	return user, nil
}

func (k *Keystone) validate(ctx context.Context, token string, allowExpired bool) (Identity, string, error) {
	if !validHeaderToken(token) {
		return Identity{}, "", ErrInvalidToken
	}
	if k.cache != nil {
		if identity, ok := k.cache.get(token); ok {
			// The validator project still comes from the current own token.
			serviceToken, err := k.serviceToken(ctx)
			if err != nil {
				return Identity{}, "", err
			}
			if !allowExpired {
				if err := identity.Check(time.Now()); err != nil {
					return Identity{}, "", err
				}
			}
			return identity, serviceToken.projectID, nil
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		serviceToken, err := k.serviceToken(ctx)
		if err != nil {
			return Identity{}, "", err
		}
		endpoint := k.url + "?nocatalog"
		if allowExpired {
			endpoint += "&allow_expired=true"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return Identity{}, "", ErrUnavailable
		}
		req.Header.Set("X-Auth-Token", serviceToken.value)
		req.Header.Set("X-Subject-Token", token)
		req.Header.Set("Accept", "application/json")
		response, err := k.client.Do(req)
		if err != nil {
			return Identity{}, "", ErrUnavailable
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			k.invalidate(serviceToken.value)
			continue
		}
		if response.StatusCode == http.StatusNotFound {
			response.Body.Close()
			return Identity{}, "", ErrInvalidToken
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return Identity{}, "", ErrUnavailable
		}
		identity, err := decodeIdentity(response.Body)
		response.Body.Close()
		if ctx.Err() != nil {
			return Identity{}, "", errors.Join(ErrUnavailable, ctx.Err())
		}
		if err == nil && k.cache != nil {
			k.cache.put(token, identity) // like TokenCache.set after verify_token
		}
		if err == nil && !allowExpired {
			err = identity.Check(time.Now())
		}
		if err != nil {
			return Identity{}, "", err
		}
		return identity, serviceToken.projectID, nil
	}
	return Identity{}, "", ErrUnavailable
}
