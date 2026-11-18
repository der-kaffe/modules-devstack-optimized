package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

func TestDownloadVerifiedBytesAndLifetime(t *testing.T) {
	u, d, fs, log := uploadFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	var opened store.Key
	fs.before = func(name string, key store.Key) {
		if name == "OpenStream" {
			opened = key
		}
	}
	out, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Close() != ErrUploadBusy {
		t.Fatal("released store with open download")
	}
	got, err := io.ReadAll(out.Body) // small fixture only, never production image buffering
	if err != nil || !bytes.Equal(got, uploadPayload) || out.Size != int64(len(got)) || out.Checksum != uploadInfo(got).Checksum {
		t.Fatal(out, err)
	}
	if opened != (store.Key{ImageID: uploadImageID, AttemptID: oldUploadAttempt}) {
		t.Fatal("key not derived from SQL")
	}
	if err := out.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// One catalog and one state read, then an unhashed open (as glance_store).
	want := []string{"catalog.Get", "sql.State", "fs.OpenStream", "fs.Close"}
	if !reflect.DeepEqual(log.snapshot(), want) {
		t.Fatal(log.snapshot())
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID); !errors.Is(err, ErrUploadNotReady) {
		t.Fatal(err)
	}
}

func TestDownloadDenialsDoNotOpenOrMutate(t *testing.T) {
	for _, tc := range []string{"member-only", "expired", "foreign-private", "foreign-shared", "missing", "deleted", "bad-id", "not-ready", "busy", "sql-catalog", "sql-state", "cancelled", "active-no-operation", "wrong-attempt-id", "wrong-image", "wrong-phase", "receipt-mismatch", "invalid-size"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			who, id, ctx := identity("reader"), uploadImageID, context.Background()
			switch tc {
			case "member-only":
				who = identity("member")
			case "expired":
				who.ExpiresAt = who.ExpiresAt.AddDate(-1, 0, 0)
			case "foreign-private", "foreign-shared":
				who.ProjectID = "other"
				d.image.Visibility = strings.TrimPrefix(tc, "foreign-")
			case "missing":
				u.images = uploadCatalogDouble{sql: d, err: repository.ErrNotFound}
			case "deleted":
				d.image.Status = "deleted"
			case "bad-id":
				id = "invalid"
			case "not-ready":
				u.ready = false
			case "busy":
				u.readers = u.limits.MaxConcurrent
			case "sql-catalog":
				u.images = uploadCatalogDouble{sql: d, err: errors.New("secret catalog SQL")}
			case "sql-state":
				d.faults["State"] = uploadSQLFault{err: errors.New("secret SQL")}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "active-no-operation":
				d.op = nil
			case "wrong-attempt-id":
				d.op.AttemptID = "invented"
			case "wrong-image":
				d.op.ImageID = oldUploadAttempt
			case "wrong-phase":
				d.op.Phase = model.DataPublished
			case "receipt-mismatch":
				d.image.Size = uploadPtr(int64(999))
			case "invalid-size":
				d.op.Size = uploadPtr(int64(-1))
			}
			if _, err := u.OpenDownload(ctx, who, id); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
			for _, call := range log.snapshot() {
				if strings.HasPrefix(call, "fs.") || strings.HasPrefix(call, "sql.") && call != "sql.State" {
					t.Fatal("denial had I/O side effect", log.snapshot())
				}
			}
		})
	}
}

func TestDownloadVisibilityAndAbsentContent(t *testing.T) {
	for _, tc := range []string{"public", "community", "admin", "queued", "saving-reserved", "saving-staged", "saving-published", "saving-cleanup", "cleaned", "empty-active"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			who := identity("reader")
			absent := true
			switch tc {
			case "public", "community", "admin", "empty-active":
				absent = false
				uploadSeed(d, model.DataCommitted)
				fs.blob = append([]byte{}, uploadPayload...)
				if tc == "admin" {
					who = identity("admin")
					who.ProjectID = "other"
				} else if tc != "empty-active" {
					d.image.Visibility = tc
					who.ProjectID = "other"
				}
				if tc == "empty-active" {
					info := uploadInfo(nil)
					fs.blob = []byte{}
					d.op.Size, d.op.VirtualSize, d.op.Checksum, d.op.SHA512 = uploadPtr(int64(0)), uploadPtr(int64(0)), &info.Checksum, &info.SHA512
					d.image.Size, d.image.VirtualSize, d.image.Checksum, d.image.HashValue = d.op.Size, d.op.VirtualSize, d.op.Checksum, d.op.SHA512
				}
			case "queued":
			case "cleaned":
				uploadSeed(d, model.DataCleaned)
			default:
				uploadSeed(d, model.DataPhase(strings.TrimPrefix(tc, "saving-")))
			}
			out, err := u.OpenDownload(context.Background(), who, uploadImageID)
			if absent {
				if !errors.Is(err, ErrNoImageContent) || out != nil {
					t.Fatal(out, err)
				}
				for _, call := range log.snapshot() {
					if strings.HasPrefix(call, "fs.") {
						t.Fatal(log.snapshot())
					}
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				_ = out.Body.Close()
			}
		})
	}
}

func TestDownloadFailuresAfterOpenCloseAndRetainEvidence(t *testing.T) {
	// Since GET streams without hashing (like glance_store), it no longer
	// re-reads SQL after opening: the *-after-open fencing cases were removed.
	for _, tc := range []string{"missing", "corrupt-receipt", "corrupt-store", "filesystem", "cancel-after-open"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, model.DataCommitted)
			fs.blob = append([]byte{}, uploadPayload...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var want error
			switch tc {
			case "missing":
				fs.blob = nil
				want = ErrBlobMissing
			case "corrupt-receipt":
				fs.blob = []byte("damaged")
				want = ErrBlobCorrupt
			case "corrupt-store":
				fs.faults["OpenStream"] = uploadStoreFault{err: store.ErrIntegrity}
				want = ErrBlobCorrupt
			case "filesystem":
				fs.faults["OpenStream"] = uploadStoreFault{err: errors.New("secret filesystem path")}
				want = ErrDownloadUnavailable
			default:
				fs.before = func(name string, _ store.Key) {
					if name == "OpenStream" && tc == "cancel-after-open" {
						cancel()
					}
				}
			}
			out, err := u.OpenDownload(ctx, identity("reader"), uploadImageID)
			if err == nil || out != nil || want != nil && !errors.Is(err, want) || strings.Contains(err.Error(), "secret") {
				t.Fatal(out, err)
			}
			for _, call := range log.snapshot() {
				if call == "fs.Delete" || strings.HasPrefix(call, "sql.") && call != "sql.State" {
					t.Fatal("GET changed data", log.snapshot())
				}
			}
			if tc == "corrupt-receipt" || strings.HasSuffix(tc, "after-open") && tc != "cancel-after-open" {
				if !strings.Contains(strings.Join(log.snapshot(), ","), "fs.Close") {
					t.Fatal("descriptor leaked", log.snapshot())
				}
			}
			if u.readers != 0 {
				t.Fatal("capacity leaked")
			}
		})
	}
}

func TestDownloadSharedCapacityAndRecoveryGate(t *testing.T) {
	u, d, fs, _ := uploadFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	u.limits.MaxConcurrent = 1
	out, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.OpenDownload(context.Background(), identity("reader"), uploadImageID); !errors.Is(err, ErrUploadBusy) {
		t.Fatal(err)
	}
	if _, err = u.claim(oldUploadAttempt); !errors.Is(err, ErrUploadBusy) {
		t.Fatal("upload bypassed shared capacity", err)
	}
	if _, err = u.RecoverUploads(context.Background()); err == nil {
		t.Fatal("recovery ran with reader")
	}
	_ = out.Body.Close()
	if u.readers != 0 {
		t.Fatal("capacity not released")
	}
}

func TestUploadDistinguishesSourceAndFilesystemFailures(t *testing.T) {
	for _, tc := range []string{"source", "stage-filesystem", "publish-filesystem"} {
		t.Run(tc, func(t *testing.T) {
			u, _, fs, _ := uploadFixture(t)
			var src io.Reader = bytes.NewReader(uploadPayload)
			want := ErrUploadUnavailable
			switch tc {
			case "source":
				src = downloadErrorSource{}
				want = ErrUploadInterrupted
			case "stage-filesystem":
				fs.faults["Stage"] = uploadStoreFault{err: errors.New("secret filesystem write")}
			case "publish-filesystem":
				fs.faults["Publish"] = uploadStoreFault{err: errors.New("secret filesystem fsync"), applied: true}
			}
			_, err := u.Upload(context.Background(), identity("member"), uploadImageID, src, nil)
			if !errors.Is(err, want) || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
}

type downloadErrorSource struct{}

func (downloadErrorSource) Read([]byte) (int, error) { return 0, errors.New("secret source error") }

type downloadStoreOverride struct {
	store.Store
	open func(context.Context, store.Key) (store.ReadSeekCloser, store.Info, error)
}

// The download path uses OpenStream; the override reports Info.Size as size.
func (s downloadStoreOverride) OpenStream(ctx context.Context, k store.Key) (store.ReadSeekCloser, int64, error) {
	f, info, err := s.open(ctx, k)
	return f, info.Size, err
}

func TestDownloadClosesAmbiguousOpenAndPreservesCancellation(t *testing.T) {
	for _, tc := range []string{"nil", "descriptor-with-error", "wrapped-cancel", "wrong-info"} {
		t.Run(tc, func(t *testing.T) {
			u, d, fs, log := uploadFixture(t)
			uploadSeed(d, model.DataCommitted)
			u.bytes = downloadStoreOverride{Store: fs, open: func(context.Context, store.Key) (store.ReadSeekCloser, store.Info, error) {
				if tc == "nil" {
					return nil, store.Info{}, nil
				}
				f := uploadReadCloser{bytes.NewReader(uploadPayload), log}
				switch tc {
				case "descriptor-with-error":
					return f, store.Info{}, errors.New("secret open error")
				case "wrapped-cancel":
					return f, store.Info{}, errors.Join(store.ErrIntegrity, context.Canceled)
				default:
					return f, store.Info{Size: 1}, nil
				}
			}}
			_, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
			if tc == "wrapped-cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel was classified as corruption", err)
			}
			closed := 0
			for _, call := range log.snapshot() {
				if call == "fs.Close" {
					closed++
				}
			}
			if tc != "nil" && closed != 1 || u.readers != 0 {
				t.Fatal("leaked resource", closed, u.readers)
			}
		})
	}
}

func TestDownloadConcurrentCloseReleasesOwnershipOnce(t *testing.T) {
	u, d, fs, log := uploadFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	first, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = first.Body.Close(); _ = second.Body.Close() }()
	}
	wg.Wait()
	if u.readers != 0 {
		t.Fatal("capacity leaked")
	}
	closes := 0
	for _, call := range log.snapshot() {
		if call == "fs.Close" {
			closes++
		}
	}
	if closes != 2 || u.Close() != nil {
		t.Fatal("ownership released incorrectly", closes)
	}
}

// M2: a repository offering DownloadView serves GET from one read transaction
// (no catalog.Get + separate state read) with the same visibility policy.
type downloadViewDouble struct {
	*uploadSQLDouble
}

func (v downloadViewDouble) DownloadView(ctx context.Context, id string) (model.Image, model.DataState, error) {
	v.log.add("sql.View")
	s, err := v.DataStateSnapshot(ctx, id)
	if err != nil {
		return model.Image{}, model.DataState{}, err
	}
	return v.image, s, nil
}

func TestDownloadUsesSingleViewWhenAvailable(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		u, d, fs, log := uploadFixture(t)
		uploadSeed(d, model.DataCommitted)
		fs.blob = append([]byte{}, uploadPayload...)
		u.transfers = downloadViewDouble{d}
		who := identity("reader")
		if foreign {
			who.ProjectID = "other"
			d.image.Visibility = "private"
		}
		out, err := u.OpenDownload(context.Background(), who, uploadImageID)
		calls := strings.Join(log.snapshot(), ",")
		if foreign {
			if err == nil || out != nil || strings.Contains(calls, "fs.OpenStream") {
				t.Fatal("foreign private image served", err, calls)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !bytes.Equal(got, uploadPayload) || strings.Contains(calls, "catalog.Get") || !strings.HasPrefix(calls, "sql.View,sql.State,fs.OpenStream") {
			t.Fatal(calls)
		}
	}
}
