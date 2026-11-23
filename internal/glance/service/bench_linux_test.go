//go:build linux

package service

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
	"github.com/google/uuid"
)

// Whole service upload (Stage, Publish, post-publish check and SQL
// checkpoints) on the real filesystem store; SQL is the in-memory double.
// Developer-host measurement, not a campaign.
func BenchmarkServiceUpload64MiBRealStore(b *testing.B) { benchmarkServiceUpload(b, false) }

// The same with [image_format] inspection, as DevStack runs it.
func BenchmarkServiceUpload64MiBInspected(b *testing.B) { benchmarkServiceUpload(b, true) }

func benchmarkServiceUpload(b *testing.B, inspect bool) {
	root := b.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		b.Fatal(err)
	}
	scope := uuid.NewString()
	if err := store.Initialize(root, scope); err != nil {
		b.Fatal(err)
	}
	fs, err := testStore(root, scope)
	if err != nil {
		b.Fatal(err)
	}
	defer fs.Close()
	log := &uploadEvents{}
	sql := &uploadSQLDouble{log: log, faults: map[string]uploadSQLFault{}, failReadAt: map[int]bool{}, otherStates: map[string]model.DataState{}}
	fresh := func() {
		id := uuid.NewString()
		image, err := model.New(model.CreateInput{ID: &id, DiskFormat: uploadPtr("raw"), ContainerFormat: uploadPtr("bare")}, "p", time.Now())
		if err != nil {
			b.Fatal(err)
		}
		sql.mu.Lock()
		sql.image, sql.op, sql.reads = image, nil, 0
		sql.mu.Unlock()
		log.mu.Lock()
		log.calls = nil
		log.mu.Unlock()
	}
	fresh()
	u, err := NewUploadCoordinator(uploadCatalogDouble{sql: sql}, sql, fs, UploadLimits{MaxBytes: 1 << 30, MaxConcurrent: 2, ResolutionTimeout: time.Minute, RecoveryTimeout: time.Minute, Inspect: inspect, RequireFormat: inspect})
	if err != nil {
		b.Fatal(err)
	}
	defer u.Close()
	if _, err = u.RecoverUploads(context.Background()); err != nil {
		b.Fatal(err)
	}
	data := bytes.Repeat([]byte("glance-go-bench-"), (64<<20)/16)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		fresh()
		b.StartTimer()
		if _, err := u.Upload(context.Background(), identity("member"), sql.image.ID, bytes.NewReader(data), nil); err != nil {
			b.Fatal(err)
		}
	}
}
