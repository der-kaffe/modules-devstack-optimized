package config

import (
	"errors"
	"net"
	"net/url"
	"strings"

	driver "github.com/go-sql-driver/mysql"
)

// ToolSettings is what the original's command-line tools (glance-manage,
// glance-cache-*, glance-scrubber) read from their --config-file list.
type ToolSettings struct {
	CatalogDSN   string // the [database] connection's own database
	Catalog      string
	MetadataPath string // [DEFAULT] metadata_source_path
	StoreDir     string
	Cache        CacheSettings
	StallTime    int // image_cache_stall_time (seconds)
	files        osloFile
}

// Get returns a raw option of the merged configuration.
func (t ToolSettings) Get(section, key string) string { return t.files.get(section, key) }

// LoadTool reads and merges the configuration files as oslo.config does
// (later files win). With none it uses /etc/glance/glance-api.conf.
func LoadTool(paths []string) (ToolSettings, error) {
	if len(paths) == 0 {
		paths = []string{"/etc/glance/glance-api.conf"}
	}
	merged := osloFile{}
	for _, p := range paths {
		f, err := readOriginalConfig(p)
		if err != nil {
			return ToolSettings{}, err
		}
		for section, values := range f {
			if merged[section] == nil {
				merged[section] = map[string]string{}
			}
			for k, v := range values {
				merged[section][k] = v
			}
		}
	}
	t := ToolSettings{files: merged, MetadataPath: firstOf(merged.get("DEFAULT", "metadata_source_path"), "/etc/glance/metadefs/"), StallTime: 86400}
	if c := merged.get("database", "connection"); c != "" {
		u, err := url.Parse(c)
		if err != nil || !strings.HasPrefix(u.Scheme, "mysql") || u.User == nil || u.Hostname() == "" {
			return t, errors.New("the [database] connection must be a MySQL URL")
		}
		password, _ := u.User.Password()
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		d := driver.NewConfig()
		t.Catalog = strings.TrimPrefix(u.Path, "/")
		d.User, d.Passwd, d.Net, d.Addr, d.DBName = u.User.Username(), password, "tcp", net.JoinHostPort(u.Hostname(), port), t.Catalog
		d.ParseTime = true
		t.CatalogDSN = d.FormatDSN()
	}
	t.StoreDir = strings.TrimRight(merged.get("glance_store", "filesystem_store_datadir"), "/")
	c := &Config{}
	if err := readCache(c, merged); err == nil {
		t.Cache = c.Cache
	}
	if dir := strings.TrimRight(merged.get("DEFAULT", "image_cache_dir"), "/"); dir != "" {
		t.Cache.Dir = dir
		if t.Cache.NodeURL == "" {
			t.Cache.NodeURL = strings.TrimRight(merged.get("DEFAULT", "worker_self_reference_url"), "/")
		}
	}
	return t, nil
}
