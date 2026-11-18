//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/api"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

// MUTATING. Independently authorized NEW EMPTY schema and EXISTING EMPTY private
// root only. Leaves SQL rows and the initialized root for review. Never provision,
// start/stop MySQL, obtain tokens, or delete schemas/accounts/root automatically.
func TestB25MySQLFilesystemHTTP(t *testing.T) {
	if os.Getenv("GLANCE_GO_B25_MYSQL_TEST") != "1" {
		t.Fatal("explicit B25 disposable integration authorization required")
	}
	readDSN := func(key string) string {
		t.Helper()
		b, err := config.ReadSecret(os.Getenv(key))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.TrimSpace(string(b))
		if err = mysql.ValidateTestDSN(s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	migrateDSN, runtimeDSN := readDSN("GLANCE_GO_TEST_DSN_FILE"), readDSN("GLANCE_GO_TEST_RUNTIME_DSN_FILE")
	if migrateDSN == runtimeDSN {
		t.Fatal("separate migration and runtime accounts required")
	}
	migration, err := mysql.Open(migrateDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer migration.Close()
	db, err := mysql.Open(runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = mysql.CheckAccount(ctx, migration, "migrate"); err != nil {
		t.Fatal(err)
	}
	if err = mysql.CheckAccount(ctx, db, "serve"); err != nil {
		t.Fatal(err)
	}
	ms, err := databaseScope(ctx, migration)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := databaseScope(ctx, db)
	if err != nil || ms != rs {
		t.Fatal("runtime/migrator scopes differ")
	}
	var tables int
	if migration.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables) != nil || tables != 0 {
		t.Fatal("NEW EMPTY schema required")
	}
	// GLANCE_GO_B25_STORE=rbd runs the same pipeline on a NEW EMPTY RBD pool
	// (binary built with -tags rbd, cluster from infra/ceph-micro).
	backend := config.Binary{Backend: "filesystem"}
	root := ""
	if os.Getenv("GLANCE_GO_B25_STORE") == "rbd" {
		backend = config.Binary{Backend: "rbd", RBDConf: os.Getenv("GLANCE_GO_RBD_TEST_CONF"), RBDUser: "admin", RBDPool: os.Getenv("GLANCE_GO_RBD_TEST_POOL")}
		if n, err := rbdImageCount(backend); err != nil || n != 0 {
			t.Fatal("NEW EMPTY dedicated rbd pool required", err)
		}
	} else {
		root = os.Getenv("GLANCE_GO_B25_TEST_ROOT")
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("EXISTING EMPTY dedicated root required")
		}
	}
	t.Log("STORE:", backend.Backend)
	if err = mysql.Migrate(ctx, migration); err != nil {
		t.Fatal(err)
	}
	if err = mysql.Migrate(ctx, migration); err != nil {
		t.Fatal(err)
	}
	if err = mysql.VerifySchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	var version string
	if db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version) != nil {
		t.Fatal("cannot record server version")
	}
	t.Log("MySQL effective version:", version, "scope:", rs)
	backend.Root = root
	if err = initStore(backend, rs); err != nil {
		t.Fatal("store initialization failed", err)
	}
	c := testConfig()
	c.Binary.Backend, c.Binary.Root, c.Binary.Scope = backend.Backend, root, rs
	c.Binary.RBDConf, c.Binary.RBDUser, c.Binary.RBDPool = backend.RBDConf, backend.RBDUser, backend.RBDPool
	c.DependencyTimeout = 5 * time.Second
	c.RequestTimeout = 15 * time.Second
	c.Binary.RecoveryTimeout = 30 * time.Second
	c.Binary.ResolutionTimeout = 5 * time.Second
	c.Binary.UploadTimeout = time.Minute
	c.Binary.DownloadTimeout = time.Minute
	var authenticator auth.Authenticator
	token := "explicit-test-token"
	keystoneURL := "http://keystone-fixture.invalid/v3"
	if os.Getenv("GLANCE_GO_B25_KEYSTONE_TEST") == "1" {
		// Real validation only, using PREEXISTING authorized short-lived secrets.
		// This test neither issues/renews credentials nor obtains client tokens.
		keystoneURL = strings.TrimRight(os.Getenv("GLANCE_GO_KEYSTONE_AUTH_URL"), "/")
		if config.ValidateURL(keystoneURL) != nil || !strings.HasSuffix(keystoneURL, "/v3") {
			t.Fatal("invalid Keystone URL")
		}
		b, err := config.ReadSecret(os.Getenv("GLANCE_GO_KEYSTONE_CREDENTIAL_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		var credential config.ServiceCredential
		if json.Unmarshal(b, &credential) != nil || credential.ID == "" || credential.Secret == "" {
			t.Fatal("invalid service credential")
		}
		b, err = config.ReadSecret(os.Getenv("GLANCE_GO_TEST_CLIENT_TOKEN_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		token = strings.TrimSpace(string(b))
		authenticator = auth.NewKeystone(keystoneURL, &http.Client{Timeout: 5 * time.Second}, credential)
		if _, err = authenticator.Authenticate(ctx, token); err != nil {
			t.Fatal("real Keystone validation failed (diagnostic suppressed)")
		}
		t.Log("AUTH: real Keystone token validation")
	} else {
		authenticator = integrationIdentity{}
		t.Log("AUTH: EXPLICIT TEST DOUBLE, not real Keystone")
	}
	var runtime *binaryRuntime
	var stopHTTP context.CancelFunc
	var httpDone chan error
	address := ""
	start := func() {
		t.Helper()
		r := mysql.New(db, c.DependencyTimeout)
		checks := startupChecks{ping: db.PingContext, account: func(ctx context.Context) error { return mysql.CheckAccount(ctx, db, "serve") }, schema: func(ctx context.Context) error { return mysql.VerifySchema(ctx, db) }, scope: func(ctx context.Context) (string, error) { return databaseScope(ctx, db) }}
		if err = verifyBackend(ctx, checks); err != nil {
			t.Fatal(err)
		}
		runtime, err = prepareBinary(ctx, c, r, checks.scope, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		h, err := api.New(service.New(r, time.Now), authenticator, "http://127.0.0.1:9293", keystoneURL, 1048576, testLogger(), api.WithBinaryTransfers(runtime.owner))
		if err != nil {
			t.Fatal(err)
		}
		ln := localListener(t)
		address = "http://" + ln.Addr().String()
		httpCtx, stop := context.WithCancel(ctx)
		stopHTTP = stop
		httpDone = make(chan error, 1)
		go func() { httpDone <- serveHTTP(httpCtx, ln, c, h, runtime.owner, testLogger()) }()
	}
	stop := func() {
		t.Helper()
		if stopHTTP != nil {
			stopHTTP()
			if err := <-httpDone; err != nil {
				t.Fatal(err)
			}
			stopHTTP = nil
		}
		if runtime != nil {
			if err := runtime.close(); err != nil {
				t.Fatal(err)
			}
			runtime = nil
		}
	}
	defer func() {
		if stopHTTP != nil {
			stop()
		}
	}()
	start()
	client := &http.Client{Timeout: 20 * time.Second}
	request := func(method, path, body, media, subject string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, address+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if subject != "" {
			req.Header.Set("X-Auth-Token", subject)
		}
		if media != "" {
			req.Header.Set("Content-Type", media)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("HTTP transport failed")
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d (body suppressed)", method, path, resp.StatusCode, want)
		}
		return b
	}
	request("GET", "/v2/images", "", "", "", 401)
	request("GET", "/v2/images", "", "", "invalid-test-token", 401)
	b := request("POST", "/v2/images", `{"name":"b25-integration","disk_format":"raw","container_format":"bare","visibility":"private","tags":["retained"],"custom":"preserved"}`, "application/json", token, 201)
	var image struct{ ID, Owner, Status string }
	if json.Unmarshal(b, &image) != nil || image.ID == "" || image.Owner == "" || image.Status != "queued" {
		t.Fatal("invalid created image")
	}
	id := image.ID
	base := "/v2/images/" + id
	t.Log("test image:", id)
	request("PUT", base+"/file", "abcdef", "application/octet-stream", token, 204)
	if got := request("GET", base+"/file", "", "", token, 200); !bytes.Equal(got, []byte("abcdef")) {
		t.Fatal("bytes differ")
	}
	r := mysql.New(db, c.DependencyTimeout)
	s, err := r.DataState(ctx, id)
	if err != nil || s.Status != "active" || s.Operation == nil || s.Operation.Phase != model.DataCommitted || s.Size == nil || *s.Size != 6 {
		t.Fatal("physical SQL receipt differs")
	}
	request("PATCH", base, `[{"op":"replace","path":"/name","value":"patched"}]`, "application/openstack-images-v2.1-json-patch", token, 200)
	request("PATCH", base, `[{"op":"replace","path":"/name","value":"partial"},{"op":"replace","path":"/size","value":99}]`, "application/openstack-images-v2.1-json-patch", token, 403)
	stop()
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = mysql.Open(runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	start() // NEW pool/owner/server, NOT a process/MySQL restart
	b = request("GET", base, "", "", token, 200)
	var metadata map[string]any
	if json.Unmarshal(b, &metadata) != nil || metadata["name"] != "patched" || metadata["custom"] != "preserved" {
		t.Fatal("PATCH was lost")
	}
	if got := request("GET", base+"/file", "", "", token, 200); !bytes.Equal(got, []byte("abcdef")) {
		t.Fatal("reopened bytes differ")
	}
	request("DELETE", base, "", "", token, 204)
	request("GET", base, "", "", token, 404)
	r = mysql.New(db, c.DependencyTimeout)
	s, err = r.DataState(ctx, id)
	if err != nil || !s.Deleted || s.Operation.Phase != model.DataCleaned {
		t.Fatal("tombstone/cleanup not persisted")
	}
	if backend.Backend == "rbd" {
		if n, err := rbdImageCount(c.Binary); err != nil || n != 0 {
			t.Fatal("rbd cleanup not complete", n, err)
		}
	} else {
		for _, dir := range []string{"objects", "staging"} {
			entries, err := os.ReadDir(root + "/" + dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("filesystem cleanup not complete")
			}
		}
	}
	stop()
}

type integrationIdentity struct{}

func (integrationIdentity) Authenticate(_ context.Context, token string) (auth.Identity, error) {
	if token != "explicit-test-token" {
		return auth.Identity{}, auth.ErrInvalidToken
	}
	return auth.Identity{UserID: "integration-user", ProjectID: "integration-project", Roles: []string{"reader", "member"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
