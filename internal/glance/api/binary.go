package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// BinaryTransfers is implemented by service.UploadCoordinator, including its
// startup recovery gate. Implementations must authorize/fence before any I/O,
// limit the real stream, and return downloads only after full integrity checks.
// HTTP never receives a storage key, attempt ID, cleanup target or root.
type BinaryTransfers interface {
	MaxUploadBytes() int64
	Upload(context.Context, auth.Identity, string, io.Reader, *int64) (model.DataContent, error)
	OpenDownload(context.Context, auth.Identity, string) (*service.Download, error)
}

var _ BinaryTransfers = (*service.UploadCoordinator)(nil)

type Option func(*Handler) error

// WithBinaryTransfers opts in to the two binary routes. Metadata-only cmd/glance
// stays unchanged until explicit store/SQL scope binding and exclusive startup
// recovery are wired. This option neither runs recovery nor makes a service ready.
func WithBinaryTransfers(t BinaryTransfers) Option {
	return func(h *Handler) error {
		if t == nil || t.MaxUploadBytes() <= 0 || t.MaxUploadBytes() > service.MaxServerUploadBytes {
			return errors.New("invalid binary transfer dependencies or limit")
		}
		h.binary = t
		return nil
	}
}

type onceBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *onceBody) Close() error { b.once.Do(func() { b.err = b.ReadCloser.Close() }); return b.err }

func (h *Handler) file(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		h.putFile(w, r)
	case http.MethodGet:
		h.getFile(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		h.status(w, 405, "Method not implemented.")
	}
}

func decimalSize(s string) (int64, error) {
	if s == "" {
		return 0, store.ErrInvalidSize
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, store.ErrInvalidSize
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, store.ErrInvalidSize
	}
	return n, nil
}

// net/http owns HTTP framing. Only inspect its parsed ContentLength; never
// reinterpret raw Content-Length fields. The optional Glance declaration is
// stricter and must agree with framing when both are known.
func declaredSize(r *http.Request) (*int64, error) {
	if r.ContentLength < -1 {
		return nil, store.ErrInvalidSize
	}
	var expected *int64
	if r.ContentLength >= 0 {
		n := r.ContentLength
		expected = &n
	}
	values := r.Header.Values("X-Openstack-Image-Size")
	if len(values) > 0 {
		if len(values) != 1 {
			return nil, store.ErrInvalidSize
		}
		n, err := decimalSize(values[0])
		if err != nil || expected != nil && *expected != n {
			return nil, store.ErrSizeMismatch
		}
		expected = &n
	}
	return expected, nil
}

// uploadReader bounds every read and probes one extra byte rather than treating
// a truncated prefix as EOF/success. No allocation depends on requested size.
type uploadReader struct {
	ctx      context.Context
	src      io.Reader
	max      int64
	expected *int64
	read     int64
}

func (b *uploadReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := b.max - b.read
	if b.expected != nil && *b.expected-b.read < remaining {
		remaining = *b.expected - b.read
	}
	if int64(len(p)) > remaining+1 {
		p = p[:int(remaining+1)]
	}
	n, err := b.src.Read(p)
	if n < 0 || n > len(p) {
		return 0, service.ErrUploadInterrupted
	}
	b.read += int64(n)
	if b.read > b.max {
		return 0, store.ErrTooLarge
	}
	if b.expected != nil && b.read > *b.expected {
		return 0, store.ErrSizeMismatch
	}
	if b.ctx.Err() != nil {
		return 0, b.ctx.Err()
	}
	if err == io.EOF && b.expected != nil && b.read != *b.expected {
		return n, store.ErrSizeMismatch
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return n, store.ErrSizeMismatch
	}
	return n, err
}

func (h *Handler) putFile(w http.ResponseWriter, r *http.Request) {
	values := r.Header.Values("Content-Type")
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(values) != 1 || err != nil || media != "application/octet-stream" {
		h.status(w, 415, "Expected Content-Type application/octet-stream.")
		return
	}
	expected, err := declaredSize(r)
	if err != nil {
		h.binaryFailure(w, err)
		return
	}
	if expected != nil && *expected > h.binary.MaxUploadBytes() {
		h.binaryFailure(w, store.ErrTooLarge)
		return
	}
	var src io.Reader = http.NoBody
	if r.Body != nil {
		src = r.Body
	}
	bounded := &uploadReader{ctx: r.Context(), src: src, max: h.binary.MaxUploadBytes(), expected: expected}
	who := r.Context().Value(identityKey{}).(auth.Identity)
	if h.multi != nil {
		// X-Image-Meta-Store chooses the backend (default_backend if absent).
		_, err = h.multi.UploadTo(r.Context(), who, r.PathValue("id"), bounded, expected, r.Header.Get("X-Image-Meta-Store"))
	} else {
		_, err = h.binary.Upload(r.Context(), who, r.PathValue("id"), bounded, expected)
	}
	if err != nil {
		h.binaryFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) binaryFailure(w http.ResponseWriter, err error) {
	// Resolve uncertainty/cleanup failures before the triggering 400/413: bytes
	// may remain and the operation requires reconciliation, not a blind retry.
	switch {
	case errors.Is(err, service.ErrUploadReconciliation), errors.Is(err, repository.ErrCommitUncertain):
		binaryKind(w, "reconciliation")
		h.status(w, 503, "Image transfer requires reconciliation.")
	case errors.Is(err, service.ErrUploadGone):
		h.status(w, 410, "Image was deleted during upload.")
	case errors.Is(err, service.ErrUploadNotReady), errors.Is(err, service.ErrUploadBusy):
		binaryKind(w, "transfer_busy")
		h.status(w, 503, "Image transfers are temporarily unavailable.")
	case errors.Is(err, store.ErrTooLarge), errors.Is(err, store.ErrStoreFull):
		h.status(w, 413, "Image size or storage limit exceeded.")
	case errors.Is(err, store.ErrSizeMismatch), errors.Is(err, store.ErrInvalidSize), errors.Is(err, service.ErrUploadInterrupted):
		h.status(w, 400, "Invalid or interrupted image payload.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		binaryKind(w, "transfer_cancelled")
		h.status(w, 503, "Image transfer interrupted.")
	default:
		var domain *model.Error
		if errors.As(err, &domain) || errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrUnavailable) {
			h.failure(w, err)
			return
		}
		binaryKind(w, "transfer_dependency")
		h.status(w, 503, "Image content is temporarily unavailable.")
	}
}

func binaryKind(w http.ResponseWriter, kind string) {
	if observed, ok := w.(*responseLog); ok {
		observed.kind = kind
	}
}

func (h *Handler) getFile(w http.ResponseWriter, r *http.Request) {
	d, err := h.binary.OpenDownload(r.Context(), r.Context().Value(identityKey{}).(auth.Identity), r.PathValue("id"))
	if err != nil {
		if d != nil && d.Body != nil {
			_ = d.Body.Close()
		}
		if errors.Is(err, service.ErrNoImageContent) {
			w.WriteHeader(204)
		} else {
			h.binaryFailure(w, err)
		}
		return
	}
	if d == nil || d.Body == nil || d.Size < 0 {
		if d != nil && d.Body != nil {
			_ = d.Body.Close()
		}
		h.binaryFailure(w, service.ErrBlobCorrupt)
		return
	}
	defer func() {
		if d.Body.Close() != nil {
			binaryKind(w, "download_close")
		}
	}()
	start, count, status := int64(0), d.Size, 200
	if values := r.Header.Values("Range"); len(values) != 0 {
		if len(values) > 1 {
			h.status(w, 400, "Multiple ranges are not supported.")
			return
		}
		start, count, status = singleRange(values[0], d.Size)
		if status != 206 {
			if status == 416 {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", d.Size))
			}
			h.status(w, status, "Invalid or unsupported image range.")
			return
		}
	}
	if offset, err := d.Body.Seek(start, io.SeekStart); err != nil || offset != start {
		h.binaryFailure(w, service.ErrDownloadUnavailable)
		return
	}
	if err := r.Context().Err(); err != nil {
		h.binaryFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(count, 10))
	w.Header().Set("Content-MD5", d.Checksum) // whole-image hash, also for 206 in pinned Python
	if status == 206 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+count-1, d.Size))
	}
	w.WriteHeader(status)
	var n int64
	var copyErr error
	if file, ok := d.Body.(interface{ File() *os.File }); ok && file.File() != nil {
		// Zero-copy (sendfile) like a plain file server. timedHandler's
		// connection deadlines bound and cancel it, instead of contextReader.
		n, copyErr = io.Copy(w, io.LimitReader(file.File(), count))
	} else {
		reader := &contextReader{ctx: r.Context(), src: io.LimitReader(d.Body, count)}
		n, copyErr = io.CopyBuffer(w, reader, make([]byte, store.BufferSize))
	}
	if copyErr == nil && d.Sent != nil {
		d.Sent(n, count)
	}
	if copyErr != nil || n != count {
		// Headers are committed: never append JSON or replace status. Abort the
		// real HTTP stream (incl. HTTP/2) so a truncated payload cannot look whole.
		binaryKind(w, "binary_stream")
		panic(http.ErrAbortHandler)
	}
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (b *contextReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.src.Read(p)
	if cancelled := b.ctx.Err(); cancelled != nil {
		return 0, cancelled
	}
	return n, err
}

func singleRange(value string, size int64) (start, count int64, status int) {
	if strings.Contains(value, ",") {
		return 0, 0, 400
	}
	spec, ok := strings.CutPrefix(value, "bytes=")
	if !ok || size <= 0 {
		return 0, 0, 416
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, 416
	}
	if a == "" {
		suffix, err := decimalSize(b)
		if err != nil || suffix == 0 {
			return 0, 0, 416
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, 206
	}
	first, err := decimalSize(a)
	if err != nil || first >= size {
		return 0, 0, 416
	}
	last := size - 1
	if b != "" {
		n, err := decimalSize(b)
		if err != nil || n < first {
			return 0, 0, 416
		}
		if n < last {
			last = n
		}
	}
	return first, last - first + 1, 206
}
