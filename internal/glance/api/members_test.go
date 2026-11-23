package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

type memberRepoFake struct {
	*repoFake
	members map[string]model.Member
}

func (r *memberRepoFake) ListMembers(context.Context, string) ([]model.Member, error) {
	out := []model.Member{}
	for _, m := range r.members {
		out = append(out, m)
	}
	return out, nil
}
func (r *memberRepoFake) GetMember(_ context.Context, _, id string) (model.Member, error) {
	if m, ok := r.members[id]; ok {
		return m, nil
	}
	return model.Member{}, repository.ErrMemberNotFound
}
func (r *memberRepoFake) CreateMember(_ context.Context, image, id string, now time.Time, check func(model.Image, int) error) (model.Member, error) {
	if err := check(r.image, len(r.members)); err != nil {
		return model.Member{}, err
	}
	m := model.Member{ImageID: image, MemberID: id, Status: "pending", CreatedAt: now, UpdatedAt: now}
	r.members[id] = m
	return m, nil
}
func (r *memberRepoFake) UpdateMember(_ context.Context, _, id, status string, now time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return m, repository.ErrMemberNotFound
	}
	if err := check(r.image, m); err != nil {
		return model.Member{}, err
	}
	m.Status, m.UpdatedAt = status, now
	r.members[id] = m
	return m, nil
}
func (r *memberRepoFake) DeleteMember(_ context.Context, _, id string, _ time.Time, check func(model.Image, model.Member) error) (model.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return m, repository.ErrMemberNotFound
	}
	if err := check(r.image, m); err != nil {
		return model.Member{}, err
	}
	delete(r.members, id)
	return m, nil
}

func TestMembersTagsAndActionsRoutes(t *testing.T) {
	const id = "a1b2c3d4-e5f6-4789-8abc-0123456789ab"
	repo := &memberRepoFake{repoFake: &repoFake{image: model.Image{ID: id, Owner: "p", Status: "active", Visibility: "shared", Tags: []string{}}}, members: map[string]model.Member{}}
	a := &authFake{identity: auth.Identity{UserID: "u", ProjectID: "p", Roles: []string{"member", "reader"}, ExpiresAt: time.Now().Add(time.Hour)}}
	logs := &bytes.Buffer{}
	h, err := New(service.New(repo, time.Now), a, "http://127.0.0.1:9293", "http://192.168.122.10/identity/v3", 1048576, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	as := func(project string, roles ...string) {
		a.identity = auth.Identity{UserID: "u", ProjectID: project, Roles: roles, ExpiresAt: time.Now().Add(time.Hour)}
	}
	expect := func(method, path, body string, code int) map[string]any {
		t.Helper()
		w := request(h, method, path, body)
		if w.Code != code {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		out := map[string]any{}
		if w.Body.Len() > 0 {
			_ = json.Unmarshal(w.Body.Bytes(), &out)
		}
		return out
	}
	base := "/v2/images/" + id
	m := expect("POST", base+"/members", `{"member":"guest"}`, 200)
	if m["member_id"] != "guest" || m["status"] != "pending" || m["schema"] != "/v2/schemas/member" || m["image_id"] != id {
		t.Fatal(m)
	}
	expect("POST", base+"/members", `{}`, 400)
	expect("POST", base+"/members", `[1]`, 400)
	if list := expect("GET", base+"/members", "", 200); list["schema"] != "/v2/schemas/members" || len(list["members"].([]any)) != 1 {
		t.Fatal(list)
	}
	expect("PUT", base+"/members/guest", `{"status":"accepted"}`, 403) // owner cannot accept
	as("guest", "member", "reader")
	if m = expect("PUT", base+"/members/guest", `{"status":"accepted"}`, 200); m["status"] != "accepted" {
		t.Fatal(m)
	}
	expect("GET", base+"/members/guest", "", 200)
	expect("GET", base, "", 200)
	expect("PUT", base+"/tags/x", "", 403) // a member cannot modify
	as("p", "member", "reader")
	expect("DELETE", base+"/members/guest", "", 204)
	expect("DELETE", base+"/members/guest", "", 404)
	expect("PUT", base+"/tags/x", "", 204)
	expect("DELETE", base+"/tags/x", "", 204)
	expect("DELETE", base+"/tags/x", "", 404)
	expect("GET", base+"/tags/x", "", 405)
	as("other", "member", "reader")
	expect("POST", base+"/actions/deactivate", "", 404)
	as("p", "admin")
	expect("POST", base+"/actions/deactivate", "", 204)
	if repo.image.Status != "deactivated" {
		t.Fatal(repo.image.Status)
	}
	expect("POST", base+"/actions/reactivate", "", 204)
	expect("POST", base+"/actions/explode", "", 404)
	expect("GET", base+"/actions/deactivate", "", 405)
	expect("GET", "/v2/schemas/member", "", 200)
	expect("GET", "/v2/schemas/members", "", 200)
	for _, route := range []string{`"/v2/images/{id}/members"`, `"/v2/images/{id}/members/{member}"`, `"/v2/images/{id}/tags/{tag}"`, `"/v2/images/{id}/actions/deactivate"`} {
		if !strings.Contains(logs.String(), route) {
			t.Fatal("route template not logged:", route)
		}
	}
	if strings.Contains(logs.String(), "guest") {
		t.Fatal("member ID leaked into logs")
	}
}

func TestInfoUsageAndStores(t *testing.T) {
	h, _, _, _ := harness()
	w := request(h, "GET", "/v2/info/usage", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"usage":{}}` {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/v2/info/stores", "/v2/info/stores/detail"} {
		if w = request(h, "GET", path, ""); w.Code != 404 || !strings.Contains(w.Body.String(), "Multi backend is not supported at this site.") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestHealthcheckLikeOsloMiddleware(t *testing.T) {
	h, _, a, _ := harness()
	r := httptest.NewRequest("GET", "/healthcheck", nil) // no token
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "OK" || a.calls != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	old := HealthcheckDisableFile
	HealthcheckDisableFile = t.TempDir() + "/disable"
	defer func() { HealthcheckDisableFile = old }()
	if err := os.WriteFile(HealthcheckDisableFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthcheck", nil))
	if w.Code != 503 || w.Body.String() != "DISABLED BY FILE" {
		t.Fatal(w.Code, w.Body.String())
	}
}
