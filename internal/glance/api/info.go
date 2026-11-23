package api

import "net/http"

// /v2/info/*, as the original's discovery controller.

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	usage, err := h.service.Usage(r.Context(), who(r))
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, 200, map[string]any{"usage": usage})
}

// stores: one filesystem store (no enabled_backends) answers as the
// original without multiple backends.
func (h *Handler) stores(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.status(w, 405, "Method not implemented.")
		return
	}
	if len(h.storeList) > 0 {
		h.storesView(w, r)
		return
	}
	h.status(w, 404, "Multi backend is not supported at this site.")
}
