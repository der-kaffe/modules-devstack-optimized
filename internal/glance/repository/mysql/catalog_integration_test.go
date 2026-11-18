//go:build integration

package mysql_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

// MUTATING, opt-in, NEW EMPTY independent schema only. Identities below are
// explicit service-policy fixtures, not evidence of live Keystone validation.
// This suite does not provision users/grants or reuse the partial-DDL schema.
func TestMySQLCatalogOperations(t *testing.T) {
	if os.Getenv("GLANCE_GO_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable MySQL opt-in required")
	}
	secret, err := config.ReadSecret(os.Getenv("GLANCE_GO_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(secret))
	if err = mysql.ValidateTestDSN(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = mysql.CheckAccount(ctx, db, "migrate"); err != nil {
		t.Fatal(err)
	}
	var tables int
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables) != nil || tables != 0 {
		t.Fatal("catalog suite requires a NEW EMPTY disposable schema")
	}
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := mysql.New(db, 5*time.Second)
	if err := r.PrepareStatements(ctx); err != nil {
		t.Fatal(err)
	}
	s := service.New(r, time.Now)
	who := auth.Identity{UserID: "catalog-fixture", ProjectID: "project-a", Roles: []string{"member", "reader"}, ExpiresAt: time.Now().Add(time.Hour)}
	other := who
	other.ProjectID = "project-b"
	admin := other
	admin.Roles = []string{"admin"}
	options := model.ListOptions{Limit: 25}
	page, err := s.List(ctx, who, options)
	if err != nil || len(page) != 0 {
		t.Fatal("empty catalog", err)
	}
	var own []model.Image
	for _, name := range []string{"alpha", "bravo", "delta"} {
		name := name
		i, err := s.Create(ctx, who, model.CreateInput{Name: &name, Tags: []string{"x"}, Properties: map[string]string{"flavor": "one"}})
		if err != nil {
			t.Fatal(err)
		}
		own = append(own, i)
	}
	for _, visibility := range []string{"private", "shared", "public", "community"} {
		name := "z-" + visibility
		vis := visibility
		if _, err = s.Create(ctx, admin, model.CreateInput{Name: &name, Visibility: &vis}); err != nil {
			t.Fatal(err)
		}
	}
	community := "community"
	name := "epsilon"
	if _, err = s.Create(ctx, who, model.CreateInput{Name: &name, Visibility: &community}); err != nil {
		t.Fatal(err)
	}
	hidden := true
	name = "hidden"
	if _, err = s.Create(ctx, who, model.CreateInput{Name: &name, Hidden: &hidden}); err != nil {
		t.Fatal(err)
	}
	page, err = s.List(ctx, who, options)
	if err != nil || len(page) != 5 {
		t.Fatal("visibility/default hidden filtering", len(page), err)
	}
	for _, i := range page {
		if i.Owner != who.ProjectID && i.Visibility != "public" {
			t.Fatal("foreign private/shared/community disclosed by default")
		}
	}
	options.Sort = []model.Sort{{Key: "name", Direction: "asc"}}
	options.Limit = 2
	first, err := s.List(ctx, who, options)
	if err != nil || len(first) != 2 || first[0].ID != own[0].ID || first[1].ID != own[1].ID {
		t.Fatal("sort/page", first, err)
	}
	options.Marker = first[1].ID
	second, err := s.List(ctx, who, options)
	if err != nil || len(second) != 2 || second[0].ID != own[2].ID {
		t.Fatal("marker page", second, err)
	}
	options.Marker = second[1].ID
	last, err := s.List(ctx, who, options)
	if err != nil || len(last) != 1 || last[0].Visibility != "public" {
		t.Fatal("last page", last, err)
	}
	options.Marker = "00000000-0000-4000-8000-000000000000"
	if _, err = s.List(ctx, who, options); err == nil {
		t.Fatal("missing marker accepted")
	}
	options = model.ListOptions{Limit: 25, Filters: map[string]string{"visibility": "community"}}
	page, err = s.List(ctx, who, options)
	if err != nil || len(page) != 2 {
		t.Fatal("community filter", len(page), err)
	}
	options.Filters = map[string]string{"os_hidden": "true"}
	page, err = s.List(ctx, who, options)
	if err != nil || len(page) != 1 || !page[0].Hidden {
		t.Fatal("hidden filter", err)
	}
	options.Filters = map[string]string{"name": "alpha", "flavor": "one"}
	options.Tags = []string{"x"}
	page, err = s.List(ctx, who, options)
	if err != nil || len(page) != 1 || page[0].ID != own[0].ID {
		t.Fatal("property/name/tag filter", err)
	}
	op := func(verb, key, value string) model.PatchOperation {
		return model.PatchOperation{Op: verb, Key: key, Value: json.RawMessage(value)}
	}
	patched, err := s.Patch(ctx, who, own[0].ID, []model.PatchOperation{op("add", "description", `"new"`), op("replace", "flavor", `"two"`), op("replace", "tags", `["x","y"]`)})
	if err != nil {
		t.Fatal(err)
	}
	page, err = s.List(ctx, who, model.ListOptions{Limit: 25, Tags: []string{"x", "y"}})
	if err != nil || len(page) != 1 || page[0].ID != patched.ID {
		t.Fatal("AND tags", err)
	}
	before, err := r.Get(ctx, patched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Patch(ctx, who, patched.ID, []model.PatchOperation{op("replace", "name", `"partial"`), op("replace", "min_ram", `-1`)}); err == nil {
		t.Fatal("invalid second operation accepted")
	}
	after, err := r.Get(ctx, patched.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("invalid batch partially persisted", err)
	}
	// Force a genuine child SQL error AFTER parent UPDATE and child DELETEs.
	_, err = r.Update(ctx, patched.ID, func(i model.Image) (model.Image, error) {
		name := "partial-sql"
		i.Name = &name
		i.UpdatedAt = time.Now().UTC()
		i.Properties["too-large"] = strings.Repeat("x", 65536)
		return i, nil
	})
	if err == nil {
		t.Fatal("strict SQL TEXT overflow accepted")
	}
	after, err = r.Get(ctx, patched.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("child failure did not rollback parent/children", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"concurrent-one", "concurrent-two"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			_, err := s.Patch(ctx, who, patched.ID, []model.PatchOperation{op("add", key, `"kept"`)})
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	r = mysql.New(db, 5*time.Second)
	s = service.New(r, time.Now)
	after, err = r.Get(ctx, patched.ID)
	if err != nil || after.Properties["concurrent-one"] != "kept" || after.Properties["concurrent-two"] != "kept" || len(after.Tags) != 2 {
		t.Fatal("concurrency/persistence", err)
	}
	if _, err = s.Patch(ctx, who, patched.ID, []model.PatchOperation{op("remove", "description", ``), op("replace", "protected", `true`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, who, patched.ID); err == nil {
		t.Fatal("protected image deleted")
	}
	reader := who
	reader.Roles = []string{"reader"}
	if _, err = s.Patch(ctx, reader, patched.ID, nil); err == nil {
		t.Fatal("reader patched")
	}
	if err = s.Delete(ctx, reader, patched.ID); err == nil {
		t.Fatal("reader deleted")
	}
	if _, err = s.Patch(ctx, other, patched.ID, nil); err == nil {
		t.Fatal("foreign owner patched")
	}
	if err = s.Delete(ctx, other, patched.ID); err == nil {
		t.Fatal("foreign owner deleted")
	}
	if _, err = s.Patch(ctx, who, patched.ID, []model.PatchOperation{op("replace", "protected", `false`)}); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, who, patched.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Get(ctx, patched.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatal("deleted image readable", err)
	}
	if err = s.Delete(ctx, who, patched.ID); err == nil {
		t.Fatal("repeated delete succeeded")
	}
	page, err = s.List(ctx, who, model.ListOptions{Limit: 25, Filters: map[string]string{"id": patched.ID}})
	if err != nil || len(page) != 0 {
		t.Fatal("deleted image listed", err)
	}
	var deleted bool
	var status string
	var properties, tags int
	if db.QueryRowContext(ctx, "SELECT deleted,status FROM images WHERE id=?", patched.ID).Scan(&deleted, &status) != nil || !deleted || status != "deleted" {
		t.Fatal("tombstone missing")
	}
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM image_properties WHERE image_id=?", patched.ID).Scan(&properties) != nil || properties != 3 {
		t.Fatal("tombstone properties not retained")
	}
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM image_tags WHERE image_id=?", patched.ID).Scan(&tags) != nil || tags != 2 {
		t.Fatal("tombstone tags not retained")
	}
	// All fixture data intentionally remains for review. No automatic cleanup.
}
