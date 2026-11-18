package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ServiceCredential struct {
	ID     string `json:"application_credential_id"`
	Secret string `json:"application_credential_secret"`
}

type Config struct {
	ListenAddress, PublicURL, DatabaseDSN, KeystoneURL string
	Credential                                         ServiceCredential
	RequestTimeout, DependencyTimeout                  time.Duration
	MaxBodyBytes                                       int64
	Binary                                             Binary
	// TokenCacheTime mirrors keystonemiddleware's token_cache_time (Glance only;
	// 0 = validate every request). Default 300s, as in official Glance.
	TokenCacheTime time.Duration
}

// ReadSecret rejects group/world-readable files and never returns their contents in errors.
func ReadSecret(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open private configuration file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return nil, errors.New("private configuration must be a regular file <=64 KiB with mode 0600 or stricter")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, errors.New("cannot read private configuration file")
	}
	return b, nil
}

func ValidateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(value, "?#") || u.Opaque != "" {
		return errors.New("expected an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if strings.ContainsAny(value, "\r\n\t \\") || strings.Contains(u.Path, "..") || strings.Contains(value, "%") || (u.Path != "" && path.Clean(u.Path) != strings.TrimRight(u.Path, "/") && strings.Trim(u.Path, "/") != "") {
		return errors.New("invalid URL path")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid URL port")
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid URL host")
	}
	if net.ParseIP(u.Hostname()) == nil {
		host := strings.TrimSuffix(u.Hostname(), ".")
		if len(host) > 253 {
			return errors.New("invalid URL host")
		}
		for _, label := range strings.Split(host, ".") {
			if !dnsLabel.MatchString(label) {
				return errors.New("invalid URL host")
			}
		}
	}
	return nil
}

var dnsLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Load keeps migration mode independent of Keystone. It does not open a DB or run SQL.
func Load(getenv func(string) string, mode string) (Config, error) {
	if mode != "serve" && mode != "migrate" && mode != "store-scope" && mode != "store-init" {
		return Config{}, errors.New("command must be serve, migrate, store-scope or store-init")
	}
	return load(getenv, mode, "GLANCE_GO_", "127.0.0.1:9293")
}

// LoadPlacement uses an independent namespace; Glance settings cannot select its DB.
func LoadPlacement(getenv func(string) string, mode string) (Config, error) {
	if mode != "serve" && mode != "migrate" {
		return Config{}, errors.New("command must be serve or migrate")
	}
	return load(getenv, mode, "PLACEMENT_GO_", "127.0.0.1:8779")
}

func load(getenv func(string) string, mode, prefix, defaultAddress string) (Config, error) {
	c := Config{ListenAddress: defaultAddress, PublicURL: "http://" + defaultAddress, RequestTimeout: 15 * time.Second, DependencyTimeout: 5 * time.Second, MaxBodyBytes: 1048576}
	if value := getenv(prefix + "LISTEN_ADDR"); value != "" {
		c.ListenAddress = value
	}
	host, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil || host == "" {
		return Config{}, errors.New(prefix + "LISTEN_ADDR must contain an explicit host and port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Config{}, errors.New("invalid listener port")
	}
	if value := getenv(prefix + "PUBLIC_BASE_URL"); value != "" {
		c.PublicURL = value
	}
	if err := ValidateURL(c.PublicURL); err != nil {
		return Config{}, fmt.Errorf(prefix+"PUBLIC_BASE_URL: %w", err)
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	for key, target := range map[string]*time.Duration{prefix + "REQUEST_TIMEOUT": &c.RequestTimeout, prefix + "DEPENDENCY_TIMEOUT": &c.DependencyTimeout} {
		if value := getenv(key); value != "" {
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 || d > time.Minute {
				return Config{}, fmt.Errorf("%s must be >0 and <=1m", key)
			}
			*target = d
		}
	}
	if c.DependencyTimeout >= c.RequestTimeout {
		return Config{}, errors.New("dependency timeout must be less than request timeout")
	}
	if value := getenv(prefix + "MAX_BODY_BYTES"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 || n > 10485760 {
			return Config{}, errors.New(prefix + "MAX_BODY_BYTES must be between 1 and 10485760")
		}
		c.MaxBodyBytes = n
	}
	dsn, err := ReadSecret(getenv(prefix + "DATABASE_DSN_FILE"))
	if err != nil {
		return Config{}, fmt.Errorf("database configuration: %w", err)
	}
	c.DatabaseDSN = strings.TrimSpace(string(dsn))
	if c.DatabaseDSN == "" {
		return Config{}, errors.New("empty database configuration")
	}
	// Binary storage is Glance policy; Placement must not read that namespace.
	if prefix == "GLANCE_GO_" {
		if err := loadBinary(&c, getenv, mode); err != nil {
			return Config{}, err
		}
		c.TokenCacheTime = 300 * time.Second
		if value := getenv("GLANCE_GO_TOKEN_CACHE_TIME"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n == 0 || n < -1 || n > 86400 {
				return Config{}, errors.New("GLANCE_GO_TOKEN_CACHE_TIME must be -1 (disabled) or 1..86400 seconds")
			}
			c.TokenCacheTime = time.Duration(max(n, 0)) * time.Second
		}
	}
	if mode != "serve" {
		return c, nil
	}
	c.KeystoneURL = strings.TrimRight(getenv(prefix+"KEYSTONE_AUTH_URL"), "/")
	if err := ValidateURL(c.KeystoneURL); err != nil {
		return Config{}, fmt.Errorf(prefix+"KEYSTONE_AUTH_URL: %w", err)
	}
	if !strings.HasSuffix(c.KeystoneURL, "/v3") {
		return Config{}, errors.New("Keystone URL must end in /v3")
	}
	credential, err := ReadSecret(getenv(prefix + "KEYSTONE_CREDENTIAL_FILE"))
	if err != nil {
		return Config{}, fmt.Errorf("Keystone configuration: %w", err)
	}
	if err := json.Unmarshal(credential, &c.Credential); err != nil || c.Credential.ID == "" || c.Credential.Secret == "" {
		return Config{}, errors.New("invalid Keystone application credential file")
	}
	return c, nil
}
