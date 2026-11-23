package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/notify"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

// RequestIDKey carries the request's X-Openstack-Request-Id (a task's request_id).
type RequestIDKey struct{}

// Importer is the original's interoperable image import (glance-direct,
// web-download) and the tasks API. An accepted import moves the image to
// "importing" with the lock os_glance_import_task and an api_image_import
// task, in one transaction. A worker then writes the staged data through the
// same reserve/stage/publish/commit path as a client upload; on failure the
// image returns to "queued" and the task records the failure, as the
// original's import flow reverts.
type Importer struct {
	cfg      config.ImportSettings
	svc      *Service
	tasks    repository.Tasks
	data     *UploadCoordinator
	client   *http.Client
	now      func() time.Time
	notifier notify.Notifier

	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	slots   chan struct{} // the original's task executor pool (10)
}

// ImportRequest is the body of POST /v2/images/{id}/import.
type ImportRequest struct {
	// Raw is the body as the client sent it (the task's import_req).
	Raw    json.RawMessage `json:"-"`
	Method struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	} `json:"method"`
	Stores    []string `json:"stores"`
	AllStores bool     `json:"all_stores"`
	// AllStoresMustSucceed defaults to true, as the original.
	AllStoresMustSucceed *bool `json:"all_stores_must_succeed"`
}

// importJob is what the worker needs beyond the task.
type importJob struct {
	method, uri string
	roles       []string
	stores      []string
	mustSucceed bool
}

func NewImporter(cfg config.ImportSettings, svc *Service, data *UploadCoordinator, client *http.Client) (*Importer, error) {
	tasks, ok := svc.repo.(repository.Tasks)
	if !ok || data == nil || client == nil || cfg.StagingDir == "" {
		return nil, errors.New("invalid import configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Importer{cfg: cfg, svc: svc, tasks: tasks, data: data, client: client, now: time.Now, notifier: notify.OrNoop(nil),
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, 10)}
	// web-download follows redirects only to URIs that pass the same filter.
	redirects := *client
	redirects.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return m.filter(r.URL)
	}
	m.client = &redirects
	return m, nil
}

func (m *Importer) SetNotifier(n notify.Notifier) { m.notifier = notify.OrNoop(n) }

// Methods is /v2/info/import.
func (m *Importer) Methods() []string { return append([]string(nil), m.cfg.Methods...) }

// Close stops accepting work, cancels running imports and waits for them.
func (m *Importer) Close() {
	m.cancel()
	m.workers.Wait()
}

func (m *Importer) staged(id string) string { return filepath.Join(m.cfg.StagingDir, id) }

func importConflict(err error, id string) error {
	if errors.Is(err, repository.ErrConflict) {
		return model.Fail(model.Conflict, "Image "+id+" is not in a valid state for this operation.")
	}
	return mutationError(err, id)
}

// Stage is PUT /v2/images/{id}/stage: a queued image becomes "uploading"
// and its data is written to the staging directory (node_staging_uri).
func (m *Importer) Stage(ctx context.Context, who auth.Identity, value string, src io.Reader, declared *int64) error {
	if err := who.Check(m.now()); err != nil {
		return err
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return err
	}
	if !slices.Contains(m.cfg.Methods, "glance-direct") {
		return model.Fail(model.NotFound, "The glance-direct import method is not enabled.")
	}
	if declared != nil && *declared > m.data.MaxUploadBytes() {
		return store.ErrTooLarge
	}
	check := func(image model.Image) error {
		if err := m.svc.writable(ctx, who, image); err != nil {
			return err
		}
		if image.Properties[model.ImportTaskProperty] != "" {
			return repository.ErrConflict
		}
		return nil
	}
	if q := m.svc.quota; q != nil {
		if err = q.EnforceStage(ctx, who.ProjectID); err != nil {
			return err
		}
	}
	if err = m.tasks.SetStaged(ctx, id, "queued", "uploading", m.now(), check); err != nil {
		return importConflict(err, id)
	}
	n, err := m.writeStaged(ctx, id, src, declared)
	if err == nil {
		err = m.tasks.StagedSize(context.WithoutCancel(ctx), id, n)
	}
	if err != nil {
		_ = os.Remove(m.staged(id))
		if e := m.tasks.SetStaged(context.WithoutCancel(ctx), id, "uploading", "queued", m.now(), func(model.Image) error { return nil }); e != nil && !errors.Is(e, repository.ErrNotFound) {
			return errors.Join(err, e)
		}
		return err
	}
	return nil
}

func (m *Importer) writeStaged(ctx context.Context, id string, src io.Reader, declared *int64) (int64, error) {
	if err := os.MkdirAll(m.cfg.StagingDir, 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(m.cfg.StagingDir, "."+id+".")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(contextReader{ctx, src}, m.data.MaxUploadBytes()+1))
	if err == nil && n > m.data.MaxUploadBytes() {
		err = store.ErrTooLarge
	}
	if err == nil && declared != nil && n != *declared {
		err = store.ErrSizeMismatch
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), m.staged(id))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// filter is the original's validate_import_uri with [import_filtering_opts].
func (m *Importer) filter(u *url.URL) error {
	bad := func() error {
		return model.Fail(model.Invalid, "URI for web-download does not pass filtering: "+u.Redacted())
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if host == "" || u.User != nil {
		return bad()
	}
	in := func(list []string, v string) bool {
		return slices.ContainsFunc(list, func(s string) bool { return strings.EqualFold(s, v) })
	}
	if len(m.cfg.AllowedSchemes) > 0 && !in(m.cfg.AllowedSchemes, scheme) || in(m.cfg.DisallowedSchemes, scheme) {
		return bad()
	}
	if len(m.cfg.AllowedHosts) > 0 && !in(m.cfg.AllowedHosts, host) || in(m.cfg.DisallowedHosts, host) {
		return bad()
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	p, err := strconv.Atoi(port)
	if err != nil || len(m.cfg.AllowedPorts) > 0 && !slices.Contains(m.cfg.AllowedPorts, p) || slices.Contains(m.cfg.DisallowedPorts, p) {
		return bad()
	}
	return nil
}

// Import is POST /v2/images/{id}/import (202 Accepted).
func (m *Importer) Import(ctx context.Context, who auth.Identity, value string, req ImportRequest) (model.Task, error) {
	if err := who.Check(m.now()); err != nil {
		return model.Task{}, err
	}
	id, err := model.CanonicalID(value)
	if err != nil {
		return model.Task{}, err
	}
	method := req.Method.Name
	if !slices.Contains(m.cfg.Methods, method) || method == "glance-download" {
		return model.Task{}, model.Fail(model.Invalid, "Import method "+method+" is not valid for this image.")
	}
	stores, err := m.importStores(ctx, id, method, req)
	if err != nil {
		return model.Task{}, err
	}
	must := req.AllStoresMustSucceed == nil || *req.AllStoresMustSucceed
	from := []string{"uploading"}
	if method == "copy-image" {
		from = []string{"active"}
	}
	if method == "web-download" {
		u, err := url.Parse(req.Method.URI)
		if err != nil || req.Method.URI == "" {
			return model.Task{}, model.Fail(model.Invalid, "URI for web-download is required.")
		}
		if err = m.filter(u); err != nil {
			return model.Task{}, err
		}
		from = []string{"queued"}
	}
	if q := m.svc.quota; q != nil {
		if err = q.EnforceStage(ctx, who.ProjectID); err != nil {
			return model.Task{}, err
		}
	}
	tid, err := uuid.NewRandom()
	if err != nil {
		return model.Task{}, err
	}
	body := req.Raw
	if body == nil {
		body, _ = json.Marshal(req)
	}
	backend := []any{nil}
	if len(stores) > 0 {
		backend = []any{}
		for _, s := range stores {
			backend = append(backend, s)
		}
	}
	input, _ := json.Marshal(map[string]any{"image_id": id, "import_req": body, "backend": backend})
	now := m.now().UTC().Truncate(time.Second)
	in, user := string(input), who.UserID
	task := model.Task{ID: tid.String(), Type: "api_image_import", Status: "pending", Owner: who.ProjectID, ImageID: &id, UserID: &user,
		Input: &in, CreatedAt: now, UpdatedAt: now}
	if rid, ok := ctx.Value(RequestIDKey{}).(string); ok && rid != "" {
		task.RequestID = &rid
	}
	err = m.tasks.BeginImport(ctx, task, from, func(image model.Image) error {
		if method == "copy-image" {
			// copy_image is admin-only by default and needs an active image.
			if err := m.svc.permitImage(ctx, who, "copy_image", image, adminOnly(who, "copy_image")); err != nil {
				return err
			}
			if image.Status != "active" {
				return model.Fail(model.Conflict, "Image needs to be active to be copied")
			}
		} else if err := m.svc.writable(ctx, who, image); err != nil {
			return err
		}
		if image.DiskFormat == nil || image.ContainerFormat == nil || *image.DiskFormat == "" || *image.ContainerFormat == "" {
			return model.Fail(model.Conflict, "'disk_format' and 'container_format' must be set before import.")
		}
		if image.Properties[model.ImportTaskProperty] != "" {
			return model.Fail(model.Conflict, "Image has active task.")
		}
		if method == "glance-direct" && image.Status != "uploading" {
			return model.Fail(model.Conflict, "Image needs to be staged before 'glance-direct' method can be used")
		}
		if method == "web-download" && image.Status != "queued" {
			return model.Fail(model.Conflict, "Image needs to be in 'queued' state to use 'web-download' method")
		}
		return nil
	})
	if err != nil {
		return model.Task{}, importConflict(err, id)
	}
	m.notifier.Notify("task.create", notify.Info, notify.TaskPayload(task))
	if image, err := m.svc.repo.Get(ctx, id); err == nil {
		m.svc.notify("image.update", notify.ImagePayload(image))
	}
	if len(stores) > 0 {
		_ = m.setImage(ctx, id, func(i *model.Image) {
			i.Properties = cloneProps(i.Properties)
			i.Properties["os_glance_importing_to_stores"] = strings.Join(stores, ",")
		})
	}
	m.start(task, importJob{method: method, uri: req.Method.URI, roles: who.Roles, stores: stores, mustSucceed: must})
	return task, nil
}

func (m *Importer) start(task model.Task, job importJob) {
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		select {
		case m.slots <- struct{}{}:
		case <-m.ctx.Done():
			return // restarted: Recover will close the task
		}
		defer func() { <-m.slots }()
		m.run(task, job)
	}()
}

func (m *Importer) setTask(task *model.Task, status string, result any, message string) {
	task.Status, task.Message, task.UpdatedAt = status, message, m.now().UTC().Truncate(time.Second)
	if result != nil {
		b, _ := json.Marshal(result)
		r := string(b)
		task.Result = &r
	}
	if status == "success" || status == "failure" {
		e := task.UpdatedAt.Add(m.cfg.TaskTimeToLive)
		task.ExpiresAt = &e
	}
	_ = m.tasks.UpdateTask(context.WithoutCancel(m.ctx), *task)
	m.notifier.Notify("task."+status, notify.Info, notify.TaskPayload(*task))
}

func (m *Importer) run(task model.Task, job importJob) {
	method, uri, roles := job.method, job.uri, job.roles
	id := *task.ImageID
	ctx := m.ctx
	m.setTask(&task, "processing", nil, "")
	fail := func(err error) {
		_ = os.Remove(m.staged(id))
		if _, e := m.tasks.ReleaseImport(context.WithoutCancel(ctx), id, task.ID, "queued", false, m.now()); e != nil {
			err = errors.Join(err, e)
		}
		m.setTask(&task, "failure", nil, importMessage(err))
	}
	if method == "copy-image" {
		err := m.copyStores(ctx, id, job.stores, job.mustSucceed)
		if _, e := m.tasks.ReleaseImport(context.WithoutCancel(ctx), id, task.ID, "active", false, m.now()); e != nil {
			err = errors.Join(err, e)
		}
		if err != nil {
			m.setTask(&task, "failure", nil, importMessage(err))
			return
		}
		m.setTask(&task, "success", map[string]string{"image_id": id}, "")
		return
	}
	if method == "web-download" {
		if err := m.download(ctx, id, uri); err != nil {
			fail(err)
			return
		}
	}
	user0 := ""
	if task.UserID != nil {
		user0 = *task.UserID
	}
	if err := m.runPlugins(ctx, auth.Identity{UserID: user0, ProjectID: task.Owner, Roles: roles}, id); err != nil {
		fail(err)
		return
	}
	f, err := os.Open(m.staged(id))
	if err != nil {
		fail(errors.New("staged image data not found"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fail(err)
		return
	}
	held, err := m.tasks.ReleaseImport(ctx, id, task.ID, "queued", true, m.now())
	if err != nil || !held {
		fail(errors.Join(errors.New("image import lock lost"), err))
		return
	}
	user := ""
	if task.UserID != nil {
		user = *task.UserID
	}
	who := auth.Identity{UserID: user, ProjectID: task.Owner, Roles: []string{"member", "reader"}, ExpiresAt: m.now().Add(24 * time.Hour)}
	size := info.Size()
	first := ""
	if len(job.stores) > 0 {
		first = job.stores[0]
	}
	if _, err = m.data.ImportUploadTo(ctx, who, id, f, &size, task.ID, first); err != nil {
		fail(err)
		return
	}
	if len(job.stores) > 0 {
		_ = m.setImage(ctx, id, func(i *model.Image) {
			i.Properties = cloneProps(i.Properties)
			i.Properties["os_glance_importing_to_stores"] = strings.Join(job.stores[1:], ",")
		})
		if err = m.copyStores(ctx, id, job.stores[1:], job.mustSucceed); err != nil {
			_ = os.Remove(m.staged(id))
			_, _ = m.tasks.ReleaseImport(context.WithoutCancel(ctx), id, task.ID, "queued", false, m.now())
			m.setTask(&task, "failure", nil, importMessage(err))
			return
		}
	}
	_ = os.Remove(m.staged(id))
	if _, err = m.tasks.ReleaseImport(context.WithoutCancel(ctx), id, task.ID, "queued", false, m.now()); err != nil {
		m.setTask(&task, "failure", nil, importMessage(err))
		return
	}
	m.setTask(&task, "success", map[string]string{"image_id": id}, fmt.Sprintf("Copied %d MiB", size>>20))
}

func importMessage(err error) string {
	var domain *model.Error
	if errors.As(err, &domain) {
		return domain.Message
	}
	switch {
	case errors.Is(err, store.ErrTooLarge), errors.Is(err, store.ErrStoreFull):
		return "Image size or storage limit exceeded."
	case errors.Is(err, context.Canceled):
		return "Import interrupted."
	}
	return "Image import failed: " + strings.SplitN(err.Error(), "\n", 2)[0]
}

// download is the original's web-download: the URI's data is staged first.
func (m *Importer) download(ctx context.Context, id, uri string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	res, err := m.client.Do(req)
	if err != nil {
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return fmt.Errorf("cannot resolve %s", dns.Name)
		}
		return errors.New("web-download request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("web-download returned HTTP %d", res.StatusCode)
	}
	var declared *int64
	if res.ContentLength >= 0 {
		n := res.ContentLength
		declared = &n
	}
	_, err = m.writeStaged(ctx, id, res.Body, declared)
	return err
}

// Recover closes, at startup, the imports a previous process left open (the
// original's stale import lock): a committed image keeps its data, any other
// returns to queued; the task records the outcome. Run after upload recovery.
func (m *Importer) Recover(ctx context.Context) (int, error) {
	open, err := m.tasks.OpenTasks(ctx)
	if err != nil {
		return 0, err
	}
	for _, task := range open {
		if task.ImageID == nil {
			continue
		}
		id := *task.ImageID
		image, err := m.svc.repo.Get(ctx, id)
		if err == nil && image.Status == "active" && image.Properties[model.ImportTaskProperty] == task.ID {
			if _, err = m.tasks.ReleaseImport(ctx, id, task.ID, "queued", false, m.now()); err != nil {
				return 0, err
			}
			m.setTask(&task, "success", map[string]string{"image_id": id}, "")
			continue
		}
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return 0, err
		}
		_ = os.Remove(m.staged(id))
		if _, err = m.tasks.ReleaseImport(ctx, id, task.ID, "queued", false, m.now()); err != nil {
			return 0, err
		}
		m.setTask(&task, "failure", nil, "Import interrupted: the image service restarted.")
	}
	return len(open), nil
}

// DiscardStaged removes an image's staged data after it was deleted.
func (m *Importer) DiscardStaged(id string) { _ = os.Remove(m.staged(id)) }

// Tasks API: tasks_api_access is admin in the original's default policy.

func (m *Importer) GetTask(ctx context.Context, who auth.Identity, id string) (model.Task, error) {
	if err := m.tasksAccess(who); err != nil {
		return model.Task{}, err
	}
	t, err := m.tasks.GetTask(ctx, id)
	if errors.Is(err, repository.ErrTaskNotFound) {
		return t, model.Fail(model.NotFound, "Task with the given ID "+id+" not found")
	}
	return t, err
}

func (m *Importer) ListTasks(ctx context.Context, who auth.Identity, o model.TaskListOptions) ([]model.Task, error) {
	if err := m.tasksAccess(who); err != nil {
		return nil, err
	}
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = min(max(o.Limit, 0), 1000)
		if o.Limit == 0 {
			o.Limit = 25
		}
	}
	if o.SortKey == "" {
		o.SortKey = "created_at"
	}
	if o.SortDir == "" {
		o.SortDir = "desc"
	}
	if !slices.Contains([]string{"created_at", "updated_at", "id", "type", "status"}, o.SortKey) || o.SortDir != "asc" && o.SortDir != "desc" {
		return nil, model.Fail(model.Invalid, "Invalid sort key or direction.")
	}
	if o.Status != "" && !slices.Contains([]string{"pending", "processing", "success", "failure"}, o.Status) {
		return nil, model.Fail(model.Invalid, "Invalid status value: "+o.Status)
	}
	if o.Type != "" && o.Type != "import" && o.Type != "api_image_import" {
		return nil, model.Fail(model.Invalid, "Invalid type value: "+o.Type)
	}
	if o.Marker != "" {
		if _, err := uuid.Parse(o.Marker); err != nil {
			return nil, model.Fail(model.Invalid, "Invalid marker.")
		}
	}
	tasks, err := m.tasks.ListTasks(ctx, o)
	if errors.Is(err, repository.ErrTaskNotFound) {
		return nil, model.Fail(model.Invalid, "Invalid marker. Task could not be found.")
	}
	return tasks, err
}

// ImageTasks is GET /v2/images/{id}/tasks (get_image policy).
func (m *Importer) ImageTasks(ctx context.Context, who auth.Identity, value string) ([]model.Task, error) {
	image, err := m.svc.Get(ctx, who, value)
	if err != nil {
		return nil, err
	}
	return m.tasks.ImageTasks(ctx, image.ID)
}

func (m *Importer) tasksAccess(who auth.Identity) error {
	if err := who.Check(m.now()); err != nil {
		return err
	}
	return Permit(who, "tasks_api_access", nil, func() error {
		if !who.HasRole("admin") {
			return model.Fail(model.Forbidden, "You are not authorized to complete tasks_api_access action.")
		}
		return nil
	}())
}

// LegacyImport is POST /v2/tasks with type "import" (the original's
// task-based import): admin only; it creates the image from
// input.image_properties (disk_format from import_from_format) and fills it
// from input.import_from, as a web-download. The task is returned pending.
func (m *Importer) LegacyImport(ctx context.Context, who auth.Identity, taskType string, input json.RawMessage) (model.Task, error) {
	if err := m.tasksAccess(who); err != nil {
		return model.Task{}, err
	}
	if taskType != "import" {
		return model.Task{}, model.Fail(model.Invalid, "Invalid type value: "+taskType)
	}
	var in struct {
		From       string         `json:"import_from"`
		FromFormat string         `json:"import_from_format"`
		Properties map[string]any `json:"image_properties"`
	}
	if json.Unmarshal(input, &in) != nil || in.From == "" || in.FromFormat == "" || in.Properties == nil {
		return model.Task{}, model.Fail(model.Invalid, "Input does not contain 'import_from', 'import_from_format' and 'image_properties'.")
	}
	u, err := url.Parse(in.From)
	if err != nil {
		return model.Task{}, model.Fail(model.Invalid, "Invalid import_from URI.")
	}
	if err = m.filter(u); err != nil {
		return model.Task{}, err
	}
	str := func(k string) *string {
		if v, ok := in.Properties[k].(string); ok {
			return &v
		}
		return nil
	}
	create := model.CreateInput{Name: str("name"), DiskFormat: &in.FromFormat, ContainerFormat: str("container_format"), Visibility: str("visibility"), Properties: map[string]string{}}
	if create.ContainerFormat == nil {
		bare := "bare"
		create.ContainerFormat = &bare
	}
	for k, v := range in.Properties {
		if s, ok := v.(string); ok && !model.Reserved(k) {
			create.Properties[k] = s
		}
	}
	image, err := m.svc.Create(ctx, who, create)
	if err != nil {
		return model.Task{}, err
	}
	tid, err := uuid.NewRandom()
	if err != nil {
		return model.Task{}, err
	}
	now := m.now().UTC().Truncate(time.Second)
	raw, user := string(input), who.UserID
	task := model.Task{ID: tid.String(), Type: "import", Status: "pending", Owner: who.ProjectID, ImageID: &image.ID, UserID: &user, Input: &raw, CreatedAt: now, UpdatedAt: now}
	if rid, ok := ctx.Value(RequestIDKey{}).(string); ok && rid != "" {
		task.RequestID = &rid
	}
	if err = m.tasks.BeginImport(ctx, task, []string{"queued"}, func(model.Image) error { return nil }); err != nil {
		return model.Task{}, importConflict(err, image.ID)
	}
	m.notifier.Notify("task.create", notify.Info, notify.TaskPayload(task))
	m.start(task, importJob{method: "web-download", uri: in.From, roles: who.Roles})
	return task, nil
}
