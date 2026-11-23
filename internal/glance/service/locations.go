package service

import (
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/google/uuid"
)

// The original's location API (2024.1): GET /v2/images/{id}/locations
// (fetch_image_location: service role) and POST (add_image_location:
// service, or a member of the owner with an http store, which this
// single filesystem store does not have). A location_import task hashes the
// data, checks validation_data and activates the image.

type locationRepo interface {
	ImageLocations(ctx context.Context, imageID string) ([]model.Location, error)
	BeginLocationImport(ctx context.Context, t model.Task, check func(model.Image) error) error
	CommitLocation(ctx context.Context, imageID, taskID, url string, size int64, md5, sha512 string, now time.Time) error
	StoreLocation(imageID string) string
}

func (m *Importer) locations() (locationRepo, error) {
	r, ok := m.tasks.(locationRepo)
	if !ok {
		return nil, errors.New("locations are not available")
	}
	return r, nil
}

func (m *Importer) Locations(ctx context.Context, who auth.Identity, value string) ([]model.Location, error) {
	repo, err := m.locations()
	if err != nil {
		return nil, err
	}
	image, err := m.svc.Get(ctx, who, value)
	if err != nil {
		return nil, err
	}
	var def error
	if !who.HasRole("service") && !who.HasRole("admin") { // rule:service_api
		def = model.Fail(model.Forbidden, "You are not authorized to complete fetch_image_location action.")
	}
	if err = m.svc.permitImage(ctx, who, "fetch_image_location", image, def); err != nil {
		return nil, err
	}
	return repo.ImageLocations(ctx, image.ID)
}

// ValidationData is add_location's validation_data.
type ValidationData struct {
	Algo  string `json:"os_hash_algo"`
	Value string `json:"os_hash_value"`
}

func (m *Importer) AddLocation(ctx context.Context, who auth.Identity, value, url string, v *ValidationData) error {
	repo, err := m.locations()
	if err != nil {
		return err
	}
	image, err := m.svc.Get(ctx, who, value)
	if err != nil {
		return err
	}
	if image.Status != "queued" {
		return model.Fail(model.Conflict, "It's not allowed to add locations if image status is "+image.Status+".")
	}
	service := who.HasRole("service")
	var def error
	if !service && !who.HasRole("admin") && (!who.HasRole("member") || image.Owner != who.ProjectID) {
		def = model.Fail(model.Forbidden, "You are not authorized to complete add_image_location action.")
	}
	if err = m.svc.permitImage(ctx, who, "add_image_location", image, def); err != nil {
		return err
	}
	if !service {
		return model.Fail(model.Forbidden, "http store must be enabled to use location API by normal user.")
	}
	if v != nil {
		if v.Algo != "sha512" || len(v.Value) != 128 {
			return model.Fail(model.Invalid, "os_hash_value: "+v.Value+" is not valid hexadecimal value.")
		}
		if _, err := hex.DecodeString(v.Value); err != nil {
			return model.Fail(model.Invalid, "os_hash_value: "+v.Value+" is not valid hexadecimal value.")
		}
	}
	// The single filesystem store holds an image's data in its own file.
	if url != repo.StoreLocation(image.ID) {
		return model.Fail(model.Invalid, "Invalid location: "+url)
	}
	tid, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	now := m.now().UTC().Truncate(time.Second)
	in, _ := json.Marshal(map[string]any{"image_id": image.ID, "loc_url": url, "validation_data": v})
	input, user := string(in), who.UserID
	id := image.ID
	task := model.Task{ID: tid.String(), Type: "location_import", Status: "pending", Owner: who.ProjectID, ImageID: &id, UserID: &user, Input: &input, CreatedAt: now, UpdatedAt: now}
	if rid, ok := ctx.Value(RequestIDKey{}).(string); ok && rid != "" {
		task.RequestID = &rid
	}
	err = repo.BeginLocationImport(ctx, task, func(i model.Image) error {
		if i.Status != "queued" {
			return model.Fail(model.Conflict, "It's not allowed to add locations if image status is "+i.Status+".")
		}
		if i.Properties[model.ImportTaskProperty] != "" {
			return model.Fail(model.Conflict, "New operation on image '"+i.ID+"' is not permitted as prior operation is still in progress")
		}
		return nil
	})
	if err != nil {
		return importConflict(err, image.ID)
	}
	m.notifier.Notify("task.create", notify.Info, notify.TaskPayload(task))
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		m.runLocation(repo, task, url, v)
	}()
	return nil
}

func (m *Importer) runLocation(repo locationRepo, task model.Task, url string, v *ValidationData) {
	id := *task.ImageID
	m.setTask(&task, "processing", nil, "")
	fail := func(msg string) {
		_, _ = m.tasks.ReleaseImport(context.WithoutCancel(m.ctx), id, task.ID, "queued", false, m.now())
		m.setTask(&task, "failure", nil, msg)
	}
	f, err := os.Open(strings.TrimPrefix(url, "file://"))
	if err != nil {
		fail("Image location data not found")
		return
	}
	defer f.Close()
	h5, h512 := md5.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(h5, h512), contextReader{m.ctx, f})
	if err != nil {
		fail("Unable to read the image location data")
		return
	}
	sum := hex.EncodeToString(h512.Sum(nil))
	if v != nil && v.Value != sum {
		fail("os_hash_value: " + v.Value + " not matched with actual os_hash_value: " + sum)
		return
	}
	if err = repo.CommitLocation(m.ctx, id, task.ID, url, n, hex.EncodeToString(h5.Sum(nil)), sum, m.now()); err != nil {
		fail(importMessage(err))
		return
	}
	m.setTask(&task, "success", map[string]string{"image_id": id}, "")
}

// ImageLocationList is the image's locations for its view (no policy:
// the view itself was authorized).
// LocationsFor is ImageLocationList for several images in one query.
func (m *Importer) LocationsFor(ctx context.Context, ids []string) (map[string][]model.Location, error) {
	return locationsFor(ctx, m.tasks, ids)
}

type batchLocations interface {
	LocationsFor(ctx context.Context, ids []string) (map[string][]model.Location, error)
}

func locationsFor(ctx context.Context, repo any, ids []string) (map[string][]model.Location, error) {
	if b, ok := repo.(batchLocations); ok {
		return b.LocationsFor(ctx, ids)
	}
	l, ok := repo.(interface {
		ImageLocations(ctx context.Context, imageID string) ([]model.Location, error)
	})
	if !ok {
		return nil, errors.New("locations are not available")
	}
	out := map[string][]model.Location{}
	for _, id := range ids {
		list, err := l.ImageLocations(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = list
	}
	return out, nil
}

func (m *Importer) ImageLocationList(ctx context.Context, imageID string) ([]model.Location, error) {
	repo, err := m.locations()
	if err != nil {
		return nil, err
	}
	return repo.ImageLocations(ctx, imageID)
}
