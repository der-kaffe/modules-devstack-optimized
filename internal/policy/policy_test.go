package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsLikeTheOriginal(t *testing.T) {
	e, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Overridden() {
		t.Fatal("no file, no overrides")
	}
	member := Credentials{Roles: []string{"Member", "reader"}, ProjectID: "p"}
	admin := Credentials{Roles: []string{"admin"}, ProjectID: "x"}
	own := map[string]any{"project_id": "p", "owner": "p", "visibility": "private", "member_id": ""}
	other := map[string]any{"project_id": "o", "owner": "o", "visibility": "private", "member_id": ""}
	public := map[string]any{"project_id": "o", "owner": "o", "visibility": "public", "member_id": ""}
	for _, tc := range []struct {
		action string
		target map[string]any
		creds  Credentials
		want   bool
	}{
		{"get_image", own, member, true}, {"get_image", other, member, false}, {"get_image", public, member, true}, {"get_image", other, admin, true},
		{"modify_image", own, member, true}, {"modify_image", public, member, false},
		{"publicize_image", own, member, false}, {"publicize_image", own, admin, true},
		{"deactivate", own, member, true}, {"tasks_api_access", own, member, false}, {"cache_list", own, admin, true},
		{"fetch_image_location", own, Credentials{Roles: []string{"service"}}, true}, {"fetch_image_location", own, member, false},
		{"add_metadef_namespace", own, member, false}, {"add_metadef_namespace", own, admin, true},
		{"unknown_action", own, member, true}, // rule default is ""
	} {
		if got := e.Allowed(tc.action, tc.target, tc.creds); got != tc.want {
			t.Errorf("%s %v %v: got %v", tc.action, tc.target["visibility"], tc.creds.Roles, got)
		}
	}
}

func TestOverridesFromYAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(yaml, []byte("# local overrides\n\"publicize_image\": \"role:member and not 'private':%(visibility)s\"\ncontext_is_admin: 'role:admin or role:cloud_admin'\n\"get_images\": \"!\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := LoadFile(yaml)
	if err != nil || len(o) != 3 {
		t.Fatal(o, err)
	}
	e, err := New(o)
	if err != nil || !e.Overridden() {
		t.Fatal(err)
	}
	member := Credentials{Roles: []string{"member"}, ProjectID: "p"}
	if !e.Allowed("publicize_image", map[string]any{"visibility": "shared"}, member) || e.Allowed("publicize_image", map[string]any{"visibility": "private"}, member) {
		t.Fatal("override not applied")
	}
	if !e.Allowed("tasks_api_access", nil, Credentials{Roles: []string{"cloud_admin"}}) {
		t.Fatal("overridden base rule not used by other rules")
	}
	if e.Allowed("get_images", map[string]any{"project_id": "p"}, Credentials{Roles: []string{"admin"}}) {
		t.Fatal("! must deny")
	}
	if o, err = LoadFile(filepath.Join(dir, "missing.yaml")); err != nil || o != nil {
		t.Fatal("missing file must be no overrides")
	}
	js := filepath.Join(dir, "policy.json")
	_ = os.WriteFile(js, []byte(`{"add_image": "role:admin"}`), 0o600)
	if o, err = LoadFile(js); err != nil || o["add_image"] != "role:admin" {
		t.Fatal(o, err)
	}
	if _, err = New(map[string]string{"x": "role:a and ("}); err == nil {
		t.Fatal("broken rule accepted")
	}
}

func TestLazyTargetValueOnlyWhenRead(t *testing.T) {
	e, err := New(map[string]string{"a": "role:member", "b": "project_id:%(member_id)s"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	target := map[string]any{"member_id": Lazy(func() any { calls++; return "p" })}
	creds := Credentials{Roles: []string{"member"}, ProjectID: "p"}
	if !e.Allowed("a", target, creds) || calls != 0 {
		t.Fatal("lazy value computed by a rule that does not read it", calls)
	}
	if !e.Allowed("b", target, creds) || calls != 1 {
		t.Fatal("lazy value not used", calls)
	}
}
