package service

import (
	"context"
	"slices"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

func TestImportStoresFollowTheOriginal(t *testing.T) {
	ctx := context.Background()
	m, _ := pluginFixture(t, "raw")
	m.data = &UploadCoordinator{}
	if _, err := m.importStores(ctx, sharedID, "glance-direct", ImportRequest{Stores: []string{"x"}}); err == nil {
		t.Fatal("stores accepted without enabled_backends")
	}
	if s, err := m.importStores(ctx, sharedID, "glance-direct", ImportRequest{}); err != nil || s != nil {
		t.Fatal("single store", s, err)
	}
	m.data.SetBackends("local", []string{"local", "fast", "cheap"})
	if s, _ := m.importStores(ctx, sharedID, "glance-direct", ImportRequest{}); !slices.Equal(s, []string{"local"}) {
		t.Fatal("default store", s)
	}
	if s, _ := m.importStores(ctx, sharedID, "web-download", ImportRequest{AllStores: true}); !slices.Equal(s, []string{"local", "fast", "cheap"}) {
		t.Fatal("all stores", s)
	}
	if _, err := m.importStores(ctx, sharedID, "glance-direct", ImportRequest{Stores: []string{"nowhere"}}); domainKind(err) != model.Conflict {
		t.Fatal("unknown store accepted")
	}
	if _, err := m.importStores(ctx, sharedID, "copy-image", ImportRequest{}); err == nil {
		t.Fatal("copy-image without stores accepted")
	}
	if _, err := m.importStores(ctx, sharedID, "glance-direct", ImportRequest{Stores: []string{"fast"}, AllStores: true}); err == nil {
		t.Fatal("stores with all_stores accepted")
	}
}
