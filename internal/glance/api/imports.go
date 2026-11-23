package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// WithImports enables /stage, /import, /v2/info/import and the tasks API.
func WithImports(m *service.Importer) Option {
	return func(h *Handler) error {
		if m == nil {
			return errors.New("invalid import service")
		}
		h.imports = m
		return nil
	}
}

func (h *Handler) importInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	methods := []string{}
	if h.imports != nil {
		methods = h.imports.Methods()
	}
	h.json(w, 200, map[string]any{"import-methods": map[string]any{"description": "Import methods available.", "type": "array", "value": methods}})
}

func (h *Handler) stage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		h.status(w, 405, "Method not implemented.")
		return
	}
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/octet-stream" {
		h.status(w, 415, "Expected Content-Type application/octet-stream.")
		return
	}
	expected, err := declaredSize(r)
	if err != nil {
		h.binaryFailure(w, err)
		return
	}
	var src io.Reader = http.NoBody
	if r.Body != nil {
		src = r.Body
	}
	if err = h.imports.Stage(r.Context(), who(r), r.PathValue("id"), src, expected); err != nil {
		if errors.Is(err, store.ErrTooLarge) || errors.Is(err, store.ErrSizeMismatch) {
			h.binaryFailure(w, err)
			return
		}
		h.failure(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Handler) importImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.status(w, 405, "Method not implemented.")
		return
	}
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	var raw json.RawMessage
	if !h.readObject(w, r, &raw) {
		return
	}
	var req service.ImportRequest
	if json.Unmarshal(raw, &req) != nil {
		h.status(w, 400, "Expected a JSON object.")
		return
	}
	req.Raw = raw
	if req.Method.Name == "" {
		h.status(w, 400, "Import method name is required.")
		return
	}
	if _, err := h.imports.Import(r.Context(), who(r), r.PathValue("id"), req); err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(202)
}

func taskRepresentation(t model.Task, full bool) map[string]any {
	out := map[string]any{"id": t.ID, "type": t.Type, "status": t.Status, "owner": t.Owner,
		"created_at": t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), "updated_at": t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"image_id": t.ImageID, "request_id": t.RequestID, "user_id": t.UserID, "self": "/v2/tasks/" + t.ID, "schema": "/v2/schemas/task"}
	if t.ExpiresAt != nil {
		out["expires_at"] = t.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	} else {
		out["expires_at"] = nil
	}
	if full {
		out["input"], out["result"], out["message"] = rawJSON(t.Input), rawJSON(t.Result), t.Message
	}
	return out
}

func rawJSON(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return json.RawMessage(*s)
}

func (h *Handler) tasksList(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		t, err := h.imports.LegacyImport(r.Context(), who(r), body.Type, body.Input)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 201, taskRepresentation(t, true))
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
		return
	}
	q := r.URL.Query()
	o := model.TaskListOptions{Marker: q.Get("marker"), SortKey: q.Get("sort_key"), SortDir: q.Get("sort_dir"), Type: q.Get("type"), Status: q.Get("status")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			h.status(w, 400, "limit param must be an integer")
			return
		}
		o.Limit = n
	}
	identity := who(r)
	if !identity.HasRole("admin") {
		o.Owner = identity.ProjectID
	}
	tasks, err := h.imports.ListTasks(r.Context(), identity, o)
	if err != nil {
		h.failure(w, err)
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		// The original's task stub: no input, result, message, image,
		// request or user.
		rep := taskRepresentation(t, false)
		delete(rep, "image_id")
		delete(rep, "request_id")
		delete(rep, "user_id")
		out = append(out, rep)
	}
	first := "/v2/tasks"
	if e := withoutMarker(r.URL.RawQuery); e != "" {
		first += "?" + e
	}
	q.Del("marker")
	body := map[string]any{"tasks": out, "first": first, "schema": "/v2/schemas/tasks"}
	limit := o.Limit
	if limit == 0 {
		limit = 25
	}
	if len(tasks) == limit {
		body["next"] = nextLink("/v2/tasks", r.URL.RawQuery, tasks[len(tasks)-1].ID)
	}
	h.json(w, 200, body)
}

func (h *Handler) task(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	t, err := h.imports.GetTask(r.Context(), who(r), r.PathValue("task"))
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, 200, taskRepresentation(t, true))
}

func (h *Handler) imageTasks(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	tasks, err := h.imports.ImageTasks(r.Context(), who(r), r.PathValue("id"))
	if err != nil {
		h.failure(w, err)
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		// As the original's image tasks: the task row, without links.
		rep := taskRepresentation(t, true)
		delete(rep, "self")
		delete(rep, "schema")
		rep["deleted"], rep["deleted_at"] = false, nil
		out = append(out, rep)
	}
	h.json(w, 200, map[string]any{"tasks": out})
}

func (h *Handler) locations(w http.ResponseWriter, r *http.Request) {
	if h.imports == nil {
		h.status(w, 404, "The resource could not be found.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := h.imports.Locations(r.Context(), who(r), r.PathValue("id"))
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, l := range list {
			out = append(out, map[string]any{"url": l.URL, "metadata": json.RawMessage(l.Metadata)})
		}
		h.json(w, 200, out)
	case http.MethodPost:
		var body struct {
			URL            *string                 `json:"url"`
			ValidationData *service.ValidationData `json:"validation_data"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		if body.URL == nil || *body.URL == "" {
			h.status(w, 400, "Provided object does not match schema 'location': 'url' is a required property")
			return
		}
		if err := h.imports.AddLocation(r.Context(), who(r), r.PathValue("id"), *body.URL, body.ValidationData); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(202)
	default:
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
	}
}

// WithLocationViews adds direct_url and/or locations to image views, as
// show_image_direct_url and show_multiple_locations.
func WithLocationViews(directURL, multiple bool) Option {
	return func(h *Handler) error {
		h.directURL, h.multipleLocations = directURL, multiple
		return nil
	}
}

// decorate is the original's _format_image location part: locations
// (show_multiple_locations), direct_url (show_image_direct_url) and, with
// enabled_backends, stores. One query serves the whole page, as the
// original loads locations with the images.
func (h *Handler) decorate(r *http.Request, reps ...map[string]any) {
	views := (h.directURL || h.multipleLocations) && h.imports != nil
	if !views && h.multi == nil {
		return
	}
	ids := make([]string, 0, len(reps))
	for _, rep := range reps {
		if id, _ := rep["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	var all map[string][]model.Location
	var err error
	if h.imports != nil {
		all, err = h.imports.LocationsFor(r.Context(), ids)
	} else {
		all, err = h.multi.LocationsFor(r.Context(), ids)
	}
	if err != nil {
		all = nil
	}
	for _, rep := range reps {
		id, _ := rep["id"].(string)
		list := all[id]
		if views && h.multipleLocations {
			out := make([]map[string]any, 0, len(list))
			for _, l := range list {
				out = append(out, map[string]any{"url": l.URL, "metadata": json.RawMessage(l.Metadata)})
			}
			rep["locations"] = out
		}
		if views && h.directURL && len(list) > 0 {
			rep["direct_url"] = list[0].URL
		}
		if h.multi != nil {
			if stores := service.StoresOf(list); len(stores) > 0 {
				rep["stores"] = strings.Join(stores, ",")
			}
		}
	}
}
