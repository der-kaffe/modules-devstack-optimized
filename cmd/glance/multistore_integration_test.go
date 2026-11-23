//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/api"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	driver "github.com/go-sql-driver/mysql"
)

// MUTATING, opt-in: enabled_backends (two file stores) and delayed_delete
// over MySQL (copy of the original's DDL) through HTTP: upload to a chosen
// store, copy-image, deleting one store's copy, then a delayed delete that
// only glance-scrubber completes.
func TestMultiStoreMySQLFilesystemHTTP(t *testing.T) {
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
	fast := filepath.Clean(root) + "-fast"
	if err = os.MkdirAll(fast, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = store.Initialize(root, rs); err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.Binary.Root, c.Binary.Scope, c.Binary.OfficialStoreDir = root, rs, images
	c.Binary.MaxUploadBytes = 1 << 20
	c.DependencyTimeout, c.RequestTimeout = 5*time.Second, 15*time.Second
	c.Binary.RecoveryTimeout, c.Binary.ResolutionTimeout = 30*time.Second, 5*time.Second
	c.Binary.UploadTimeout, c.Binary.DownloadTimeout = time.Minute, time.Minute
	c.Import = config.ImportSettings{StagingDir: filepath.Clean(root) + "-staging", Methods: []string{"glance-direct", "web-download", "copy-image"}, TaskTimeToLive: 48 * time.Hour}
	c.DefaultBackend, c.DelayedDelete = "local", true
	c.Backends = []config.StoreBackend{{Name: "local", Type: "file", Dir: images, Description: "Local"}, {Name: "fast", Type: "file", Dir: fast, Weight: 5}}
	r, err := mysql.New(db, 5*time.Second, official, images)
	if err != nil {
		t.Fatal(err)
	}
	checks := startupChecks{scope: func(ctx context.Context) (string, error) { return databaseScope(ctx, db) }}
	runtime, err := prepareBinary(ctx, c, r, checks.scope, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	catalog := service.New(r, time.Now)
	importer, err := service.NewImporter(c.Import, catalog, runtime.owner, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	list := []api.StoreInfo{{ID: "local", Type: "file", Description: "Local", DataDir: images, Default: true}, {ID: "fast", Type: "file", DataDir: fast, Weight: 5}}
	h, err := api.New(catalog, importIdentity{}, "http://127.0.0.1:9293", "http://keystone-fixture.invalid/v3", 1048576, testLogger(),
		api.WithBinaryTransfers(runtime.owner), api.WithImports(importer), api.WithStores(list, runtime.owner), api.WithLocationViews(true, true))
	if err != nil {
		t.Fatal(err)
	}
	ln := localListener(t)
	address := "http://" + ln.Addr().String()
	httpCtx, stopHTTP := context.WithCancel(ctx)
	httpDone := make(chan error, 1)
	go func() { httpDone <- serveHTTP(httpCtx, ln, c, h, runtime.owner, testLogger()) }()
	defer func() {
		stopHTTP()
		if err := <-httpDone; err != nil {
			t.Error(err)
		}
		importer.Close()
		if err := runtime.close(); err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(method, path string, body io.Reader, header map[string]string, token string, want int) []byte {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, address+path, body)
		req.Header.Set("X-Auth-Token", token)
		for k, v := range header {
			req.Header.Set(k, v)
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
	jsonHeader := map[string]string{"Content-Type": "application/json"}
	image := func(id string) map[string]any {
		var v map[string]any
		_ = json.Unmarshal(call("GET", "/v2/images/"+id, nil, nil, "member-token", 200), &v)
		return v
	}

	// /v2/info/stores, and the admin-only detail.
	stores := strings.TrimSpace(string(call("GET", "/v2/info/stores", nil, nil, "member-token", 200)))
	if stores != `{"stores":[{"default":"true","description":"Local","id":"local"},{"id":"fast"}]}` {
		t.Fatal(stores)
	}
	call("GET", "/v2/info/stores/detail", nil, nil, "member-token", 403)
	if d := string(call("GET", "/v2/info/stores/detail", nil, nil, "admin-token", 200)); !strings.Contains(d, `"data_dir":"`+fast+`"`) || !strings.Contains(d, `"weight":5`) {
		t.Fatal(d)
	}

	// Upload to the fast store; an unknown store is refused.
	var created struct{ ID string }
	_ = json.Unmarshal(call("POST", "/v2/images", strings.NewReader(`{"name":"multi","disk_format":"raw","container_format":"bare"}`), jsonHeader, "member-token", 201), &created)
	id := created.ID
	payload := bytes.Repeat([]byte("multi-store "), 2048)
	octet := map[string]string{"Content-Type": "application/octet-stream", "X-Image-Meta-Store": "nowhere"}
	call("PUT", "/v2/images/"+id+"/file", bytes.NewReader(payload), octet, "member-token", 400)
	octet["X-Image-Meta-Store"] = "fast"
	call("PUT", "/v2/images/"+id+"/file", bytes.NewReader(payload), octet, "member-token", 204)
	if v := image(id); v["status"] != "active" || v["stores"] != "fast" {
		t.Fatal("upload to fast", v)
	}
	if _, err = os.Stat(filepath.Join(images, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the default store got the data", err)
	}
	if got := call("GET", "/v2/images/"+id+"/file", nil, nil, "member-token", 200); !bytes.Equal(got, payload) {
		t.Fatal("download from the fast store")
	}
	// The listing shows the same locations, direct_url and stores (one
	// query for the page).
	var page struct{ Images []map[string]any }
	_ = json.Unmarshal(call("GET", "/v2/images", nil, nil, "member-token", 200), &page)
	listed := false
	for _, v := range page.Images {
		if v["id"] == id {
			listed = true
			locs, _ := v["locations"].([]any)
			if v["stores"] != "fast" || v["direct_url"] != "file://"+fast+"/"+id || len(locs) != 1 {
				t.Fatal("listed image", v)
			}
		}
	}
	if !listed {
		t.Fatal("image not listed")
	}

	// copy-image is admin-only; it copies to the default store.
	copyBody := `{"method":{"name":"copy-image"},"stores":["local"]}`
	call("POST", "/v2/images/"+id+"/import", strings.NewReader(copyBody), jsonHeader, "member-token", 403)
	call("POST", "/v2/images/"+id+"/import", strings.NewReader(`{"method":{"name":"copy-image"},"stores":["fast"]}`), jsonHeader, "admin-token", 400)
	call("POST", "/v2/images/"+id+"/import", strings.NewReader(copyBody), jsonHeader, "admin-token", 202)
	for n := 0; ; n++ {
		v := image(id)
		if v["stores"] == "fast,local" && v["os_glance_import_task"] == nil {
			if v["status"] != "active" || v["os_glance_importing_to_stores"] != "" {
				t.Fatal("copy-image state", v)
			}
			break
		}
		if n > 200 {
			t.Fatal("copy-image never finished", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if b, err := os.ReadFile(filepath.Join(images, id)); err != nil || !bytes.Equal(b, payload) {
		t.Fatal("copy in the default store", err)
	}

	// One store's copy goes (admin only); the last one cannot.
	call("DELETE", "/v2/stores/fast/"+id, nil, nil, "member-token", 403)
	call("DELETE", "/v2/stores/fast/"+id, nil, nil, "admin-token", 204)
	if v := image(id); v["stores"] != "local" {
		t.Fatal("after deleting the fast copy", v)
	}
	if _, err = os.Stat(filepath.Join(fast, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fast copy kept", err)
	}
	call("DELETE", "/v2/stores/local/"+id, nil, nil, "admin-token", 409)
	if got := call("GET", "/v2/images/"+id+"/file", nil, nil, "member-token", 200); !bytes.Equal(got, payload) {
		t.Fatal("download from the default store")
	}

	// delayed_delete: the image is gone for the API, the data stays
	// pending_delete until the scrubber runs.
	call("DELETE", "/v2/images/"+id, nil, nil, "member-token", 204)
	call("GET", "/v2/images/"+id, nil, nil, "member-token", 404)
	var status, location string
	if err = db.QueryRowContext(ctx, "SELECT status FROM "+official+".images WHERE id=?", id).Scan(&status); err != nil || status != "pending_delete" {
		t.Fatal("image after delayed delete", status, err)
	}
	if err = db.QueryRowContext(ctx, "SELECT status FROM "+official+".image_locations WHERE image_id=? ORDER BY id DESC LIMIT 1", id).Scan(&location); err != nil || location != "pending_delete" {
		t.Fatal("location after delayed delete", location, err)
	}
	if _, err = os.Stat(filepath.Join(images, id)); err != nil {
		t.Fatal("delayed delete removed the data", err)
	}
	done, err := r.Scrub(ctx, 0, time.Now().Add(time.Second))
	if err != nil || done != 1 {
		t.Fatal("scrub", done, err)
	}
	if _, err = os.Stat(filepath.Join(images, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("scrubber kept the data", err)
	}
	if err = db.QueryRowContext(ctx, "SELECT status FROM "+official+".images WHERE id=?", id).Scan(&status); err != nil || status != "deleted" {
		t.Fatal("image after scrub", status, err)
	}
}
