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

// ServiceCredential is an application credential (ID/Secret) or, read from
// the original's [keystone_authtoken], a password-scoped service user.
type ServiceCredential struct {
	ID     string `json:"application_credential_id"`
	Secret string `json:"application_credential_secret"`

	Username, Password, ProjectName, UserDomainName, ProjectDomainName string `json:"-"`
}

type Config struct {
	ListenAddress, PublicURL, DatabaseDSN, KeystoneURL string
	Credential                                         ServiceCredential
	RequestTimeout, DependencyTimeout                  time.Duration
	MaxBodyBytes                                       int64
	Binary                                             Binary
	// TokenCacheTime mirrors keystonemiddleware's token_cache_time
	// (0 = validate every request). Default 300s, as in official Glance and Placement.
	TokenCacheTime time.Duration
	// DBMaxConnections bounds the Placement SQL pool (Placement only; 0 in Glance).
	DBMaxConnections int
	// Notifications is Glance only and read only by serve.
	Notifications Notifications
	// OfficialCatalog is the original Glance database Glance Go serves from
	// (read from its glance-api.conf). Its files are in
	// Binary.OfficialStoreDir, the original's filesystem_store_datadir.
	OfficialCatalog string
	// Limits is the original's use_keystone_limits with its [oslo_limit]
	// credentials (Glance only, read only by serve).
	Limits KeystoneLimits
	// Import is the original's interoperable import configuration.
	Import ImportSettings
	// Cache is the original's image cache (paste flavor with "cache").
	Cache CacheSettings
	// ImageFormat is [image_format]: upload content inspection.
	ImageFormat ImageFormatSettings
	// Quotas are image_member_quota, image_property_quota, image_tag_quota.
	MemberQuota, PropertyQuota, TagQuota int
	// DBMaxRetries and DBRetryInterval are [database] max_retries and
	// retry_interval (oslo.db: 10 and 10 s; -1 retries forever).
	DBMaxRetries    int
	DBRetryInterval time.Duration
	// UserStorageQuota is user_storage_quota in bytes (0: none); applied
	// only without use_keystone_limits, as the original.
	UserStorageQuota int64
	// ShowDirectURL, ShowMultipleLocations: show_image_direct_url and
	// show_multiple_locations (both off by default, OSSN-0065).
	ShowDirectURL, ShowMultipleLocations bool
	// PolicyFile is [oslo_policy] policy_file, resolved next to glance-api.conf.
	PolicyFile string
	// DelayedDelete is [DEFAULT] delayed_delete (data left pending_delete
	// for glance-scrubber).
	DelayedDelete bool
	// Backends are enabled_backends (multi-store); empty with one store.
	Backends       []StoreBackend
	DefaultBackend string
	// PropertyProtectionFile and its rule format (roles or policies).
	PropertyProtectionFile, PropertyProtectionFormat string
}

// ImageFormatSettings: require_image_format_match (default True) and
// gpt_safety_checks_nonfatal, as the original.
type ImageFormatSettings struct {
	RequireMatch bool
	GPTNonfatal  []string
}

// CacheSettings: image_cache_dir with the centralized_db driver (cached
// images in the catalog database, per node_reference_url).
type CacheSettings struct {
	Enabled          bool
	Dir              string
	NodeURL          string // worker_self_reference_url
	PrefetchInterval time.Duration
	MaxSize          int64
}

// ImportSettings: node_staging_uri, enabled_import_methods,
// task_time_to_live and [import_filtering_opts], with the original's defaults.
type ImportSettings struct {
	StagingDir                    string
	Methods                       []string
	TaskTimeToLive                time.Duration
	AllowedSchemes, AllowedHosts  []string
	DisallowedSchemes             []string
	DisallowedHosts               []string
	AllowedPorts, DisallowedPorts []int
	// Plugins is [image_import_opts] image_import_plugins (glance-image-import.conf).
	Plugins          []string
	ConversionFormat string            // [image_conversion] output_format
	InjectIgnore     []string          // [inject_metadata_properties] ignore_user_roles
	Inject           map[string]string // [inject_metadata_properties] inject
	VMDKAllowed      []string          // [image_format] vmdk_allowed_types
	GPTNonfatal      []string          // [image_format] gpt_safety_checks_nonfatal
}

// KeystoneLimits is what oslo.limit reads: the Glance endpoint whose
// registered and project limits apply, and the user that reads them
// (system scope "all", or a project scope).
type KeystoneLimits struct {
	Enabled                                bool
	URL, EndpointID                        string
	Username, Password, UserDomainName     string
	SystemScope                            string
	ProjectName, ProjectDomainName, Region string
	// CacheTTL is how long a project's limits from Keystone are reused
	// (GLANCE_GO_LIMITS_CACHE_SECONDS, default 5 s; 0 asks Keystone every time,
	// as the original does). A changed limit shows up within the TTL.
	CacheTTL time.Duration
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
	var original osloFile
	path := ""
	if prefix == "GLANCE_GO_" { // Placement must never read Glance's namespace
		path = getenv("GLANCE_GO_CONFIG_FILE")
	}
	if prefix == "GLANCE_GO_" {
		// Glance Go always serves the original's catalog, MySQL account, store
		// and Keystone user, read from its glance-api.conf. There is no own mode.
		if path == "" {
			return Config{}, errors.New("GLANCE_GO_CONFIG_FILE (the original's glance-api.conf) is required")
		}
		if original, err = readOriginalConfig(path); err != nil {
			return Config{}, err
		}
	} else {
		dsn, err := ReadSecret(getenv(prefix + "DATABASE_DSN_FILE"))
		if err != nil {
			return Config{}, fmt.Errorf("database configuration: %w", err)
		}
		c.DatabaseDSN = strings.TrimSpace(string(dsn))
		if c.DatabaseDSN == "" {
			return Config{}, errors.New("empty database configuration")
		}
	}
	// Default token cache before the original's glance-api.conf may override it.
	if c.TokenCacheTime, err = tokenCacheTime(getenv, prefix); err != nil {
		return Config{}, err
	}
	// Binary storage is Glance policy; Placement must not read that namespace.
	if prefix == "GLANCE_GO_" {
		if err := loadBinary(&c, getenv, mode); err != nil {
			return Config{}, err
		}
		if err := applyOriginalConfig(&c, original, getenv, mode); err != nil {
			return Config{}, err
		}
	}
	if prefix == "PLACEMENT_GO_" {
		c.DBMaxConnections = 16
		if value := getenv(prefix + "DB_MAX_CONNECTIONS"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 256 {
				return Config{}, errors.New(prefix + "DB_MAX_CONNECTIONS must be between 1 and 256")
			}
			c.DBMaxConnections = n
		}
	}
	if mode != "serve" {
		return c, nil
	}
	if original != nil {
		return c, nil // notifications and Keystone came from the original's file
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

// tokenCacheTime reads <prefix>TOKEN_CACHE_TIME like keystonemiddleware's
// token_cache_time: 300 s by default, -1 disables the cache.
func tokenCacheTime(getenv func(string) string, prefix string) (time.Duration, error) {
	value := getenv(prefix + "TOKEN_CACHE_TIME")
	if value == "" {
		return 300 * time.Second, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n == 0 || n < -1 || n > 86400 {
		return 0, errors.New(prefix + "TOKEN_CACHE_TIME must be -1 (disabled) or 1..86400 seconds")
	}
	return time.Duration(max(n, 0)) * time.Second, nil
}

// StoreBackend is one enabled_backends entry (only "file" stores).
type StoreBackend struct {
	Name, Type, Dir, Description string
	Weight                       int
}
