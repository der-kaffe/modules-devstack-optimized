// Package store provides image-byte storage, independent of HTTP, SQL and image
// states. Callers must authorize every key and implement fencing before Publish
// or Delete. A published file alone is not proof of an active image in MySQL.
package store

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/google/uuid"
)

// BufferSize is the maximum transfer/hash buffer allocated by an operation.
const BufferSize = 1024 * 1024 // 1 MiB (was 256 KiB): ~7% faster 64 MiB uploads; bounded per transfer

var (
	ErrInvalidKey   = errors.New("invalid store UUID key")
	ErrUnsafe       = errors.New("unsafe store root or entry")
	ErrExists       = errors.New("store key already exists")
	ErrNotFound     = errors.New("published content not found")
	ErrIncomplete   = errors.New("staged content missing or incomplete")
	ErrIntegrity    = errors.New("content does not match expected integrity")
	ErrTooLarge     = errors.New("image size limit exceeded")
	ErrSizeMismatch = errors.New("declared image size mismatch")
	ErrStoreFull    = errors.New("store capacity exhausted")
	ErrBusy         = errors.New("store root already in use")
	ErrClosed       = errors.New("store closed")
	ErrInvalidSize  = errors.New("invalid size limit or declaration")
	// ErrInUse: a backend refuses to delete content that other objects depend
	// on (RBD copy-on-write clones made by Nova). Glance Python answers 409.
	ErrInUse = errors.New("store content in use")
)

// Backend is a Store with exclusive ownership that must be closed once idle.
type Backend interface {
	Store
	Close() error
}

type Key struct {
	ImageID   string
	AttemptID string
	// Backend names one of enabled_backends ("": the default store; for
	// Delete, every store).
	Backend string
}

func canonicalID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || len(value) != 36 || id.String() != strings.ToLower(value) {
		return "", ErrInvalidKey
	}
	return id.String(), nil
}

func (k Key) name() (string, error) {
	image, err := canonicalID(k.ImageID)
	if err != nil {
		return "", err
	}
	attempt, err := canonicalID(k.AttemptID)
	if err != nil {
		return "", err
	}
	return image + "-" + attempt, nil
}

// Info describes physical bytes only. No format is interpreted here and no
// virtual_size is inferred. MD5 is the compatibility checksum, not authorization.
type Info struct {
	Size     int64
	Checksum string
	SHA512   string
}

type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type Store interface {
	// Stage leaves identifiable files on failure for explicit Delete/recovery.
	// The caller owns src and must unblock a blocking Read on cancellation (for
	// example HTTP deadlines/Body.Close). No unbounded helper goroutine is used.
	Stage(ctx context.Context, key Key, src io.Reader, limit int64, expected *int64) (Info, error)
	// Publish verifies the receipt returned by Stage before creating the final
	// name. An error after linking may leave a complete published object; callers
	// must resolve the outcome rather than assume rollback. Repetition: ErrExists.
	Publish(ctx context.Context, key Key, expected Info) error
	// Open returns a caller-owned descriptor at offset zero and freshly measured
	// hashes. Compare them to authoritative metadata to detect stored corruption.
	Open(ctx context.Context, key Key) (ReadSeekCloser, Info, error)
	// OpenStream returns a caller-owned descriptor at offset zero and the
	// physical size WITHOUT hashing, as official Glance (glance_store) serves
	// downloads. Callers compare the size with authoritative metadata. Open
	// keeps the verifying contract for recovery and deletion.
	OpenStream(ctx context.Context, key Key) (ReadSeekCloser, int64, error)
	// Delete touches only this key's part, ready and blob names, never scans.
	Delete(ctx context.Context, key Key) error
}
