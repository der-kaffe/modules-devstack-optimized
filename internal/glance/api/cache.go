package api

import (
	"errors"
	"net/http"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
)

// WithCache enables the original's cache API (/v2/cache), admin only
// (cache_list, cache_image, cache_delete default to role:admin).
func WithCache(c *service.ImageCache) Option {
	return func(h *Handler) error {
		if c == nil {
			return errors.New("invalid image cache")
		}
		h.cache = c
		return nil
	}
}

func (h *Handler) cacheAllowed(w http.ResponseWriter, r *http.Request) bool {
	if h.cache == nil {
		h.status(w, 404, "Caching via API is not supported at this site.")
		return false
	}
	action := map[string]string{http.MethodGet: "cache_list", http.MethodPut: "cache_image", http.MethodDelete: "cache_delete"}[r.Method]
	var def error
	if !who(r).HasRole("admin") {
		def = model.Fail(model.Forbidden, "You are not authorized to complete "+action+" action.")
	}
	if err := service.Permit(who(r), action, nil, def); err != nil {
		h.failure(w, err)
		return false
	}
	return true
}

func epoch(t interface{ Unix() int64 }) float64 { return float64(t.Unix()) }

func (h *Handler) cacheList(w http.ResponseWriter, r *http.Request) {
	if !h.cacheAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cached, err := h.cache.Cached(r.Context())
		if err != nil {
			h.failure(w, err)
			return
		}
		queued, err := h.cache.Queued()
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(cached))
		for _, c := range cached {
			out = append(out, map[string]any{"image_id": c.ImageID, "hits": c.Hits, "size": c.Size,
				"last_accessed": epoch(c.LastAccessed), "last_modified": epoch(c.LastModified)})
		}
		h.json(w, 200, map[string]any{"cached_images": out, "queued_images": queued})
	case http.MethodDelete:
		target := r.Header.Get("x-image-cache-clear-target")
		var err error
		switch target {
		case "cache":
			err = h.cache.Delete(r.Context(), "")
		case "queue":
			err = h.cache.Unqueue("")
		case "":
			err = errors.Join(h.cache.Delete(r.Context(), ""), h.cache.Unqueue(""))
		default:
			h.status(w, 400, "If provided 'x-image-cache-clear-target' must be 'cache', 'queue' or empty string.")
			return
		}
		if err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) cacheImage(w http.ResponseWriter, r *http.Request) {
	if !h.cacheAllowed(w, r) {
		return
	}
	image, err := h.service.Get(r.Context(), who(r), r.PathValue("image"))
	if err != nil {
		h.failure(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if image.Status != "active" {
			h.status(w, 400, "Only images with status active can be targeted for queueing")
			return
		}
		if err = h.cache.Queue(image.ID); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(202)
	case http.MethodDelete:
		if err = errors.Join(h.cache.Delete(r.Context(), image.ID), h.cache.Unqueue(image.ID)); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}
