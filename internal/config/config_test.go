package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func secretFile(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func environment(t *testing.T) map[string]string {
	return map[string]string{"GLANCE_GO_DATABASE_DSN_FILE": secretFile(t, "glance_go_app:example@tcp(127.0.0.1:3306)/glance_go"), "GLANCE_GO_KEYSTONE_AUTH_URL": "http://127.0.0.1:5000/v3", "GLANCE_GO_KEYSTONE_CREDENTIAL_FILE": secretFile(t, `{"application_credential_id":"fixture-id","application_credential_secret":"fixture-secret"}`)}
}

func TestConfigDefaultsAndModes(t *testing.T) {
	env := environment(t)
	c, err := Load(func(k string) string { return env[k] }, "serve")
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddress != "127.0.0.1:9293" || c.PublicURL != "http://127.0.0.1:9293" || c.MaxBodyBytes != 1048576 || c.DependencyTimeout != 5*time.Second || c.Credential.ID != "fixture-id" {
		t.Fatal("unexpected defaults")
	}
	delete(env, "GLANCE_GO_KEYSTONE_AUTH_URL")
	delete(env, "GLANCE_GO_KEYSTONE_CREDENTIAL_FILE")
	if _, err = Load(func(k string) string { return env[k] }, "migrate"); err != nil {
		t.Fatal("migration must not require Keystone:", err)
	}
	if _, err = Load(func(k string) string { return env[k] }, "serve"); err == nil {
		t.Fatal("serve accepted missing auth")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"GLANCE_GO_LISTEN_ADDR", ":9293"}, {"GLANCE_GO_LISTEN_ADDR", "127.0.0.1:0"},
		{"GLANCE_GO_PUBLIC_BASE_URL", "http://user:password@localhost"}, {"GLANCE_GO_PUBLIC_BASE_URL", "http://localhost/?x=1"},
		{"GLANCE_GO_PUBLIC_BASE_URL", "/relative"}, {"GLANCE_GO_PUBLIC_BASE_URL", "http://localhost/a/../b"},
		{"GLANCE_GO_KEYSTONE_AUTH_URL", "http://localhost/identity"}, {"GLANCE_GO_MAX_BODY_BYTES", "0"},
		{"GLANCE_GO_REQUEST_TIMEOUT", "-1s"}, {"GLANCE_GO_DEPENDENCY_TIMEOUT", "20s"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			env := environment(t)
			env[tc.key] = tc.value
			_, err := Load(func(k string) string { return env[k] }, "serve")
			if err == nil {
				t.Fatal("accepted invalid configuration")
			}
			if strings.Contains(err.Error(), "password@") {
				t.Fatal("credential leaked")
			}
		})
	}
	if _, err := Load(func(string) string { return "" }, "unknown"); err == nil {
		t.Fatal("unknown mode")
	}
}

func TestPrivateFiles(t *testing.T) {
	p := secretFile(t, "a-private-value")
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("accepted public secret")
	}
	if _, err := ReadSecret(t.TempDir()); err == nil {
		t.Fatal("accepted directory")
	}
	env := environment(t)
	env["GLANCE_GO_KEYSTONE_CREDENTIAL_FILE"] = secretFile(t, `{"application_credential_secret":"NEVER-LOG-THIS"`)
	_, err := Load(func(k string) string { return env[k] }, "serve")
	if err == nil || strings.Contains(err.Error(), "NEVER-LOG-THIS") {
		t.Fatal("unsafe credential error")
	}
}

func TestURLSeparatorsAndBasePaths(t *testing.T) {
	for _, value := range []string{
		"http://example.invalid?", "http://example.invalid/#", "http://example.invalid?x=1", "http://example.invalid/#frag",
		"http://example.invalid/?/", "http://example.invalid/#/", "http://example.invalid/a//b", "http://example.invalid/a/./b",
		"http://example.invalid/%2e%2e/b", "http://example.invalid/%3f", "http://example.invalid:0", "http://example.invalid:65536",
		"http://example.invalid:", "ftp://example.invalid", "https:///image", "http://bad!host", "http://[:::]", "http://user:secret@example.invalid",
	} {
		t.Run(value, func(t *testing.T) {
			env := environment(t)
			env["GLANCE_GO_PUBLIC_BASE_URL"] = value
			if _, err := Load(func(k string) string { return env[k] }, "serve"); err == nil {
				t.Fatal("ambiguous public URL accepted")
			}
		})
	}
	for _, value := range []string{"http://127.0.0.1:9293", "https://images.example.invalid/image/", "https://images.example.invalid/image///", "http://[::1]:9293/"} {
		t.Run(value, func(t *testing.T) {
			env := environment(t)
			env["GLANCE_GO_PUBLIC_BASE_URL"] = value
			c, err := Load(func(k string) string { return env[k] }, "serve")
			if err != nil || c.PublicURL != strings.TrimRight(value, "/") {
				t.Fatalf("base URL: %v", err)
			}
		})
	}
}

func TestPlacementNamespaceIsIndependent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "placement.dsn")
	if err := os.WriteFile(dsn, []byte("placement_go_app:fixture@tcp(127.0.0.1:1)/placement_go"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PLACEMENT_GO_DATABASE_DSN_FILE": dsn, "GLANCE_GO_LISTEN_ADDR": "0.0.0.0:9292", "GLANCE_GO_DATABASE_DSN_FILE": "/not-used"}
	c, err := LoadPlacement(func(k string) string { return env[k] }, "migrate")
	if err != nil || c.ListenAddress != "127.0.0.1:8779" || c.PublicURL != "http://127.0.0.1:8779" {
		t.Fatal(c.ListenAddress, err)
	}
	env["PLACEMENT_GO_LISTEN_ADDR"] = "127.0.0.1:18779"
	env["PLACEMENT_GO_PUBLIC_BASE_URL"] = "http://localhost/placement"
	c, err = LoadPlacement(func(k string) string { return env[k] }, "migrate")
	if err != nil || c.ListenAddress != "127.0.0.1:18779" || c.PublicURL != "http://localhost/placement" {
		t.Fatal(err)
	}
	delete(env, "PLACEMENT_GO_DATABASE_DSN_FILE")
	if _, err = LoadPlacement(func(k string) string { return env[k] }, "migrate"); err == nil {
		t.Fatal("Glance secret used for Placement")
	}
}

// keystonemiddleware's token_cache_time: 300 s by default, -1 disables.
func TestGlanceTokenCacheTime(t *testing.T) {
	e := environment(t)
	c, err := Load(func(k string) string { return e[k] }, "serve")
	if err != nil || c.TokenCacheTime != 300*time.Second {
		t.Fatal("default token cache time", c.TokenCacheTime, err)
	}
	for value, want := range map[string]time.Duration{"-1": 0, "1": time.Second, "600": 10 * time.Minute} {
		e["GLANCE_GO_TOKEN_CACHE_TIME"] = value
		if c, err = Load(func(k string) string { return e[k] }, "serve"); err != nil || c.TokenCacheTime != want {
			t.Fatal(value, c.TokenCacheTime, err)
		}
	}
	for _, bad := range []string{"0", "-2", "86401", "5m", "x"} {
		e["GLANCE_GO_TOKEN_CACHE_TIME"] = bad
		if _, err = Load(func(k string) string { return e[k] }, "serve"); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
