package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const devstackConf = `[DEFAULT]
public_endpoint = http://192.168.122.10/image
transport_url = rabbit://stackrabbit:secret@192.168.122.10:5672/
; comment
[database]
connection = mysql+pymysql://root:p%40ss@127.0.0.1/glance?charset=utf8&plugin=dbcounter
[keystone_authtoken]
auth_url = http://192.168.122.10/identity
auth_type = password
username = glance
password = svc
project_name = service
user_domain_name = Default
project_domain_name = Default
[oslo_messaging_notifications]
driver = messagingv2
[glance_store]
filesystem_store_datadir = /opt/stack/data/glance/images/
`

func originalConf(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "glance-api.conf")
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOriginalGlanceAPIConfIsReadLikeTheOriginal(t *testing.T) {
	env := map[string]string{"GLANCE_GO_CONFIG_FILE": originalConf(t, devstackConf, 0644)}
	c, err := Load(func(k string) string { return env[k] }, "serve")
	if err != nil {
		t.Fatal(err)
	}
	if c.OfficialCatalog != "glance" || c.Binary.OfficialStoreDir != "/opt/stack/data/glance/images" {
		t.Fatal("catalog", c.OfficialCatalog, c.Binary.OfficialStoreDir)
	}
	if !strings.HasPrefix(c.DatabaseDSN, "root:p@ss@tcp(127.0.0.1:3306)/glance_go_shared") {
		t.Fatal("same account on the shared database expected, got", c.DatabaseDSN)
	}
	if c.KeystoneURL != "http://192.168.122.10/identity/v3" || c.Credential.Username != "glance" || c.Credential.ProjectName != "service" || c.Credential.ID != "" {
		t.Fatal("keystone", c.KeystoneURL, c.Credential.Username)
	}
	if c.Notifications.Driver != "messagingv2" || c.Notifications.TransportURL != "rabbit://stackrabbit:secret@192.168.122.10:5672/" || c.Notifications.Topics[0] != "notifications" {
		t.Fatal("notifications", c.Notifications)
	}
	if c.PublicURL != "http://192.168.122.10/image" || c.TokenCacheTime != 300*time.Second {
		t.Fatal(c.PublicURL, c.TokenCacheTime)
	}
}

func TestOriginalGlanceAPIConfRefusesAmbiguityAndUnsafeFiles(t *testing.T) {
	path := originalConf(t, devstackConf, 0644)
	for _, extra := range []map[string]string{
		{"GLANCE_GO_DATABASE_DSN_FILE": "/x"},
		{"GLANCE_GO_KEYSTONE_AUTH_URL": "http://k/v3"},
		{"GLANCE_GO_NOTIFICATION_DRIVER": "noop"},
	} {
		extra["GLANCE_GO_CONFIG_FILE"] = path
		if _, err := Load(func(k string) string { return extra[k] }, "serve"); err == nil {
			t.Fatalf("accepted %v", extra)
		}
	}
	writable := originalConf(t, devstackConf, 0666)
	if _, err := Load(func(k string) string { return map[string]string{"GLANCE_GO_CONFIG_FILE": writable}[k] }, "serve"); err == nil {
		t.Fatal("world-writable glance-api.conf accepted")
	}
	noCatalog := originalConf(t, strings.Replace(devstackConf, "/glance?", "/nova?", 1), 0644)
	if _, err := Load(func(k string) string { return map[string]string{"GLANCE_GO_CONFIG_FILE": noCatalog}[k] }, "serve"); err == nil {
		t.Fatal("a non-glance database accepted")
	}
}

func TestOsloParserMergesSectionsAndLastValueWins(t *testing.T) {
	f, err := parseOslo("[DEFAULT]\nA = 1\n[x]\nb = 2\n[x]\nB = 3\n")
	if err != nil || f.get("x", "b") != "3" || f.get("x", "a") != "1" {
		t.Fatal(f, err)
	}
	if _, err = parseOslo("key = value\n"); err == nil {
		t.Fatal("option outside a section accepted")
	}
}

// The original's token_cache_time must win over the built-in 300 s default.
func TestOriginalTokenCacheTimeOverridesDefault(t *testing.T) {
	conf := strings.Replace(devstackConf, "project_domain_name = Default\n", "project_domain_name = Default\ntoken_cache_time = 60\n", 1)
	env := map[string]string{"GLANCE_GO_CONFIG_FILE": originalConf(t, conf, 0644)}
	c, err := Load(func(k string) string { return env[k] }, "serve")
	if err != nil || c.TokenCacheTime != time.Minute {
		t.Fatal(c.TokenCacheTime, err)
	}
}

func TestOriginalKeystoneLimits(t *testing.T) {
	load := func(conf string) (Config, error) {
		env := map[string]string{"GLANCE_GO_CONFIG_FILE": originalConf(t, conf, 0644)}
		return Load(func(k string) string { return env[k] }, "serve")
	}
	c, err := load(devstackConf)
	if err != nil || c.Limits.Enabled {
		t.Fatal("limits enabled without use_keystone_limits", err)
	}
	// As DevStack writes it.
	limits := strings.Replace(devstackConf, "[DEFAULT]\n", "[DEFAULT]\nuse_keystone_limits = True\n", 1) +
		"[oslo_limit]\nendpoint_id = 9520004904bb4bbbacaa6349bbc82816\nsystem_scope = all\nauth_url = http://192.168.122.10/identity\nauth_type = password\nusername = glance\npassword = svc\nuser_domain_name = Default\n"
	c, err = load(limits)
	if err != nil || !c.Limits.Enabled || c.Limits.URL != "http://192.168.122.10/identity/v3" || c.Limits.EndpointID != "9520004904bb4bbbacaa6349bbc82816" || c.Limits.SystemScope != "all" || c.Limits.Username != "glance" {
		t.Fatal(c.Limits, err)
	}
	for _, bad := range []string{
		strings.Replace(limits, "endpoint_id = 9520004904bb4bbbacaa6349bbc82816\n", "", 1),
		strings.Replace(limits, "system_scope = all", "system_scope = domain", 1),
		strings.Replace(limits, "use_keystone_limits = True", "use_keystone_limits = maybe", 1),
	} {
		if _, err = load(bad); err == nil {
			t.Fatal("invalid [oslo_limit] accepted")
		}
	}
}

func TestOriginalImageSizeCapAndQuotas(t *testing.T) {
	conf := strings.Replace(devstackConf, "[DEFAULT]\n", "[DEFAULT]\nimage_size_cap = 5368709120\nimage_member_quota = 5\nimage_tag_quota = -1\n", 1)
	env := map[string]string{"GLANCE_GO_CONFIG_FILE": originalConf(t, conf, 0644)}
	c, err := Load(func(k string) string { return env[k] }, "serve")
	if err != nil || c.Binary.MaxUploadBytes != 5<<30 || c.MemberQuota != 5 || c.TagQuota != -1 || c.PropertyQuota != 128 || !c.ImageFormat.RequireMatch {
		t.Fatal(c.Binary.MaxUploadBytes, c.MemberQuota, c.TagQuota, c.PropertyQuota, err)
	}
	env["GLANCE_GO_MAX_UPLOAD_BYTES"] = "1024"
	if c, err = Load(func(k string) string { return env[k] }, "serve"); err != nil || c.Binary.MaxUploadBytes != 1024 {
		t.Fatal("explicit bound ignored", c.Binary.MaxUploadBytes, err)
	}
}

func TestOriginalEnabledBackendsAndDelayedDelete(t *testing.T) {
	conf := strings.Replace(devstackConf, "[DEFAULT]\n", "[DEFAULT]\nenabled_backends = fast:file, cheap:file\ndelayed_delete = True\n", 1)
	conf = strings.Replace(conf, "[glance_store]\n", "[glance_store]\ndefault_backend = fast\n", 1)
	conf += "[fast]\nfilesystem_store_datadir = /opt/stack/data/glance/fast/\nstore_description = Fast\n[cheap]\nfilesystem_store_datadir = /opt/stack/data/glance/cheap\nweight = 5\n[os_glance_staging_store]\nfilesystem_store_datadir = /opt/stack/data/glance/os_glance_staging_store\n"
	env := map[string]string{"GLANCE_GO_CONFIG_FILE": originalConf(t, conf, 0644)}
	c, err := Load(func(k string) string { return env[k] }, "serve")
	if err != nil {
		t.Fatal(err)
	}
	if !c.DelayedDelete || c.DefaultBackend != "fast" || len(c.Backends) != 2 || c.Backends[0].Dir != "/opt/stack/data/glance/fast" || c.Backends[0].Description != "Fast" || c.Backends[1].Weight != 5 {
		t.Fatalf("%+v %v", c.Backends, c.DelayedDelete)
	}
	if c.Binary.OfficialStoreDir != "/opt/stack/data/glance/fast" || c.Import.StagingDir != "/opt/stack/data/glance/os_glance_staging_store" {
		t.Fatal("default store / staging", c.Binary.OfficialStoreDir, c.Import.StagingDir)
	}
	bad := strings.Replace(conf, "cheap:file", "cheap:rbd", 1)
	env["GLANCE_GO_CONFIG_FILE"] = originalConf(t, bad, 0644)
	if _, err = Load(func(k string) string { return env[k] }, "serve"); err == nil {
		t.Fatal("non-file backend accepted")
	}
}
