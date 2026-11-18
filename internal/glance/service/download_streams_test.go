package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// Downloads beyond MaxDownloadStreams wait in line (as requests beyond official
// Glance's uwsgi workers) instead of being refused, and never leak a slot.
func streamFixture(t *testing.T, streams int) *UploadCoordinator {
	t.Helper()
	u, d, fs, _ := uploadFixture(t)
	uploadSeed(d, model.DataCommitted)
	fs.blob = append([]byte{}, uploadPayload...)
	u.limits.MaxDownloadStreams = streams
	u.streams = make(chan struct{}, streams)
	return u
}

type opened struct {
	out *Download
	err error
}

func openAsync(u *UploadCoordinator, ctx context.Context) chan opened {
	done := make(chan opened, 1)
	go func() {
		out, err := u.OpenDownload(ctx, identity("reader"), uploadImageID)
		done <- opened{out, err}
	}()
	return done
}

func waiting(t *testing.T, ch chan opened) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatal("download was not queued", got.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDownloadBeyondStreamsWaitsInsteadOfRefusing(t *testing.T) {
	u := streamFixture(t, 1)
	first, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	second := openAsync(u, context.Background())
	waiting(t, second)
	u.mu.Lock()
	readers := u.readers
	u.mu.Unlock()
	if readers != 1 {
		t.Fatalf("a queued download must not hold transfer capacity: readers=%d", readers)
	}
	got, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	_ = first.Body.Close() // idempotent: one slot back, not two
	if string(got) != string(uploadPayload) {
		t.Fatal("first body")
	}
	next := <-second
	if next.err != nil {
		t.Fatal(next.err)
	}
	_ = next.out.Body.Close()
	if len(u.streams) != 0 {
		t.Fatalf("slots leaked: %d", len(u.streams))
	}
}

func TestQueuedDownloadEndsWithItsClientContext(t *testing.T) {
	u := streamFixture(t, 1)
	first, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queued := openAsync(u, ctx)
	waiting(t, queued)
	cancel()
	if got := <-queued; !errors.Is(got.err, context.Canceled) || got.out != nil {
		t.Fatal("cancelled wait must fail with the context error", got.err)
	}
	_ = first.Body.Close()
	again, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal("slot lost after a cancelled wait", err)
	}
	_ = again.Body.Close()
}

func TestStopAdmissionWakesQueuedDownloads(t *testing.T) {
	u := streamFixture(t, 1)
	first, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	queued := openAsync(u, context.Background())
	waiting(t, queued)
	u.StopAdmission()
	if got := <-queued; !errors.Is(got.err, ErrUploadNotReady) {
		t.Fatal("queued download must not outlive admission", got.err)
	}
	_ = first.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := u.WaitIdle(ctx); err != nil {
		t.Fatal("not idle after the open body closed", err)
	}
	if len(u.streams) != 0 {
		t.Fatal("slot leaked at shutdown")
	}
}

// Forbidden or missing images answer at once, never after waiting in line.
func TestUnauthorizedDownloadNeverWaits(t *testing.T) {
	u := streamFixture(t, 1)
	first, err := u.OpenDownload(context.Background(), identity("reader"), uploadImageID)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	done := make(chan error, 1)
	go func() {
		_, err := u.OpenDownload(context.Background(), identity("member"), uploadImageID)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("non-reader accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("authorization failure waited for a stream")
	}
}

func TestStreamLimitsAreValidated(t *testing.T) {
	u, _, _, _ := uploadFixture(t)
	for _, streams := range []int{-1, u.limits.MaxConcurrent + 1} {
		limits := u.limits
		limits.MaxDownloadStreams = streams
		if _, err := newUploadCoordinator(u.images, u.transfers, u.bytes, limits); err == nil {
			t.Fatal("accepted stream limit", streams)
		}
	}
	limits := u.limits
	limits.MaxDownloadStreams = 0
	plain, err := newUploadCoordinator(u.images, u.transfers, u.bytes, limits)
	if err != nil || plain.streams != nil {
		t.Fatal("0 must keep downloads unqueued", err)
	}
}
