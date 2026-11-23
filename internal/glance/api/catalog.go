package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

const patchMediaType = "application/openstack-images-v2.1-json-patch"

func parseList(raw string) (model.ListOptions, error) {
	o := model.ListOptions{Limit: model.DefaultLimit, Filters: map[string]string{}}
	bad := func() (model.ListOptions, error) { return o, model.Fail(model.Invalid, "Invalid list query.") }
	if len(raw) > 16384 {
		return bad()
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return bad()
	}
	for key, values := range q {
		if key != "tag" && key != "sort_key" && key != "sort_dir" && len(values) != 1 {
			return bad()
		}
	}
	if v, ok := q["limit"]; ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		if n > model.MaxLimit {
			n = model.MaxLimit
		}
		o.Limit = int(n)
	}
	if v, ok := q["marker"]; ok {
		if v[0] == "" {
			return bad()
		}
		o.Marker = v[0]
	}
	if v, ok := q["sort"]; ok {
		if q.Has("sort_key") || q.Has("sort_dir") {
			return bad()
		}
		for _, part := range strings.Split(strings.TrimSpace(v[0]), ",") {
			key, dir, found := strings.Cut(part, ":")
			if !found || dir == "" {
				dir = "desc"
			}
			o.Sort = append(o.Sort, model.Sort{Key: strings.TrimSpace(key), Direction: strings.TrimSpace(dir)})
		}
	} else {
		keys, dirs := q["sort_key"], q["sort_dir"]
		if len(dirs) > 1 && len(dirs) != len(keys) {
			return bad()
		}
		if len(keys) == 0 {
			keys = []string{"created_at"}
		}
		if len(dirs) == 1 {
			o.TieDirection = strings.TrimSpace(dirs[0])
		}
		for n, key := range keys {
			dir := "desc"
			if len(dirs) == 1 {
				dir = dirs[0]
			} else if len(dirs) > 1 {
				dir = dirs[n]
			}
			o.Sort = append(o.Sort, model.Sort{Key: strings.TrimSpace(key), Direction: strings.TrimSpace(dir)})
		}
	}
	for _, tag := range q["tag"] {
		o.Tags = append(o.Tags, strings.TrimSpace(tag))
	}
	for _, key := range []string{"limit", "marker", "sort", "sort_key", "sort_dir", "tag"} {
		q.Del(key)
	}
	if q.Has("member_status") {
		o.MemberStatus = q.Get("member_status")
		q.Del("member_status")
	}
	for key, values := range q {
		o.Filters[key] = values[0]
	}
	return o.Validate()
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	opts, err := parseList(r.URL.RawQuery)
	if err != nil {
		h.failure(w, err)
		return
	}
	images, err := h.service.List(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), opts)
	if err != nil {
		h.failure(w, err)
		return
	}
	values := make([]map[string]any, 0, len(images))
	for _, image := range images {
		values = append(values, representation(image))
	}
	h.decorate(r, values...)
	q := r.URL.Query()
	q.Del("marker")
	first := "/v2/images"
	if encoded := q.Encode(); encoded != "" {
		first += "?" + encoded
	}
	out := map[string]any{"images": values, "first": first, "schema": "/v2/schemas/images"}
	// Match Glance: a full page advertises next even if the following page is empty.
	if opts.Limit > 0 && len(images) == opts.Limit {
		q.Set("marker", images[len(images)-1].ID)
		out["next"] = "/v2/images?" + q.Encode()
	}
	h.json(w, 200, out)
}

func decodePointer(path string) (string, error) {
	bad := func() (string, error) {
		return "", model.Fail(model.Invalid, "Only a single JSON Pointer token is supported.")
	}
	// /locations/<position> reaches the original's location rules.
	if strings.HasPrefix(path, "/locations/") {
		return "locations", nil
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path[1:], "/") {
		return bad()
	}
	for n := 1; n < len(path); n++ {
		if path[n] == '~' {
			n++
			if n >= len(path) || (path[n] != '0' && path[n] != '1') {
				return bad()
			}
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(path[1:], "~1", "/"), "~0", "~"), nil
}

func patchObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, model.Fail(model.Invalid, "Patch operations must be objects.")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, model.Fail(model.Invalid, "Invalid operation member.")
		}
		if _, ok := fields[name]; ok {
			return nil, model.Fail(model.Invalid, "Duplicate operation member.")
		}
		if name != "op" && name != "path" && name != "value" {
			return nil, model.Fail(model.Invalid, "Unsupported operation member.")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

func decodePatch(data []byte) ([]model.PatchOperation, error) {
	bad := func() ([]model.PatchOperation, error) {
		return nil, model.Fail(model.Invalid, "Expected a JSON Patch array of add, replace or remove operations.")
	}
	var raw []json.RawMessage
	if !utf8.Valid(data) || json.Unmarshal(data, &raw) != nil || raw == nil || len(raw) > 128 {
		return bad()
	}
	ops := make([]model.PatchOperation, 0, len(raw))
	for _, item := range raw {
		fields, err := patchObject(item)
		if err != nil {
			return bad()
		}
		var op, path *string
		if json.Unmarshal(fields["op"], &op) != nil || op == nil || json.Unmarshal(fields["path"], &path) != nil || path == nil {
			return bad()
		}
		if *op != "add" && *op != "replace" && *op != "remove" {
			return bad()
		}
		key, err := decodePointer(*path)
		if err != nil {
			return nil, err
		}
		value, exists := fields["value"]
		if *op != "remove" && !exists {
			return bad()
		}
		ops = append(ops, model.PatchOperation{Op: *op, Key: key, Value: value})
	}
	return ops, nil
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != patchMediaType {
		w.Header().Set("Accept-Patch", patchMediaType)
		h.status(w, 415, "Unsupported JSON Patch media type.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			h.status(w, 413, "Request body limit exceeded.")
		} else {
			h.status(w, 400, "Cannot read patch body.")
		}
		return
	}
	ops, err := decodePatch(data)
	if err != nil {
		h.failure(w, err)
		return
	}
	image, err := h.service.Patch(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), r.PathValue("id"), ops)
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, 200, representation(image))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if h.binary != nil {
		// Never fall back to metadata-only deletion when byte storage is enabled.
		deleter, ok := h.binary.(interface {
			DeleteImage(context.Context, auth.Identity, string) error
		})
		if !ok {
			h.binaryFailure(w, errors.New("deletion coordinator unavailable"))
			return
		}
		if err := deleter.DeleteImage(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), r.PathValue("id")); err != nil {
			h.binaryFailure(w, err)
			return
		}
		if id, err := model.CanonicalID(r.PathValue("id")); err == nil {
			if h.imports != nil { // staged data of an image deleted before its import
				h.imports.DiscardStaged(id)
			}
			if h.cache != nil { // as the original's cache middleware on DELETE
				_ = h.cache.Delete(r.Context(), id)
				_ = h.cache.Unqueue(id)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.service.Delete(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), r.PathValue("id")); err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
