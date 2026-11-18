package config

import (
	"strings"
	"testing"
	"time"
)

func TestBinaryConfiguration(t *testing.T) {
	base := func() map[string]string {
		e := environment(t)
		e["GLANCE_GO_BINARY_ENABLED"] = "true"
		e["GLANCE_GO_STORE_ROOT"] = "/private/glance"
		e["GLANCE_GO_STORE_SCOPE"] = "00000000-0000-5000-8000-000000000001"
		return e
	}
	e := base()
	c, err := Load(func(k string) string { return e[k] }, "serve")
	if err != nil || !c.Binary.Enabled || c.Binary.MaxUploadBytes != 1<<30 || c.Binary.MaxConcurrent != 16 || c.Binary.UploadTimeout != 10*time.Minute {
		t.Fatal("binary defaults", err)
	}
	for key, values := range map[string][]string{
		"GLANCE_GO_BINARY_ENABLED": {"1", "TRUE"}, "GLANCE_GO_STORE_ROOT": {"relative", "/", "/a/../b"}, "GLANCE_GO_STORE_SCOPE": {"bad", "00000000-0000-0000-0000-000000000000"},
		"GLANCE_GO_MAX_UPLOAD_BYTES": {"0", "-1", "1099511627777", "bad"}, "GLANCE_GO_MAX_CONCURRENT_TRANSFERS": {"0", "65", "bad"},
		"GLANCE_GO_UPLOAD_TIMEOUT": {"0s", "61m", "1s"}, "GLANCE_GO_DOWNLOAD_TIMEOUT": {"0s", "61m", "1s"}, "GLANCE_GO_RECOVERY_TIMEOUT": {"0s", "11m"}, "GLANCE_GO_RESOLUTION_TIMEOUT": {"0s", "61s"}, "GLANCE_GO_SHUTDOWN_TIMEOUT": {"0s", "6m"},
	} {
		for _, value := range values {
			t.Run(key+value, func(t *testing.T) {
				env := base()
				env[key] = value
				if _, err := Load(func(k string) string { return env[k] }, "serve"); err == nil {
					t.Fatal("accepted unsafe config")
				}
			})
		}
	}
	t.Run("explicit enable", func(t *testing.T) {
		env := base()
		env["GLANCE_GO_BINARY_ENABLED"] = "false"
		if _, err := Load(func(k string) string { return env[k] }, "serve"); err == nil {
			t.Fatal("silently ignored store")
		}
	})
	for _, mode := range []string{"store-scope", "store-init", "migrate"} {
		t.Run(mode, func(t *testing.T) {
			env := base()
			delete(env, "GLANCE_GO_KEYSTONE_AUTH_URL")
			delete(env, "GLANCE_GO_KEYSTONE_CREDENTIAL_FILE")
			if _, err := Load(func(k string) string { return env[k] }, mode); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("duration upper bounds", func(t *testing.T) {
		env := base()
		env["GLANCE_GO_UPLOAD_TIMEOUT"] = "1h"
		env["GLANCE_GO_DOWNLOAD_TIMEOUT"] = "1h"
		env["GLANCE_GO_RECOVERY_TIMEOUT"] = "10m"
		env["GLANCE_GO_RESOLUTION_TIMEOUT"] = "1m"
		env["GLANCE_GO_SHUTDOWN_TIMEOUT"] = "5m"
		c, err := Load(func(k string) string { return env[k] }, "serve")
		if err != nil || c.Binary.DownloadTimeout != time.Hour {
			t.Fatal(err)
		}
	})
	t.Run("sanitized", func(t *testing.T) {
		env := base()
		env["GLANCE_GO_STORE_SCOPE"] = "private-secret"
		_, err := Load(func(k string) string { return env[k] }, "serve")
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("unsafe diagnostics")
		}
	})
}

func TestRBDBackendConfiguration(t *testing.T) {
	base := func() map[string]string {
		e := environment(t)
		e["GLANCE_GO_BINARY_ENABLED"] = "true"
		e["GLANCE_GO_STORE_BACKEND"] = "rbd"
		e["GLANCE_GO_STORE_SCOPE"] = "00000000-0000-5000-8000-000000000001"
		e["GLANCE_GO_RBD_CONF"] = "/etc/ceph/ceph.conf"
		e["GLANCE_GO_RBD_USER"] = "glance"
		e["GLANCE_GO_RBD_POOL"] = "images"
		return e
	}
	e := base()
	c, err := Load(func(k string) string { return e[k] }, "serve")
	if err != nil || c.Binary.Backend != "rbd" || c.Binary.RBDPool != "images" || c.Binary.Root != "" {
		t.Fatal("rbd backend", err)
	}
	for key, values := range map[string][]string{
		"GLANCE_GO_STORE_BACKEND": {"ceph", "RBD"},
		"GLANCE_GO_STORE_ROOT":    {"/private/glance"}, // ambiguous: both backends named
		"GLANCE_GO_RBD_CONF":      {"", "relative.conf", "/etc/ceph/../ceph.conf"},
		"GLANCE_GO_RBD_USER":      {"", "client glance", "a/b"},
		"GLANCE_GO_RBD_POOL":      {"", "images;rm", strings.Repeat("p", 65)},
		"GLANCE_GO_STORE_SCOPE":   {"bad"},
	} {
		for _, value := range values {
			t.Run(key+"="+value, func(t *testing.T) {
				env := base()
				env[key] = value
				if _, err := Load(func(k string) string { return env[k] }, "serve"); err == nil {
					t.Fatal("accepted unsafe rbd config")
				}
			})
		}
	}
	t.Run("filesystem stays the default", func(t *testing.T) {
		env := base()
		delete(env, "GLANCE_GO_STORE_BACKEND")
		for _, k := range []string{"GLANCE_GO_RBD_CONF", "GLANCE_GO_RBD_USER", "GLANCE_GO_RBD_POOL"} {
			delete(env, k)
		}
		env["GLANCE_GO_STORE_ROOT"] = "/private/glance"
		c, err := Load(func(k string) string { return env[k] }, "serve")
		if err != nil || c.Binary.Backend != "filesystem" {
			t.Fatal("default backend", err)
		}
	})
	t.Run("rbd pool needs the explicit switch", func(t *testing.T) {
		env := base()
		env["GLANCE_GO_BINARY_ENABLED"] = "false"
		delete(env, "GLANCE_GO_STORE_SCOPE")
		if _, err := Load(func(k string) string { return env[k] }, "serve"); err == nil {
			t.Fatal("silently ignored rbd store")
		}
	})
}
