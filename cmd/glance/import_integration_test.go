//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/api"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	driver "github.com/go-sql-driver/mysql"
)

// importIdentity: a member and an admin of the same project.
type importIdentity struct{}

func (importIdentity) Authenticate(_ context.Context, token string) (auth.Identity, error) {
	roles := map[string][]string{"member-token": {"reader", "member"}, "admin-token": {"reader", "member", "admin"}, "service-token": {"reader", "service"}}[token]
	if roles == nil {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return auth.Identity{UserID: "import-user", ProjectID: "import-project", Roles: roles, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// MUTATING, opt-in: interoperable import (stage + glance-direct,
// web-download) and the tasks API over MySQL (copy of the original's DDL)
// and the filesystem store, through HTTP; then recovery of an open task.
func TestImportMySQLFilesystemHTTP(t *testing.T) {
	if os.Getenv("GLANCE_GO_B25_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable integration authorization required")
	}
	secret, err := config.ReadSecret(os.Getenv("GLANCE_GO_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(secret))
	if err = mysql.ValidateTestDSN(dsn); err != nil {
		t.Fatal(err)
	}
	parsed, _ := driver.ParseDSN(dsn)
	official := "glance_official_test_" + strings.TrimPrefix(parsed.DBName, "glance_go_shared_test_")
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rs, err := databaseScope(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("GLANCE_GO_B25_TEST_ROOT")
	images := sharedImages(t, root)
	staging := filepath.Clean(root) + "-staging"
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(root, rs); err != nil {
		t.Fatal(err)
	}
	// The web-download source: a local HTTP server, allowed by the filter.
	payload := bytes.Repeat([]byte("glance-go-web-download "), 4096)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cirros.img" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer source.Close()
	su, _ := url.Parse(source.URL)
	port, _ := strconv.Atoi(su.Port())
	c := testConfig()
	c.Binary.Root, c.Binary.Scope, c.Binary.OfficialStoreDir = root, rs, images
	c.Binary.MaxUploadBytes = 1 << 20
	c.DependencyTimeout, c.RequestTimeout = 5*time.Second, 15*time.Second
	c.Binary.RecoveryTimeout, c.Binary.ResolutionTimeout = 30*time.Second, 5*time.Second
	c.Binary.UploadTimeout, c.Binary.DownloadTimeout = time.Minute, time.Minute
	c.Import = config.ImportSettings{StagingDir: staging, Methods: []string{"glance-direct", "web-download"}, TaskTimeToLive: 48 * time.Hour,
		AllowedSchemes: []string{"http"}, AllowedPorts: []int{port}}
	cacheDir := filepath.Clean(root) + "-cache"
	c.Cache = config.CacheSettings{Enabled: true, Dir: cacheDir, NodeURL: "http://glance-go-test-node:9292", PrefetchInterval: 200 * time.Millisecond, MaxSize: 1 << 30}
	var runtime *binaryRuntime
	var importer *service.Importer
	var cache *service.ImageCache
	var stopHTTP context.CancelFunc
	var httpDone chan error
	address := ""
	start := func() {
		t.Helper()
		r, err := mysql.New(db, 5*time.Second, official, images)
		if err != nil {
			t.Fatal(err)
		}
		checks := startupChecks{ping: db.PingContext, schema: func(ctx context.Context) error { return mysql.VerifySchema(ctx, db) }, scope: func(ctx context.Context) (string, error) { return databaseScope(ctx, db) }}
		if runtime, err = prepareBinary(ctx, c, r, checks.scope, testLogger()); err != nil {
			t.Fatal(err)
		}
		catalog := service.New(r, time.Now)
		if importer, err = service.NewImporter(c.Import, catalog, runtime.owner, http.DefaultClient); err != nil {
			t.Fatal(err)
		}
		if _, err = importer.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if cache, err = service.NewImageCache(ctx, c.Cache, r, runtime.owner, testLogger()); err != nil {
			t.Fatal(err)
		}
		runtime.owner.SetCache(cache)
		cache.Prefetch()
		h, err := api.New(catalog, importIdentity{}, "http://127.0.0.1:9293", "http://keystone-fixture.invalid/v3", 1048576, testLogger(), api.WithBinaryTransfers(runtime.owner), api.WithImports(importer), api.WithCache(cache))
		if err != nil {
			t.Fatal(err)
		}
		ln := localListener(t)
		address = "http://" + ln.Addr().String()
		httpCtx, stop := context.WithCancel(ctx)
		stopHTTP, httpDone = stop, make(chan error, 1)
		go func() { httpDone <- serveHTTP(httpCtx, ln, c, h, runtime.owner, testLogger()) }()
	}
	stop := func() {
		t.Helper()
		stopHTTP()
		if err := <-httpDone; err != nil {
			t.Fatal(err)
		}
		importer.Close()
		cache.Close()
		if err := runtime.close(); err != nil {
			t.Fatal(err)
		}
	}
	start()
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(method, path, body, media, token string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, address+path, strings.NewReader(body))
		req.Header.Set("X-Auth-Token", token)
		if media != "" {
			req.Header.Set("Content-Type", media)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, b)
		}
		return b
	}
	create := func() string {
		b := call("POST", "/v2/images", `{"name":"import","disk_format":"raw","container_format":"bare"}`, "application/json", "member-token", 201)
		var image struct{ ID string }
		_ = json.Unmarshal(b, &image)
		return image.ID
	}
	status := func(id string) map[string]any {
		var image map[string]any
		_ = json.Unmarshal(call("GET", "/v2/images/"+id, "", "", "member-token", 200), &image)
		return image
	}
	wait := func(id, want string) map[string]any {
		t.Helper()
		for range 200 {
			// done when the image reached the status and the task released its lock
			if image := status(id); image["status"] == want && image["os_glance_import_task"] == nil {
				return image
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal(id, "never reached", want, status(id))
		return nil
	}
	info := call("GET", "/v2/info/import", "", "", "member-token", 200)
	if !strings.Contains(string(info), `"value":["glance-direct","web-download"]`) {
		t.Fatal(string(info))
	}

	// glance-direct: stage, then import.
	id := create()
	call("POST", "/v2/images/"+id+"/import", `{"method":{"name":"glance-direct"}}`, "application/json", "member-token", 409) // not staged
	call("PUT", "/v2/images/"+id+"/stage", "staged-bytes", "application/octet-stream", "member-token", 204)
	if s := status(id)["status"]; s != "uploading" {
		t.Fatal("staged image status", s)
	}
	call("PUT", "/v2/images/"+id+"/file", "x", "application/octet-stream", "member-token", 409) // staged: not queued
	call("POST", "/v2/images/"+id+"/import", `{"method":{"name":"glance-direct"}}`, "application/json", "member-token", 202)
	image := wait(id, "active")
	if image["size"] != float64(len("staged-bytes")) || image["os_glance_import_task"] != nil {
		t.Fatal("imported image", image)
	}
	if got := call("GET", "/v2/images/"+id+"/file", "", "", "member-token", 200); string(got) != "staged-bytes" {
		t.Fatal("imported bytes differ")
	}
	if _, err := os.Stat(filepath.Join(staging, id)); !os.IsNotExist(err) {
		t.Fatal("staged data left behind")
	}
	var tasks struct{ Tasks []map[string]any }
	_ = json.Unmarshal(call("GET", "/v2/images/"+id+"/tasks", "", "", "member-token", 200), &tasks)
	if len(tasks.Tasks) != 1 || tasks.Tasks[0]["status"] != "success" || tasks.Tasks[0]["type"] != "api_image_import" || tasks.Tasks[0]["expires_at"] == nil {
		t.Fatal("import task", tasks)
	}
	task := tasks.Tasks[0]["id"].(string)
	call("GET", "/v2/tasks/"+task, "", "", "member-token", 403)
	call("GET", "/v2/tasks/"+task, "", "", "admin-token", 200)
	if list := call("GET", "/v2/tasks?type=api_image_import", "", "", "admin-token", 200); !strings.Contains(string(list), task) {
		t.Fatal("task not listed")
	}

	// web-download, a bad URI and a source that fails.
	id = create()
	call("POST", "/v2/images/"+id+"/import", `{"method":{"name":"web-download","uri":"ftp://example.org/x"}}`, "application/json", "member-token", 400)
	call("POST", "/v2/images/"+id+"/import", `{"method":{"name":"web-download","uri":"`+source.URL+`/cirros.img"}}`, "application/json", "member-token", 202)
	wait(id, "active")
	if got := call("GET", "/v2/images/"+id+"/file", "", "", "member-token", 200); !bytes.Equal(got, payload) {
		t.Fatal("downloaded bytes differ")
	}
	id = create()
	call("POST", "/v2/images/"+id+"/import", `{"method":{"name":"web-download","uri":"`+source.URL+`/missing"}}`, "application/json", "member-token", 202)
	for range 200 {
		_ = json.Unmarshal(call("GET", "/v2/images/"+id+"/tasks", "", "", "member-token", 200), &tasks)
		if len(tasks.Tasks) == 1 && tasks.Tasks[0]["status"] == "failure" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tasks.Tasks[0]["status"] != "failure" || status(id)["status"] != "queued" || status(id)["os_glance_import_task"] != nil {
		t.Fatal("failed import not reverted", tasks, status(id))
	}

	// The original's task-based import (POST /v2/tasks, type import).
	call("POST", "/v2/tasks", `{"type":"import","input":{"import_from":"`+source.URL+`/cirros.img","import_from_format":"raw","image_properties":{"name":"legacy"}}}`, "application/json", "member-token", 403)
	var legacy map[string]any
	_ = json.Unmarshal(call("POST", "/v2/tasks", `{"type":"import","input":{"import_from":"`+source.URL+`/cirros.img","import_from_format":"raw","image_properties":{"name":"legacy","container_format":"bare"}}}`, "application/json", "admin-token", 201), &legacy)
	if legacy["status"] != "pending" || legacy["type"] != "import" {
		t.Fatal("legacy task", legacy)
	}
	legacyImage := legacy["image_id"].(string)
	wait(legacyImage, "active")
	if got := call("GET", "/v2/images/"+legacyImage+"/file", "", "", "member-token", 200); !bytes.Equal(got, payload) {
		t.Fatal("legacy import bytes differ")
	}
	if b := call("GET", "/v2/tasks/"+legacy["id"].(string), "", "", "admin-token", 200); !strings.Contains(string(b), `"status":"success"`) {
		t.Fatal(string(b))
	}

	// Location API: a service registers the image's file in the store.
	located := create()
	data := []byte("located-bytes")
	if err := os.WriteFile(filepath.Join(images, located), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	loc := `{"url":"file://` + images + "/" + located + `","validation_data":{"os_hash_algo":"sha512","os_hash_value":"` + hex.EncodeToString(sum[:]) + `"}}`
	call("POST", "/v2/images/"+located+"/locations", loc, "application/json", "member-token", 403)
	call("POST", "/v2/images/"+located+"/locations", `{"url":"file:///etc/passwd"}`, "application/json", "service-token", 400)
	call("POST", "/v2/images/"+located+"/locations", loc, "application/json", "service-token", 202)
	wait(located, "active")
	if got := call("GET", "/v2/images/"+located+"/file", "", "", "member-token", 200); !bytes.Equal(got, data) {
		t.Fatal("located bytes differ")
	}
	if l := call("GET", "/v2/images/"+located+"/locations", "", "", "service-token", 200); !strings.Contains(string(l), "file://"+images+"/"+located) {
		t.Fatal(string(l))
	}
	call("GET", "/v2/images/"+located+"/locations", "", "", "member-token", 403)
	call("PATCH", "/v2/images/"+located, `[{"op":"add","path":"/locations/-","value":{"url":"file:///x","metadata":{}}}]`, "application/openstack-images-v2.1-json-patch", "member-token", 403)

	// Recovery: a task left processing by a previous process.
	stuck := create()
	call("PUT", "/v2/images/"+stuck+"/stage", "abc", "application/octet-stream", "member-token", 204)
	stop()
	if _, err = db.ExecContext(ctx, "UPDATE `"+official+"`.images SET status='importing' WHERE id=?", stuck); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO `"+official+"`.image_properties (image_id, name, value, created_at, deleted) VALUES (?, 'os_glance_import_task', '00000000-0000-4000-8000-000000000001', UTC_TIMESTAMP(), 0)", stuck); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO `"+official+"`.tasks (id, type, status, owner, created_at, deleted, image_id) VALUES ('00000000-0000-4000-8000-000000000001', 'api_image_import', 'processing', 'import-project', UTC_TIMESTAMP(), 0, ?)", stuck); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO `"+official+"`.task_info (task_id, input) VALUES ('00000000-0000-4000-8000-000000000001', '{}')"); err != nil {
		t.Fatal(err)
	}
	start()
	if image := status(stuck); image["status"] != "queued" || image["os_glance_import_task"] != nil {
		t.Fatal("stale import not recovered", image)
	}
	b := call("GET", "/v2/tasks/00000000-0000-4000-8000-000000000001", "", "", "admin-token", 200)
	if !strings.Contains(string(b), `"status":"failure"`) {
		t.Fatal(string(b))
	}
	// Image cache: a complete download is cached; the next one is a hit.
	cachedImage := create()
	call("PUT", "/v2/images/"+cachedImage+"/file", "cache-me", "application/octet-stream", "member-token", 204)
	call("GET", "/v2/cache", "", "", "member-token", 403)
	call("GET", "/v2/images/"+cachedImage+"/file", "", "", "member-token", 200)
	listing := func() map[string]any {
		var l map[string]any
		_ = json.Unmarshal(call("GET", "/v2/cache", "", "", "admin-token", 200), &l)
		return l
	}
	hits := func(id string) float64 {
		for range 100 {
			for _, e := range listing()["cached_images"].([]any) {
				if m := e.(map[string]any); m["image_id"] == id {
					return m["hits"].(float64)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return -1
	}
	if hits(cachedImage) != 0 {
		t.Fatal("complete download not cached", listing())
	}
	if b, err := os.ReadFile(filepath.Join(cacheDir, cachedImage)); err != nil || string(b) != "cache-me" {
		t.Fatal("cached copy differs", err)
	}
	if got := call("GET", "/v2/images/"+cachedImage+"/file", "", "", "member-token", 200); string(got) != "cache-me" {
		t.Fatal("cached download differs")
	}
	for range 100 {
		if hits(cachedImage) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hits(cachedImage) != 1 {
		t.Fatal("cache hit not counted", listing())
	}
	// Queue + prefetcher, then deletion.
	queued := create()
	call("PUT", "/v2/cache/"+queued, "", "", "admin-token", 400) // not active
	call("PUT", "/v2/images/"+queued+"/file", "prefetch-me", "application/octet-stream", "member-token", 204)
	call("PUT", "/v2/cache/"+queued, "", "", "admin-token", 202)
	if hits(queued) != 0 {
		t.Fatal("queued image not prefetched", listing())
	}
	call("DELETE", "/v2/cache/"+queued, "", "", "admin-token", 204)
	call("DELETE", "/v2/images/"+cachedImage, "", "", "member-token", 204)
	for _, e := range listing()["cached_images"].([]any) {
		if id := e.(map[string]any)["image_id"]; id == queued || id == cachedImage {
			t.Fatal("deleted cache entry still listed", id)
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, cachedImage)); !os.IsNotExist(err) {
		t.Fatal("cached copy of a deleted image left behind")
	}
	call("DELETE", "/v2/cache", "", "", "admin-token", 204)
	if l := listing(); len(l["cached_images"].([]any)) != 0 || len(l["queued_images"].([]any)) != 0 {
		t.Fatal("cache not cleared", l)
	}
	// Deleting a staged image removes its staged data.
	staged := create()
	call("PUT", "/v2/images/"+staged+"/stage", "abc", "application/octet-stream", "member-token", 204)
	call("DELETE", "/v2/images/"+staged, "", "", "member-token", 204)
	if _, err := os.Stat(filepath.Join(staging, staged)); !os.IsNotExist(err) {
		t.Fatal("staged data of a deleted image left behind")
	}
	stop()
}
