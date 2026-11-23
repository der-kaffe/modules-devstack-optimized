package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

// Reading the original's glance-api.conf (GLANCE_GO_CONFIG_FILE). With it,
// Glance Go takes from the same file, as oslo.config reads it, what the
// original uses: [database] connection (same MySQL account), [glance_store]
// filesystem_store_datadir, [keystone_authtoken] (same Keystone service user
// and token_cache_time), [oslo_messaging_notifications] with the [DEFAULT]
// transport_url fallback, default_publisher_id, disabled_notifications and
// public_endpoint. Glance Go's own transfer bookkeeping goes to the
// glance_go_shared database on that same server.

// SharedDatabase is where Glance Go keeps its bookkeeping next to the catalog.
const SharedDatabase = "glance_go_shared"

var officialCatalogName = regexp.MustCompile(`^(glance|glance_official_test_[a-z0-9_]+)$`)

type osloFile map[string]map[string]string

// parseOslo reads an oslo.config INI file: repeated sections merge, the last
// value of a key wins, keys are case-insensitive, # and ; start comments and
// continuation lines are not used by DevStack's generated files.
func parseOslo(text string) (osloFile, error) {
	out := osloFile{"DEFAULT": {}}
	section := ""
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if !strings.HasSuffix(line, "]") {
				return nil, errors.New("glance-api.conf: malformed section on line " + strconv.Itoa(n+1))
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			return nil, errors.New("glance-api.conf: malformed option on line " + strconv.Itoa(n+1))
		}
		out[section][strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return out, nil
}

// get falls back to [DEFAULT], as oslo.config does for unset group options.
func (f osloFile) get(section, key string) string {
	if v, ok := f[section][key]; ok {
		return v
	}
	return f["DEFAULT"][key]
}

func (f osloFile) list(section, key string) []string {
	var out []string
	for _, item := range strings.Split(f.get(section, key), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// readOriginalConfig reads the file only if it is a regular file owned by
// this user or root and not writable by group or others.
func readOriginalConfig(path string) (osloFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open the original glance-api.conf")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("the original glance-api.conf must be a regular file not writable by group or others")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || (st.Uid != uint32(os.Geteuid()) && st.Uid != 0) {
		return nil, errors.New("the original glance-api.conf must belong to this user or root")
	}
	data := make([]byte, 1<<20)
	n, err := f.Read(data)
	if err != nil || n == len(data) {
		return nil, errors.New("cannot read the original glance-api.conf")
	}
	return parseOslo(string(data[:n]))
}

// databaseDSN converts the original's SQLAlchemy URL (mysql+pymysql://user:
// password@host[:port]/glance?...) into a Go DSN for the same account and
// server, on database glance_go_shared. It returns the catalog name too.
func databaseDSN(connection string) (dsn, catalog string, err error) {
	u, err := url.Parse(connection)
	if err != nil || !strings.HasPrefix(u.Scheme, "mysql") || u.User == nil || u.Hostname() == "" {
		return "", "", errors.New("the original [database] connection must be a MySQL URL")
	}
	catalog = strings.TrimPrefix(u.Path, "/")
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	c := driver.NewConfig()
	c.User, c.Passwd, c.Net, c.Addr, c.DBName = u.User.Username(), password, "tcp", net.JoinHostPort(u.Hostname(), port), SharedDatabase
	c.ParseTime = true
	return c.FormatDSN(), catalog, nil
}

// applyOriginalConfig fills c from the original's file. Settings that the
// file now provides must not also come from GLANCE_GO_* variables.
func applyOriginalConfig(c *Config, f osloFile, env func(string) string, mode string) error {
	for _, key := range []string{"DATABASE_DSN_FILE", "KEYSTONE_AUTH_URL", "KEYSTONE_CREDENTIAL_FILE", "OFFICIAL_DATABASE", "OFFICIAL_STORE_DIR",
		"NOTIFICATION_DRIVER", "TRANSPORT_URL_FILE", "NOTIFICATION_TOPICS", "DEFAULT_PUBLISHER_ID", "DISABLED_NOTIFICATIONS", "TOKEN_CACHE_TIME"} {
		if env("GLANCE_GO_"+key) != "" {
			return errors.New("GLANCE_GO_" + key + " conflicts with GLANCE_GO_CONFIG_FILE, which provides it")
		}
	}
	dsn, catalog, err := databaseDSN(f.get("database", "connection"))
	if err != nil {
		return err
	}
	if !officialCatalogName.MatchString(catalog) {
		return errors.New("the original [database] connection must name the glance database")
	}
	c.DatabaseDSN, c.OfficialCatalog = dsn, catalog
	c.DBMaxRetries, c.DBRetryInterval = 10, 10*time.Second
	if v := f.get("database", "max_retries"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -1 {
			return errors.New("invalid [database] max_retries")
		}
		c.DBMaxRetries = n
	}
	if v := f.get("database", "retry_interval"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errors.New("invalid [database] retry_interval")
		}
		c.DBRetryInterval = time.Duration(n) * time.Second
	}
	dir := strings.TrimRight(f.get("glance_store", "filesystem_store_datadir"), "/")
	c.DelayedDelete = strings.EqualFold(f.get("DEFAULT", "delayed_delete"), "true")
	// Multi-store: enabled_backends = name:type,...; the default backend's
	// directory is the main store, the others are extra ones.
	if backends := f.list("DEFAULT", "enabled_backends"); len(backends) > 0 {
		c.DefaultBackend = f.get("glance_store", "default_backend")
		for _, b := range backends {
			name, kind, _ := strings.Cut(b, ":")
			name, kind = strings.TrimSpace(name), strings.TrimSpace(kind)
			if kind != "file" {
				return errors.New("only file stores are supported in enabled_backends: " + b)
			}
			d := strings.TrimRight(f.get(name, "filesystem_store_datadir"), "/")
			if d == "" || d[0] != '/' {
				return errors.New("store " + name + " needs an absolute filesystem_store_datadir")
			}
			w, _ := strconv.Atoi(firstOf(f.get(name, "weight"), "0"))
			c.Backends = append(c.Backends, StoreBackend{Name: name, Type: kind, Dir: d, Description: f.get(name, "store_description"), Weight: w})
			if name == c.DefaultBackend {
				dir = d
			}
		}
		if c.DefaultBackend == "" {
			return errors.New("[glance_store] default_backend is required with enabled_backends")
		}
		if dir == strings.TrimRight(f.get("glance_store", "filesystem_store_datadir"), "/") && f.get(c.DefaultBackend, "filesystem_store_datadir") == "" {
			return errors.New("default_backend " + c.DefaultBackend + " is not in enabled_backends")
		}
	}
	if dir == "" || dir[0] != '/' {
		return errors.New("the original [glance_store] filesystem_store_datadir must be absolute")
	}
	c.Binary.OfficialStoreDir = dir
	if v := f.get("keystone_authtoken", "token_cache_time"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -1 || n > 86400 {
			return errors.New("invalid [keystone_authtoken] token_cache_time")
		}
		c.TokenCacheTime = time.Duration(max(n, 0)) * time.Second
	}
	if env("GLANCE_GO_PUBLIC_BASE_URL") == "" {
		if public := strings.TrimRight(f.get("DEFAULT", "public_endpoint"), "/"); public != "" {
			if err := ValidateURL(public); err != nil {
				return errors.New("invalid [DEFAULT] public_endpoint")
			}
			c.PublicURL = public
		}
	}
	if mode != "serve" {
		return nil
	}
	n := Notifications{Driver: "noop", Topics: []string{"notifications"}, PublisherID: "image.localhost", Disabled: map[string]bool{}}
	for _, d := range f.list("oslo_messaging_notifications", "driver") {
		if d == "messagingv2" {
			n.Driver = d
		}
	}
	if n.Driver == "messagingv2" {
		if n.TransportURL = f.get("oslo_messaging_notifications", "transport_url"); n.TransportURL == "" {
			return errors.New("messagingv2 notifications without a transport_url")
		}
	}
	if topics := f.list("oslo_messaging_notifications", "topics"); len(topics) > 0 {
		n.Topics = topics
	}
	if id := f["DEFAULT"]["default_publisher_id"]; id != "" {
		n.PublisherID = id
	}
	for _, d := range f.list("DEFAULT", "disabled_notifications") {
		n.Disabled[d] = true
	}
	for _, name := range append(append([]string{n.PublisherID}, n.Topics...), f.list("DEFAULT", "disabled_notifications")...) {
		if !notificationName.MatchString(name) {
			return errors.New("invalid notification topic, publisher or disabled name in glance-api.conf")
		}
	}
	c.Notifications = n
	if err := readLimits(c, f, env); err != nil {
		return err
	}
	if err := readImport(c, f, env); err != nil {
		return err
	}
	if err := readCache(c, f); err != nil {
		return err
	}
	c.MemberQuota, c.PropertyQuota, c.TagQuota = 128, 128, 128
	for key, dst := range map[string]*int{"image_member_quota": &c.MemberQuota, "image_property_quota": &c.PropertyQuota, "image_tag_quota": &c.TagQuota} {
		if v := f.get("DEFAULT", key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < -1 {
				return errors.New("invalid [DEFAULT] " + key)
			}
			*dst = n
		}
	}
	if v := firstOf(f.get("DEFAULT", "user_storage_quota"), "0"); v != "0" {
		m := storageQuota.FindStringSubmatch(v)
		if m == nil {
			return errors.New("Invalid value for option user_storage_quota: " + v)
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		c.UserStorageQuota = n << map[string]uint{"": 0, "B": 0, "KB": 10, "MB": 20, "GB": 30, "TB": 40}[m[2]]
	}
	for key, dst := range map[string]*bool{"show_image_direct_url": &c.ShowDirectURL, "show_multiple_locations": &c.ShowMultipleLocations} {
		b, ok := osloBool(f.get("DEFAULT", key))
		if !ok {
			return errors.New("invalid [DEFAULT] " + key)
		}
		*dst = b
	}
	c.PropertyProtectionFile, c.PropertyProtectionFormat = f.get("DEFAULT", "property_protection_file"), firstOf(f.get("DEFAULT", "property_protection_rule_format"), "roles")
	if c.PropertyProtectionFile != "" && !strings.HasPrefix(c.PropertyProtectionFile, "/") {
		c.PropertyProtectionFile = filepath.Join(filepath.Dir(env("GLANCE_GO_CONFIG_FILE")), c.PropertyProtectionFile)
	}
	c.PolicyFile = firstOf(f.get("oslo_policy", "policy_file"), "policy.yaml")
	if !strings.HasPrefix(c.PolicyFile, "/") {
		c.PolicyFile = filepath.Join(filepath.Dir(env("GLANCE_GO_CONFIG_FILE")), c.PolicyFile)
	}
	// image_size_cap (1 TiB by default) bounds uploads, unless
	// GLANCE_GO_MAX_UPLOAD_BYTES sets the bound explicitly.
	if env("GLANCE_GO_MAX_UPLOAD_BYTES") == "" {
		c.Binary.MaxUploadBytes = 1 << 40
		if v := f.get("DEFAULT", "image_size_cap"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || n > 1<<40 {
				return errors.New("invalid [DEFAULT] image_size_cap (1..1099511627776)")
			}
			c.Binary.MaxUploadBytes = n
		}
	}
	c.ImageFormat = ImageFormatSettings{RequireMatch: true, GPTNonfatal: f.list("image_format", "gpt_safety_checks_nonfatal")}
	if v := f.get("image_format", "require_image_format_match"); v != "" {
		b, ok := osloBool(v)
		if !ok {
			return errors.New("invalid [image_format] require_image_format_match")
		}
		c.ImageFormat.RequireMatch = b
	}
	k := func(key string) string { return f["keystone_authtoken"][key] }
	if t := k("auth_type"); t != "" && t != "password" {
		return errors.New("only [keystone_authtoken] auth_type = password is supported")
	}
	c.KeystoneURL = strings.TrimRight(k("auth_url"), "/")
	if !strings.HasSuffix(c.KeystoneURL, "/v3") {
		c.KeystoneURL += "/v3"
	}
	if err := ValidateURL(c.KeystoneURL); err != nil {
		return errors.New("invalid [keystone_authtoken] auth_url")
	}
	c.Credential = ServiceCredential{Username: k("username"), Password: k("password"), ProjectName: k("project_name"),
		UserDomainName: firstOf(k("user_domain_name"), "Default"), ProjectDomainName: firstOf(k("project_domain_name"), "Default")}
	if c.Credential.Username == "" || c.Credential.Password == "" || c.Credential.ProjectName == "" {
		return errors.New("[keystone_authtoken] needs username, password and project_name")
	}
	return nil
}

// osloBool parses an oslo.config boolean (true/false, yes/no, on/off, 1/0).
func osloBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0", "":
		return false, true
	}
	return false, false
}

// readLimits: [DEFAULT] use_keystone_limits and [oslo_limit], as oslo.limit
// (password auth; endpoint_id names the Glance endpoint).
func readLimits(c *Config, f osloFile, env func(string) string) error {
	enabled, ok := osloBool(f.get("DEFAULT", "use_keystone_limits"))
	if !ok {
		return errors.New("invalid [DEFAULT] use_keystone_limits")
	}
	if !enabled {
		return nil
	}
	l := func(key string) string { return f["oslo_limit"][key] }
	if t := l("auth_type"); t != "" && t != "password" {
		return errors.New("only [oslo_limit] auth_type = password is supported")
	}
	url := strings.TrimRight(l("auth_url"), "/")
	if !strings.HasSuffix(url, "/v3") {
		url += "/v3"
	}
	if err := ValidateURL(url); err != nil {
		return errors.New("invalid [oslo_limit] auth_url")
	}
	c.Limits = KeystoneLimits{Enabled: true, URL: url, EndpointID: l("endpoint_id"), Username: l("username"), Password: l("password"),
		UserDomainName: firstOf(l("user_domain_name"), "Default"), SystemScope: l("system_scope"), ProjectName: l("project_name"),
		ProjectDomainName: firstOf(l("project_domain_name"), "Default"), Region: l("endpoint_region_name"), CacheTTL: 5 * time.Second}
	if v := env("GLANCE_GO_LIMITS_CACHE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 3600 {
			return errors.New("GLANCE_GO_LIMITS_CACHE_SECONDS must be 0-3600")
		}
		c.Limits.CacheTTL = time.Duration(n) * time.Second
	}
	if c.Limits.EndpointID == "" || c.Limits.Username == "" || c.Limits.Password == "" || c.Limits.SystemScope == "" && c.Limits.ProjectName == "" {
		return errors.New("[oslo_limit] needs endpoint_id, username, password and system_scope or project_name")
	}
	if c.Limits.SystemScope != "" && c.Limits.SystemScope != "all" {
		return errors.New("[oslo_limit] system_scope must be all")
	}
	return nil
}

// readImport: the original's import options and defaults.
func readImport(c *Config, f osloFile, env func(string) string) error {
	staging := firstOf(f.get("DEFAULT", "node_staging_uri"), "file:///tmp/staging/")
	if len(c.Backends) > 0 { // multi-store: the reserved staging store
		if d := f.get("os_glance_staging_store", "filesystem_store_datadir"); d != "" {
			staging = "file://" + d
		}
	}
	dir, ok := strings.CutPrefix(staging, "file://")
	dir = strings.TrimRight(dir, "/")
	if !ok || dir == "" || dir[0] != '/' {
		return errors.New("invalid [DEFAULT] node_staging_uri: only file:// is supported")
	}
	methods := f.list("DEFAULT", "enabled_import_methods")
	if len(methods) == 0 {
		methods = []string{"glance-direct", "web-download", "copy-image"}
	}
	c.Import = ImportSettings{StagingDir: dir, TaskTimeToLive: 48 * time.Hour,
		AllowedSchemes: []string{"http", "https"}, AllowedPorts: []int{80, 443}}
	for _, m := range methods {
		m = strings.Trim(strings.TrimSpace(m), "'\"[]")
		switch m {
		// copy-image is advertised as the original does, though it needs
		// several stores to be used.
		case "glance-direct", "web-download", "glance-download", "copy-image":
			c.Import.Methods = append(c.Import.Methods, m)
		default:
			if m != "" {
				return errors.New("invalid [DEFAULT] enabled_import_methods")
			}
		}
	}
	if v := f.get("task", "task_time_to_live"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return errors.New("invalid [task] task_time_to_live")
		}
		c.Import.TaskTimeToLive = time.Duration(n) * time.Hour
	}
	o := func(key string) ([]string, bool) {
		if _, ok := f["import_filtering_opts"][key]; !ok {
			return nil, false
		}
		return f.list("import_filtering_opts", key), true
	}
	if v, ok := o("allowed_schemes"); ok {
		c.Import.AllowedSchemes = v
	}
	c.Import.DisallowedSchemes, _ = o("disallowed_schemes")
	c.Import.AllowedHosts, _ = o("allowed_hosts")
	c.Import.DisallowedHosts, _ = o("disallowed_hosts")
	ports := func(key string, dst *[]int) error {
		v, ok := o(key)
		if !ok {
			return nil
		}
		*dst = nil
		for _, p := range v {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("invalid [import_filtering_opts] " + key)
			}
			*dst = append(*dst, n)
		}
		return nil
	}
	// glance-image-import.conf, next to glance-api.conf, holds the plugins.
	imp := f
	if conf := env("GLANCE_GO_CONFIG_FILE"); conf != "" {
		if extra, err := readOriginalConfig(filepath.Join(filepath.Dir(conf), "glance-image-import.conf")); err == nil {
			imp = osloFile{}
			for _, src := range []osloFile{f, extra} {
				for sec, vals := range src {
					if imp[sec] == nil {
						imp[sec] = map[string]string{}
					}
					for k, v := range vals {
						imp[sec][k] = v
					}
				}
			}
		}
	}
	for _, p := range imp.list("image_import_opts", "image_import_plugins") {
		if p = strings.Trim(strings.TrimSpace(p), "'\"[]"); p != "" {
			switch p {
			case "no_op", "image_conversion", "inject_image_metadata", "image_decompression":
				c.Import.Plugins = append(c.Import.Plugins, p)
			default:
				return errors.New("unknown image import plugin " + p)
			}
		}
	}
	c.Import.ConversionFormat = firstOf(imp.get("image_conversion", "output_format"), "raw")
	if c.Import.ConversionFormat != "raw" && c.Import.ConversionFormat != "qcow2" && c.Import.ConversionFormat != "vmdk" {
		return errors.New("invalid [image_conversion] output_format")
	}
	c.Import.InjectIgnore = imp.list("inject_metadata_properties", "ignore_user_roles")
	if _, set := imp["inject_metadata_properties"]["ignore_user_roles"]; !set {
		c.Import.InjectIgnore = []string{"admin"}
	}
	c.Import.Inject = map[string]string{}
	for _, kv := range imp.list("inject_metadata_properties", "inject") {
		if k, v, ok := strings.Cut(kv, ":"); ok {
			c.Import.Inject[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	c.Import.GPTNonfatal = imp.list("image_format", "gpt_safety_checks_nonfatal")
	c.Import.VMDKAllowed = imp.list("image_format", "vmdk_allowed_types")
	if _, set := imp["image_format"]["vmdk_allowed_types"]; !set {
		c.Import.VMDKAllowed = []string{"streamOptimized", "monolithicSparse"}
	}
	if err := ports("allowed_ports", &c.Import.AllowedPorts); err != nil {
		return err
	}
	return ports("disallowed_ports", &c.Import.DisallowedPorts)
}

// readCache: the cache is on when the paste flavor includes it (keystone+cache
// or keystone+cachemanagement), as the original's pipelines.
func readCache(c *Config, f osloFile) error {
	flavor := f.get("paste_deploy", "flavor")
	if !strings.Contains(flavor, "cache") {
		return nil
	}
	dir := strings.TrimRight(f.get("DEFAULT", "image_cache_dir"), "/")
	if dir == "" || dir[0] != '/' {
		return errors.New("the image cache needs an absolute [DEFAULT] image_cache_dir")
	}
	if d := firstOf(f.get("DEFAULT", "image_cache_driver"), "centralized_db"); d != "centralized_db" {
		return errors.New("only image_cache_driver = centralized_db is supported")
	}
	c.Cache = CacheSettings{Enabled: true, Dir: dir, NodeURL: strings.TrimRight(f.get("DEFAULT", "worker_self_reference_url"), "/"),
		PrefetchInterval: 300 * time.Second, MaxSize: 10 << 30}
	if v := f.get("DEFAULT", "cache_prefetcher_interval"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New("invalid [DEFAULT] cache_prefetcher_interval")
		}
		c.Cache.PrefetchInterval = time.Duration(n) * time.Second
	}
	if v := f.get("DEFAULT", "image_cache_max_size"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return errors.New("invalid [DEFAULT] image_cache_max_size")
		}
		c.Cache.MaxSize = n
	}
	if c.Cache.NodeURL == "" {
		c.Cache.NodeURL = c.PublicURL
	}
	return nil
}

var storageQuota = regexp.MustCompile(`^(\d+)((K|M|G|T)?B)?$`)

func firstOf(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
