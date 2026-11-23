package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// Multi-store (enabled_backends): each image file lives in one or more
// stores, one location per store with meta_data {"store": name}.

// storeLocations is what the MySQL repository adds for multi-store.
type storeLocations interface {
	AddStoreLocation(ctx context.Context, id, backend string, now time.Time) error
	RemoveStoreLocation(ctx context.Context, id, backend string, now time.Time) error
	ImageLocations(ctx context.Context, imageID string) ([]model.Location, error)
}

type storeCopier interface {
	Copy(ctx context.Context, from store.Key, to string) (store.Info, error)
}

// MultiStore reports whether enabled_backends is configured.
func (u *UploadCoordinator) MultiStore() bool { return len(u.backends) > 0 }

// StoreNames are the enabled backends in enabled_backends order.
func (u *UploadCoordinator) StoreNames() []string { return append([]string(nil), u.backendOrder...) }

// DefaultStore is [glance_store] default_backend.
func (u *UploadCoordinator) DefaultStore() string { return u.defaultBackend }

func (u *UploadCoordinator) knownStore(name string) bool {
	return name == u.defaultBackend || u.backends[name]
}

// LocationsFor is the images' active locations in one query.
func (u *UploadCoordinator) LocationsFor(ctx context.Context, ids []string) (map[string][]model.Location, error) {
	return locationsFor(ctx, u.transfers, ids)
}

// ImageStores are the stores holding the image's data, in location order.
func (u *UploadCoordinator) ImageStores(ctx context.Context, id string) ([]string, error) {
	l, ok := u.transfers.(storeLocations)
	if !ok {
		return nil, nil
	}
	list, err := l.ImageLocations(ctx, id)
	if err != nil {
		return nil, err
	}
	return StoresOf(list), nil
}

// StoresOf are the stores of locations (meta_data "store"), in order.
func StoresOf(list []model.Location) []string {
	var out []string
	for _, loc := range list {
		var meta map[string]any
		_ = json.Unmarshal([]byte(loc.Metadata), &meta)
		if s, _ := meta["store"].(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// CopyToStore copies an active image's file to another store and adds its
// location (copy-image). The copy must have the image's checksum.
func (u *UploadCoordinator) CopyToStore(ctx context.Context, id, to string) error {
	if !u.knownStore(to) {
		return model.Fail(model.Conflict, "Unknown scheme '"+to+"' found in URI")
	}
	l, ok := u.transfers.(storeLocations)
	c, ok2 := u.bytes.(storeCopier)
	if !ok || !ok2 {
		return ErrUploadUnavailable
	}
	s, err := u.readState(ctx, id)
	if err != nil {
		return err
	}
	if s.Deleted || s.Status != "active" || s.Operation == nil || s.Checksum == nil {
		return model.Fail(model.Conflict, "Image needs to be active to be copied")
	}
	dest, _ := u.backendKey(to)
	from := store.Key{ImageID: id, AttemptID: s.Operation.AttemptID, Backend: s.Backend}
	if from.Backend == dest {
		return nil // already there
	}
	info, err := c.Copy(ctx, from, dest)
	if err != nil {
		return err
	}
	if info.Checksum != *s.Checksum {
		_ = u.bytes.Delete(context.WithoutCancel(ctx), store.Key{ImageID: id, AttemptID: s.Operation.AttemptID, Backend: dest})
		return errors.New("copied data does not match the image checksum")
	}
	if dest == "" {
		dest = u.defaultBackend
	}
	return l.AddStoreLocation(ctx, id, dest, u.now())
}

// DeleteFromStore is DELETE /v2/stores/{store}/{image}: the image's copy in
// one store, never the last one.
func (u *UploadCoordinator) DeleteFromStore(ctx context.Context, who auth.Identity, backend, value string) error {
	if err := who.Check(u.now()); err != nil {
		return err
	}
	if err := Permit(who, "delete_image_location", nil, adminOnly(who, "delete_image_location")); err != nil {
		return err
	}
	if !u.MultiStore() {
		return model.Fail(model.Invalid, "Multi backend is not supported at this site.")
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return err
	}
	if !u.knownStore(backend) {
		return model.Fail(model.Conflict, "Unknown scheme '"+backend+"' found in URI")
	}
	l, ok := u.transfers.(storeLocations)
	if !ok {
		return ErrUploadUnavailable
	}
	s, err := u.readState(ctx, id)
	if err != nil || s.Deleted {
		return notFound(id)
	}
	if s.Status != "active" || s.Operation == nil {
		return model.Fail(model.Conflict, "It's not allowed to remove image data from store if image status is not 'active'")
	}
	stores, err := u.ImageStores(ctx, id)
	if err != nil {
		return err
	}
	if !slices.Contains(stores, backend) {
		return model.Fail(model.NotFound, "Image "+id+" is not present in store "+backend)
	}
	if len(stores) == 1 {
		return model.Fail(model.Conflict, "It's not allowed to delete the last location of an image")
	}
	if err = l.RemoveStoreLocation(ctx, id, backend, u.now()); err != nil {
		return err
	}
	key, _ := u.backendKey(backend)
	if key == "" { // the default store: Delete with "" removes every copy
		return u.deleteDefaultCopy(ctx, id, s.Operation.AttemptID)
	}
	return u.bytes.Delete(ctx, store.Key{ImageID: id, AttemptID: s.Operation.AttemptID, Backend: key})
}

type defaultDeleter interface {
	DeleteDefault(ctx context.Context, key store.Key) error
}

func (u *UploadCoordinator) deleteDefaultCopy(ctx context.Context, id, attempt string) error {
	if d, ok := u.bytes.(defaultDeleter); ok {
		return d.DeleteDefault(ctx, store.Key{ImageID: id, AttemptID: attempt})
	}
	return ErrUploadUnavailable
}

func adminOnly(who auth.Identity, action string) error {
	if who.HasRole("admin") {
		return nil
	}
	return denied(action)
}

// importStores resolves an import request's stores (the original's
// _get_stores): the listed ones, every store with all_stores (less those
// holding the image for copy-image), or the default store.
func (m *Importer) importStores(ctx context.Context, id, method string, req ImportRequest) ([]string, error) {
	u := m.data
	if !u.MultiStore() {
		if len(req.Stores) > 0 || req.AllStores || method == "copy-image" {
			return nil, model.Fail(model.Invalid, "Multiple stores are not supported at this site.")
		}
		return nil, nil
	}
	have, err := u.ImageStores(ctx, id)
	if err != nil {
		return nil, err
	}
	stores := req.Stores
	if req.AllStores {
		if len(req.Stores) > 0 {
			return nil, model.Fail(model.Invalid, "All_stores parameter can't be used with x-image-meta-store header or stores parameter")
		}
		stores = nil
		for _, s := range u.StoreNames() {
			if method != "copy-image" || !slices.Contains(have, s) {
				stores = append(stores, s)
			}
		}
	}
	if len(stores) == 0 {
		if method == "copy-image" {
			if req.AllStores {
				return nil, model.Fail(model.Conflict, "Image is already present at all available stores")
			}
			return nil, model.Fail(model.Invalid, "Stores parameter is required for copy-image")
		}
		stores = []string{u.DefaultStore()}
	}
	for _, s := range stores {
		if !u.knownStore(s) {
			return nil, model.Fail(model.Conflict, "Unknown scheme '"+s+"' found in URI")
		}
		if method == "copy-image" && slices.Contains(have, s) {
			return nil, model.Fail(model.Invalid, "Image is already present at store '"+s+"'")
		}
	}
	return stores, nil
}

// copyStores copies the imported data to each store, tracking
// os_glance_importing_to_stores and os_glance_failed_import as the original;
// with all_stores_must_succeed a failure fails the import.
func (m *Importer) copyStores(ctx context.Context, id string, stores []string, mustSucceed bool) error {
	for n, s := range stores {
		err := m.data.CopyToStore(ctx, id, s)
		rest := strings.Join(stores[n+1:], ",")
		if err != nil {
			if mustSucceed {
				return err
			}
			_ = m.setImage(ctx, id, func(i *model.Image) {
				i.Properties = cloneProps(i.Properties)
				i.Properties["os_glance_importing_to_stores"] = rest
				failed := i.Properties["os_glance_failed_import"]
				if failed != "" {
					failed += ","
				}
				i.Properties["os_glance_failed_import"] = failed + s
			})
			continue
		}
		_ = m.setImage(ctx, id, func(i *model.Image) {
			i.Properties = cloneProps(i.Properties)
			i.Properties["os_glance_importing_to_stores"] = rest
		})
	}
	return nil
}
