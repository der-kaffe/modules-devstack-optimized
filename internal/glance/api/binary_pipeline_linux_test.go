//go:build linux

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

// This is NOT MySQL/Keystone integration. SQL/identity are independent doubles;
// only the store uses an exclusively owned t.TempDir, never laboratory data.
type pipelineSQL struct {
	*repoFake
	mu              sync.Mutex
	op              *model.DataOperation
	reserveCalls    int
	commitUncertain bool
}

func (d *pipelineSQL) Get(ctx context.Context, id string) (model.Image, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return model.Image{}, ctx.Err()
	}
	if id != d.image.ID || d.image.Status == "deleted" {
		return model.Image{}, repository.ErrNotFound
	}
	return d.image, nil
}
func (d *pipelineSQL) ReserveData(_ context.Context, id, attempt string, expected *int64, now time.Time) (model.DataOperation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reserveCalls++
	if id != d.image.ID || d.image.Status != "queued" || d.op != nil && d.op.Phase != model.DataCleaned {
		return model.DataOperation{}, repository.ErrConflict
	}
	d.image.Status = "saving"
	d.op = &model.DataOperation{ImageID: id, AttemptID: attempt, Phase: model.DataReserved, ExpectedSize: expected, CreatedAt: now, UpdatedAt: now}
	return *d.op, nil
}
func (d *pipelineSQL) check(id, attempt string, phase model.DataPhase) error {
	if id != d.image.ID || d.image.Status != "saving" || d.op == nil || d.op.AttemptID != attempt || d.op.Phase != phase {
		return repository.ErrConflict
	}
	return nil
}
func (d *pipelineSQL) StageData(_ context.Context, id, attempt string, c model.DataContent, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(id, attempt, model.DataReserved); err != nil {
		return err
	}
	d.op.Phase, d.op.Size, d.op.VirtualSize, d.op.Checksum, d.op.SHA512, d.op.UpdatedAt = model.DataStaged, &c.Size, c.VirtualSize, &c.Checksum, &c.SHA512, now
	return nil
}
func (d *pipelineSQL) PublishData(_ context.Context, id, attempt string, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(id, attempt, model.DataStaged); err != nil {
		return err
	}
	d.op.Phase, d.op.UpdatedAt = model.DataPublished, now
	return nil
}
func (d *pipelineSQL) CommitData(_ context.Context, id, attempt string, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(id, attempt, model.DataPublished); err != nil {
		return err
	}
	d.op.Phase, d.op.UpdatedAt, d.image.Status = model.DataCommitted, now, "active"
	algorithm := "sha512"
	d.image.Size, d.image.VirtualSize, d.image.Checksum, d.image.HashAlgorithm, d.image.HashValue = d.op.Size, d.op.VirtualSize, d.op.Checksum, &algorithm, d.op.SHA512
	if d.commitUncertain {
		return repository.ErrCommitUncertain
	}
	return nil
}
func (d *pipelineSQL) BeginDataCleanup(_ context.Context, id, attempt, target string, code *string, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.op == nil || d.op.AttemptID != attempt || d.image.Status != "saving" || target != "queued" || d.op.Phase == model.DataCommitted {
		return repository.ErrConflict
	}
	d.op.Phase, d.op.CleanupTarget, d.op.LastErrorCode, d.op.UpdatedAt = model.DataCleanup, &target, code, now
	return nil
}
func (d *pipelineSQL) CompleteDataCleanup(_ context.Context, id, attempt string, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(id, attempt, model.DataCleanup); err != nil {
		return err
	}
	d.op.Phase, d.op.UpdatedAt, d.image.Status = model.DataCleaned, now, "queued"
	return nil
}
func (d *pipelineSQL) DataStateSnapshot(ctx context.Context, id string) (model.DataState, error) {
	return d.DataState(ctx, id)
}
func (d *pipelineSQL) DataState(ctx context.Context, id string) (model.DataState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx.Err() != nil {
		return model.DataState{}, ctx.Err()
	}
	if id != d.image.ID {
		return model.DataState{}, repository.ErrNotFound
	}
	var op *model.DataOperation
	if d.op != nil {
		c := *d.op
		op = &c
	}
	i := d.image
	return model.DataState{ImageID: i.ID, Status: i.Status, Deleted: i.Status == "deleted", Size: i.Size, VirtualSize: i.VirtualSize, Checksum: i.Checksum, HashAlgorithm: i.HashAlgorithm, HashValue: i.HashValue, Operation: op}, nil
}
func (d *pipelineSQL) ListDataOperations(_ context.Context, after string, _ int) ([]model.DataOperation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.op != nil && d.op.ImageID > after {
		return []model.DataOperation{*d.op}, nil
	}
	return []model.DataOperation{}, nil
}

type binaryIdentity struct{ who auth.Identity }

func (a binaryIdentity) Authenticate(context.Context, string) (auth.Identity, error) {
	return a.who, nil
}

func pipelineHarness(t *testing.T, ready bool, who auth.Identity, max int64) (http.Handler, *pipelineSQL, *service.UploadCoordinator) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scope := uuid.NewString()
	if err := store.Initialize(root, scope); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFilesystem(root, scope)
	if err != nil {
		t.Fatal(err)
	}
	id, raw, bare := binaryImage, "raw", "bare"
	i, err := model.New(model.CreateInput{ID: &id, DiskFormat: &raw, ContainerFormat: &bare}, "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d := &pipelineSQL{repoFake: &repoFake{image: i}}
	u, err := service.NewUploadCoordinator(d, d, fs, service.UploadLimits{MaxBytes: max, MaxConcurrent: 1, ResolutionTimeout: time.Second, RecoveryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := u.Close(); err != nil {
			t.Error(err)
		}
		if err := fs.Close(); err != nil {
			t.Error(err)
		}
	})
	if ready {
		if _, err := u.RecoverUploads(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h, err := New(service.New(d, time.Now), binaryIdentity{who}, "http://127.0.0.1:9293", "http://keystone/identity/v3", 1048576, slog.New(slog.NewTextHandler(io.Discard, nil)), WithBinaryTransfers(u))
	if err != nil {
		t.Fatal(err)
	}
	return h, d, u
}
func pipelineIdentity(roles ...string) auth.Identity {
	return auth.Identity{UserID: "u", ProjectID: "p", Roles: roles, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestBinaryPipelineUsesActualCoordinatorAndStore(t *testing.T) {
	h, d, _ := pipelineHarness(t, true, pipelineIdentity("member", "reader"), 1024)
	d.commitUncertain = true // applied commit, fresh authoritative state proves success
	put := httptest.NewRecorder()
	h.ServeHTTP(put, binaryRequest("PUT", strings.NewReader("abcdef")))
	if put.Code != 204 || d.image.Status != "active" || d.op.Phase != model.DataCommitted {
		t.Fatal(put.Code, put.Body.String())
	}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, binaryRequest("GET", nil))
	if get.Code != 200 || get.Body.String() != "abcdef" || get.Header().Get("Content-MD5") != binaryMD5 {
		t.Fatal(get.Code, get.Header(), get.Body.String())
	}
	metadata := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v2/images/"+binaryImage, nil)
	r.Header.Set("X-Auth-Token", "fixture")
	h.ServeHTTP(metadata, r)
	if metadata.Code != 200 || !strings.Contains(metadata.Body.String(), `"status":"active"`) {
		t.Fatal("B20 regression", metadata.Body.String())
	}
}

func TestBinaryPipelineAuthorizationAndStates(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		code         int
	}{
		{"reader-put", "PUT", 403}, {"member-get", "GET", 403}, {"admin-put", "PUT", 204}, {"private-put", "PUT", 404}, {"public-put", "PUT", 403},
		{"private-get", "GET", 404}, {"public-get", "GET", 204}, {"community-get", "GET", 204}, {"queued-get", "GET", 204}, {"deleted", "PUT", 404}, {"missing", "GET", 404}, {"saving-get", "GET", 204}, {"active-without-intention", "GET", 503}, {"not-ready", "PUT", 503}, {"not-ready-get", "GET", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who := pipelineIdentity("reader", "member")
			ready := !strings.HasPrefix(tc.name, "not-ready")
			switch tc.name {
			case "reader-put":
				who = pipelineIdentity("reader")
			case "member-get":
				who = pipelineIdentity("member")
			case "admin-put":
				who = pipelineIdentity("admin")
				who.ProjectID = "other"
			}
			if strings.HasPrefix(tc.name, "private-") || strings.HasPrefix(tc.name, "public-") || tc.name == "community-get" {
				who.ProjectID = "other"
			}
			h, d, _ := pipelineHarness(t, ready, who, 1024)
			if strings.HasPrefix(tc.name, "private-") {
				d.image.Visibility = "private"
			}
			if strings.HasPrefix(tc.name, "public-") {
				d.image.Visibility = "public"
			}
			if tc.name == "community-get" {
				d.image.Visibility = "community"
			}
			if tc.name == "deleted" {
				d.image.Status = "deleted"
			}
			if tc.name == "active-without-intention" {
				d.image.Status = "active"
			}
			if tc.name == "saving-get" {
				d.image.Status = "saving"
				d.op = &model.DataOperation{ImageID: binaryImage, AttemptID: uuid.NewString(), Phase: model.DataReserved}
			}
			r := binaryRequest(tc.method, strings.NewReader("abcdef"))
			if tc.name == "missing" {
				r.URL.Path = "/v2/images/00000000-0000-4000-8000-000000000099/file"
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatal(w.Code, w.Body.String())
			}
			if tc.code != 204 && d.reserveCalls != 0 {
				t.Fatal("denied request reserved data")
			}
		})
	}
}

type pipelineBlockedBody struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (b *pipelineBlockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *pipelineBlockedBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestBinaryPipelineCancellationUnblocksBodyAndCapacity(t *testing.T) {
	h, d, _ := pipelineHarness(t, true, pipelineIdentity("member", "reader"), 1024)
	b := &pipelineBlockedBody{entered: make(chan struct{}), closed: make(chan struct{})}
	r := binaryRequest("PUT", nil)
	r.Body = b
	r.ContentLength = -1
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(first, r) }()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, binaryRequest("PUT", strings.NewReader("abcdef")))
	if second.Code != 503 {
		t.Error("concurrent transfer not refused", second.Code, second.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled HTTP reader leaked")
	}
	if first.Code != 503 || d.image.Status != "queued" || d.op.Phase != model.DataCleaned || d.reserveCalls != 1 {
		t.Fatal(first.Code, d.image.Status, d.op)
	}
	third := httptest.NewRecorder()
	h.ServeHTTP(third, binaryRequest("PUT", strings.NewReader("abcdef")))
	if third.Code != 204 {
		t.Fatal("capacity not released", third.Code, third.Body.String())
	}
}

type generatedImage struct {
	remaining   int64
	largestRead int
}

func (b *generatedImage) Read(p []byte) (int, error) {
	if len(p) > b.largestRead {
		b.largestRead = len(p)
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:int(b.remaining)]
	}
	for i := range p {
		p[i] = byte(i % 251)
	}
	b.remaining -= int64(len(p))
	return len(p), nil
}

func TestBinaryPipelineBoundedUnknownStreamAndInterruptedSource(t *testing.T) {
	for _, tc := range []string{"generated", "too-large", "short", "interrupted"} {
		t.Run(tc, func(t *testing.T) {
			const size int64 = 2 * 1024 * 1024
			h, d, _ := pipelineHarness(t, true, pipelineIdentity("member", "reader"), size)
			g := &generatedImage{remaining: size}
			r := binaryRequest("PUT", nil)
			r.Body = io.NopCloser(g)
			r.ContentLength = -1
			code := 204
			switch tc {
			case "too-large":
				g.remaining++
				code = 413
			case "short":
				r.ContentLength = size
				g.remaining--
				code = 400
			case "interrupted":
				r.Body = io.NopCloser(&errorBinaryReader{})
				code = 400
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != code || g.largestRead > store.BufferSize {
				t.Fatal(w.Code, w.Body.String(), g.largestRead)
			}
			if code != 204 && (d.image.Status != "queued" || d.op.Phase != model.DataCleaned) {
				t.Fatal("failed body not cleaned", d.image.Status)
			}
			if tc == "generated" && (g.remaining != 0 || d.image.Size == nil || *d.image.Size != size) {
				t.Fatal("stream not fully consumed")
			}
		})
	}
}

type errorBinaryReader struct{}

func (*errorBinaryReader) Read([]byte) (int, error) {
	return 0, errors.New("secret source diagnostics")
}

func TestBinaryPipelineConfirmedEmptyContent(t *testing.T) {
	// Service tests independently inject corrupt/missing Open results. Here a
	// real zero-byte upload exercises the distinct confirmed-empty HTTP case.
	h, _, _ := pipelineHarness(t, true, pipelineIdentity("member", "reader"), 1024)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, binaryRequest("PUT", bytes.NewReader(nil)))
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, binaryRequest("GET", nil))
	if w.Code != 200 || w.Header().Get("Content-Length") != "0" || w.Header().Get("Content-MD5") != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatal(w.Code, w.Header())
	}
}

// Real HTTP server (not a recorder) so net/http takes the ReadFrom/sendfile
// path through the logging wrapper; full and ranged bytes must be exact.
func TestBinaryPipelineZeroCopyOverRealHTTP(t *testing.T) {
	h, _, _ := pipelineHarness(t, true, pipelineIdentity("member", "reader"), 8<<20)
	payload := bytes.Repeat([]byte("0123456789abcdef\x00\xff"), (3<<20)/18+5)
	put := httptest.NewRecorder()
	h.ServeHTTP(put, binaryRequest("PUT", bytes.NewReader(payload)))
	if put.Code != 204 {
		t.Fatal(put.Code, put.Body.String())
	}
	server := httptest.NewServer(h)
	defer server.Close()
	get := func(rangeHeader string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest("GET", server.URL+binaryPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Auth-Token", "fixture-token")
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}
	resp, body := get("")
	if resp.StatusCode != 200 || !bytes.Equal(body, payload) || resp.Header.Get("Content-Length") != strconv.Itoa(len(payload)) {
		t.Fatal("full download differs", resp.StatusCode, len(body))
	}
	resp, body = get("bytes=100-1099")
	if resp.StatusCode != 206 || !bytes.Equal(body, payload[100:1100]) || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-1099/%d", len(payload)) {
		t.Fatal("ranged download differs", resp.StatusCode, len(body))
	}
}
