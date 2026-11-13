package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/google/uuid"
)

type identityKey struct{}

type Handler struct {
	service      *service.Service
	auth         auth.Authenticator
	publicURL    *url.URL
	maxBodyBytes int64
	logger       *slog.Logger
	challenge    string
	binary       BinaryTransfers
}

func New(service *service.Service, authenticator auth.Authenticator, publicURL, keystoneURL string, maxBodyBytes int64, logger *slog.Logger, options ...Option) (http.Handler, error) {
	if err := config.ValidateURL(publicURL); err != nil {
		return nil, err
	}
	base, _ := url.Parse(publicURL) // ValidateURL has checked the same value.
	if err := config.ValidateURL(keystoneURL); err != nil {
		return nil, err
	}
	identityURL, _ := url.Parse(strings.TrimRight(keystoneURL, "/"))
	if !strings.HasSuffix(identityURL.Path, "/v3") {
		return nil, errors.New("Keystone URL must end in /v3")
	}
	identityURL.Path = strings.TrimSuffix(identityURL.Path, "/v3")
	identityURL.RawPath = ""
	h := &Handler{service: service, auth: authenticator, publicURL: base, maxBodyBytes: maxBodyBytes, logger: logger, challenge: "Keystone uri=" + strconv.Quote(identityURL.String())}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("invalid HTTP option")
		}
		if err := option(h); err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/images", h.create)
	mux.HandleFunc("/v2/images/{id}", h.show)
	mux.HandleFunc("/v2/schemas/{name}", h.schema)
	if h.binary != nil {
		mux.HandleFunc("/v2/images/{id}/file", h.file)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.failure(w, model.Fail(model.NotFound, "Route not implemented."))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Own the input even when middleware rejects authentication. Closing on
		// cancellation unblocks a cooperative/net/http request Body; no goroutine
		// is abandoned reading the payload.
		if h.binary != nil && logRoute(r.URL.Path) == "/v2/images/{id}/file" && r.Body != nil {
			body := &onceBody{ReadCloser: r.Body}
			r.Body = body
			done := make(chan struct{})
			stop := context.AfterFunc(r.Context(), func() { _ = body.Close(); close(done) })
			defer func() {
				if !stop() {
					<-done
				}
				_ = body.Close()
			}()
		}
		id, err := uuid.NewRandom()
		if err != nil {
			h.failure(w, err)
			return
		}
		requestID := "req-" + id.String()
		w.Header().Set("X-Openstack-Request-Id", requestID)
		observed := &responseLog{ResponseWriter: w}
		w = observed
		start := time.Now()
		defer func() {
			level := slog.LevelInfo
			if observed.status >= 500 {
				level = slog.LevelError
			} else if observed.status >= 400 {
				level = slog.LevelWarn
			}
			method := r.Method
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
			default:
				method = "OTHER"
			}
			h.logger.Log(r.Context(), level, "request completed", "request_id", requestID, "method", method, "route", logRoute(r.URL.Path), "status", observed.status, "duration_ms", time.Since(start).Milliseconds(), "response_bytes", observed.bytes, "error_kind", observed.kind)
		}()
		// Version discovery is public in the original (no authtoken in its
		// pipeline); keystoneauth in Nova requests it before any image call.
		if status, ok := discovery(r); ok {
			h.versions(w, status)
			return
		}
		values := r.Header.Values("X-Auth-Token")
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			h.failure(w, auth.ErrInvalidToken)
			return
		}
		var identity auth.Identity
		serviceTokens := r.Header.Values("X-Service-Token")
		if len(serviceTokens) != 0 {
			if len(serviceTokens) != 1 || strings.TrimSpace(serviceTokens[0]) == "" {
				h.failure(w, auth.ErrInvalidToken)
				return
			}
			composite, ok := h.auth.(auth.CompositeAuthenticator)
			if !ok {
				h.failure(w, auth.ErrForbidden)
				return
			}
			identity, err = composite.AuthenticateComposite(r.Context(), values[0], serviceTokens[0])
		} else {
			identity, err = h.auth.Authenticate(r.Context(), values[0])
		}
		if err != nil {
			h.failure(w, err)
			return
		}
		if err := identity.Check(time.Now()); err != nil {
			h.failure(w, err)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, identity)))
	}), nil
}

// Log route templates rather than arbitrary paths, identifiers or queries.
func logRoute(path string) string {
	if path == "/v2/images" || path == "/" || path == "/versions" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, "/v2/schemas/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "/v2/schemas/{name}"
	}
	if rest, ok := strings.CutPrefix(path, "/v2/images/"); ok && strings.HasSuffix(rest, "/file") {
		id := strings.TrimSuffix(rest, "/file")
		if id != "" && !strings.Contains(id, "/") {
			return "/v2/images/{id}/file"
		}
	}
	if rest, ok := strings.CutPrefix(path, "/v2/images/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "/v2/images/{id}"
	}
	return "unmatched"
}

type responseLog struct {
	http.ResponseWriter
	status int
	kind   string
	bytes  int64
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseLog) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if w.kind == "" {
		w.kind = map[int]string{400: "invalid_request", 401: "invalid_token", 403: "forbidden", 404: "not_found", 405: "method", 409: "conflict", 413: "body_limit", 415: "media_type", 500: "internal", 503: "dependency"}[status]
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseLog) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	if err != nil {
		w.kind = "response_write"
	}
	return n, err
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.list(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		h.status(w, 405, "Method not implemented.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.status(w, 415, "Expected Content-Type application/json.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.status(w, 413, "Request body limit exceeded.")
		} else {
			h.status(w, 400, "Expected a JSON object.")
		}
		return
	}
	if !utf8.Valid(data) {
		h.status(w, 400, "Request body must be valid UTF-8.")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		h.status(w, 400, "Expected a JSON object.")
		return
	}
	if raw == nil {
		h.status(w, 400, "Expected a JSON object.")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.status(w, 413, "Request body limit exceeded.")
		} else {
			h.status(w, 400, "Expected exactly one JSON object.")
		}
		return
	}
	input, err := decodeCreate(raw)
	if err != nil {
		h.failure(w, err)
		return
	}
	image, err := h.service.Create(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), input)
	if err != nil {
		h.failure(w, err)
		return
	}
	w.Header().Set("Location", h.publicURL.JoinPath("v2", "images", image.ID).String())
	h.json(w, 201, representation(image))
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		h.patch(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		h.delete(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		h.status(w, 405, "Method not implemented.")
		return
	}
	image, err := h.service.Get(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), r.PathValue("id"))
	if err != nil {
		h.failure(w, err)
		return
	}
	h.json(w, 200, representation(image))
}

func (h *Handler) json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value) // responseLog records transport errors.
}

func (h *Handler) status(w http.ResponseWriter, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", h.challenge)
	}
	h.json(w, status, map[string]string{"message": message + "<br /><br />\n\n\n", "code": fmt.Sprintf("%d %s", status, http.StatusText(status)), "title": http.StatusText(status)})
}

func (h *Handler) failure(w http.ResponseWriter, err error) {
	var domain *model.Error
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		h.status(w, 401, "Authentication required.")
	case errors.Is(err, auth.ErrForbidden):
		h.status(w, 403, "Project scope is required.")
	case errors.Is(err, auth.ErrUnavailable):
		if observed, ok := w.(*responseLog); ok {
			observed.kind = "identity_unavailable"
		}
		h.status(w, 503, "Identity validation unavailable.")
	case errors.As(err, &domain):
		status := map[model.Kind]int{model.Invalid: 400, model.Forbidden: 403, model.NotFound: 404, model.Conflict: 409, model.LimitExceeded: 413}[domain.Kind]
		if status == 0 {
			status = 500
		}
		h.status(w, status, domain.Message)
	default:
		// Database/transport errors may include credentials or SQL values.
		// They must never be serialized or passed to the logger.
		if observed, ok := w.(*responseLog); ok {
			observed.kind = "metadata_unavailable"
		}
		h.status(w, 503, "Metadata service unavailable.")
	}
}

func representation(image model.Image) map[string]any {
	out := map[string]any{}
	for key, value := range image.Properties {
		if !model.Reserved(key) {
			out[key] = value
		}
	}
	for key, value := range map[string]any{
		"id": image.ID, "name": image.Name, "owner": image.Owner, "status": image.Status, "visibility": image.Visibility,
		"disk_format": image.DiskFormat, "container_format": image.ContainerFormat, "min_disk": image.MinDisk, "min_ram": image.MinRAM,
		"protected": image.Protected, "os_hidden": image.Hidden, "size": image.Size, "virtual_size": image.VirtualSize,
		"checksum": image.Checksum, "os_hash_algo": image.HashAlgorithm, "os_hash_value": image.HashValue,
		"created_at": image.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), "updated_at": image.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"self": "/v2/images/" + image.ID, "file": "/v2/images/" + image.ID + "/file", "schema": "/v2/schemas/image",
	} {
		out[key] = value
	}
	tags := image.Tags
	if tags == nil {
		tags = []string{}
	}
	out["tags"] = tags
	return out
}
