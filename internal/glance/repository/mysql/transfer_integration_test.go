//go:build integration

package mysql_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
	"github.com/google/uuid"
)

// MUTATING opt-in, a NEW EMPTY independent schema only. No filesystem I/O or
// Keystone: this tests SQL persistence, not a complete upload or physical durability.
func TestMySQLTransferOperations(t *testing.T) {
	if os.Getenv("GLANCE_GO_MYSQL_TEST") != "1" {
		t.Fatal("explicit disposable MySQL opt-in required")
	}
	secret, err := config.ReadSecret(os.Getenv("GLANCE_GO_TEST_DSN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := strings.TrimSpace(string(secret))
	if err = mysql.ValidateTestDSN(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = mysql.CheckAccount(ctx, db, "migrate"); err != nil {
		t.Fatal(err)
	}
	var tables int
	if db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables) != nil || tables != 0 {
		t.Fatal("transfer suite requires a NEW EMPTY independent schema")
	}
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = mysql.Migrate(ctx, db); err != nil {
		t.Fatal("idempotence", err)
	}
	if err = mysql.VerifySchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := mysql.New(db, 5*time.Second)
	if err := r.PrepareStatements(ctx); err != nil {
		t.Fatal(err)
	}
	image, err := model.New(model.CreateInput{Properties: map[string]string{"preserve": "yes"}}, "transfer-fixture", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Create(ctx, image); err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	expected := int64(20)
	if _, err = r.ReserveData(ctx, image.ID, attempt, &expected, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReserveData(ctx, image.ID, uuid.NewString(), nil, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("reservation not fenced", err)
	}
	content := model.DataContent{Size: 20, Checksum: strings.Repeat("a", 32), SHA512: strings.Repeat("b", 128)}
	if err = r.StageData(ctx, image.ID, attempt, content, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitData(ctx, image.ID, attempt, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("committed before published", err)
	}
	if err = r.PublishData(ctx, image.ID, attempt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitData(ctx, image.ID, attempt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = mysql.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	r = mysql.New(db, 5*time.Second)
	state, err := r.DataState(ctx, image.ID)
	if err != nil || state.Status != "active" || state.Operation == nil || state.Operation.Phase != model.DataCommitted || state.HashAlgorithm == nil || *state.HashAlgorithm != "sha512" {
		t.Fatal(state, err)
	}
	// M2: DownloadView = the live image row and the snapshot state, one tx.
	viewImage, viewState, err := r.DownloadView(ctx, image.ID)
	if err != nil || viewImage.ID != image.ID || viewImage.Owner != image.Owner || viewImage.Status != "active" || viewImage.Visibility != image.Visibility || !reflect.DeepEqual(viewState, state) {
		t.Fatal("DownloadView differs", viewImage.Status, err)
	}
	// GET's non-locking read returns the same state and never waits for (or
	// blocks) a writer holding the row lock; the locking read does wait.
	snapshot, err := r.DataStateSnapshot(ctx, image.ID)
	if err != nil || !reflect.DeepEqual(snapshot, state) {
		t.Fatal("snapshot differs from locking read", snapshot, err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var held string
	if err = writer.QueryRowContext(ctx, "SELECT id FROM images WHERE id=? FOR UPDATE", image.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 2*time.Second)
	if _, err = r.DataStateSnapshot(readCtx, image.ID); err != nil {
		t.Fatal("snapshot blocked by a row lock", err)
	}
	cancelRead()
	lockedCtx, cancelLocked := context.WithTimeout(ctx, 500*time.Millisecond)
	if _, err = r.DataState(lockedCtx, image.ID); err == nil {
		t.Fatal("locking read did not wait for the writer")
	}
	cancelLocked()
	if err = writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, image.ID)
	if err != nil || got.Properties["preserve"] != "yes" {
		t.Fatal("metadata overwritten", err)
	}
	image2, err := model.New(model.CreateInput{}, "transfer-fixture", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Create(ctx, image2); err != nil {
		t.Fatal(err)
	}
	attempts := []string{uuid.NewString(), uuid.NewString()}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, a := range attempts {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			_, err := r.ReserveData(ctx, image2.ID, a, nil, time.Now())
			errs <- err
		}(a)
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, repository.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("concurrent reservations", success, conflicts)
	}
	state, err = r.DataState(ctx, image2.ID)
	if err != nil {
		t.Fatal(err)
	}
	current := state.Operation.AttemptID
	if err = r.BeginDataCleanup(ctx, image2.ID, current, "queued", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitData(ctx, image2.ID, current, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("cleanup did not fence finalization", err)
	}
	if err = r.CompleteDataCleanup(ctx, image2.ID, current, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := uuid.NewString()
	if _, err = r.ReserveData(ctx, image2.ID, next, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.StageData(ctx, image2.ID, current, content, time.Now()); !errors.Is(err, repository.ErrConflict) {
		t.Fatal("old callback accepted", err)
	}
	// Intentionally retained fixture rows. No automatic cleanup or schema deletion.
}
