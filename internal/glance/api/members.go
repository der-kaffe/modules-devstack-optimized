package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// Image members, tags by path and image actions, as the original's
// image_members, image_tags and image_actions controllers.

func who(r *http.Request) auth.Identity { return r.Context().Value(identityKey{}).(auth.Identity) }

func memberRepresentation(m model.Member) map[string]any {
	return map[string]any{"image_id": m.ImageID, "member_id": m.MemberID, "status": m.Status,
		"created_at": m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), "updated_at": m.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"schema": "/v2/schemas/member"}
}

// readObject reads one JSON object body (application/json) into dst.
func (h *Handler) readObject(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.status(w, 415, "Expected Content-Type application/json.")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.status(w, 413, "Request body limit exceeded.")
		} else {
			h.status(w, 400, "Expected a JSON object.")
		}
		return false
	}
	var object map[string]json.RawMessage
	if !utf8.Valid(data) || json.Unmarshal(data, &object) != nil || object == nil {
		h.status(w, 400, "Expected a JSON object.")
		return false
	}
	if err = json.Unmarshal(data, dst); err != nil {
		h.status(w, 400, "Expected a JSON object.")
		return false
	}
	return true
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		members, err := h.service.ListMembers(r.Context(), who(r), id)
		if err != nil {
			h.failure(w, err)
			return
		}
		out := make([]map[string]any, 0, len(members))
		for _, m := range members {
			out = append(out, memberRepresentation(m))
		}
		h.json(w, 200, map[string]any{"members": out, "schema": "/v2/schemas/members"})
	case http.MethodPost:
		var body struct {
			Member *string `json:"member"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		if body.Member == nil {
			h.status(w, 400, "Member to be added not specified")
			return
		}
		m, err := h.service.CreateMember(r.Context(), who(r), id, *body.Member)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, memberRepresentation(m))
	default:
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) imageMember(w http.ResponseWriter, r *http.Request) {
	id, member := r.PathValue("id"), r.PathValue("member")
	switch r.Method {
	case http.MethodGet:
		m, err := h.service.GetMember(r.Context(), who(r), id, member)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, memberRepresentation(m))
	case http.MethodPut:
		var body struct {
			Status *string `json:"status"`
		}
		if !h.readObject(w, r, &body) {
			return
		}
		if body.Status == nil {
			h.status(w, 400, "Status not specified")
			return
		}
		m, err := h.service.UpdateMember(r.Context(), who(r), id, member, *body.Status)
		if err != nil {
			h.failure(w, err)
			return
		}
		h.json(w, 200, memberRepresentation(m))
	case http.MethodDelete:
		if err := h.service.DeleteMember(r.Context(), who(r), id, member); err != nil {
			h.failure(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
	}
}

func (h *Handler) tag(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.Method {
	case http.MethodPut:
		err = h.service.AddTag(r.Context(), who(r), r.PathValue("id"), r.PathValue("tag"))
	case http.MethodDelete:
		err = h.service.RemoveTag(r.Context(), who(r), r.PathValue("id"), r.PathValue("tag"))
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		h.status(w, 405, "Method not implemented.")
		return
	}
	if err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Handler) action(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.status(w, 405, "Method not implemented.")
		return
	}
	var err error
	switch r.PathValue("action") {
	case "deactivate":
		err = h.service.Deactivate(r.Context(), who(r), r.PathValue("id"))
	case "reactivate":
		err = h.service.Reactivate(r.Context(), who(r), r.PathValue("id"))
	default:
		h.status(w, 404, "The resource could not be found.")
		return
	}
	if err != nil {
		h.failure(w, err)
		return
	}
	w.WriteHeader(204)
}
