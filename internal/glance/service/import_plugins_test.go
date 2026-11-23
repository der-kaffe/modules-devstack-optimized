package service

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
)

func pluginFixture(t *testing.T, disk string) (*Importer, *memberStub) {
	s, repo := sharedFixture("private", "importing")
	repo.image.DiskFormat = &disk
	dir := t.TempDir()
	return &Importer{cfg: config.ImportSettings{StagingDir: dir, ConversionFormat: "raw", InjectIgnore: []string{"admin"},
		Inject: map[string]string{"injected": "yes"}, VMDKAllowed: []string{"streamOptimized", "monolithicSparse"}}, svc: s, now: time.Now}, repo
}

func TestImportPlugins(t *testing.T) {
	ctx := context.Background()
	if _, err := exec.LookPath("qemu-img"); err == nil {
		m, repo := pluginFixture(t, "qcow2")
		if out, err := exec.Command("qemu-img", "create", "-q", "-f", "qcow2", m.staged(sharedID), "8M").CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
		if err := m.convert(ctx, sharedID); err != nil || *repo.image.DiskFormat != "raw" {
			t.Fatal("conversion", err, *repo.image.DiskFormat)
		}
		if info, _ := os.Stat(m.staged(sharedID)); info.Size() != 8<<20 {
			t.Fatal("raw size", info.Size())
		}
		// A qcow2 with a backing file is refused before any conversion.
		m, _ = pluginFixture(t, "qcow2")
		base := filepath.Join(t.TempDir(), "base.qcow2")
		_ = exec.Command("qemu-img", "create", "-q", "-f", "qcow2", base, "8M").Run()
		_ = exec.Command("qemu-img", "create", "-q", "-f", "qcow2", "-b", base, "-F", "qcow2", m.staged(sharedID)).Run()
		if err := m.convert(ctx, sharedID); err == nil || err.Error() != "Image has disallowed configuration" {
			t.Fatal("backing file accepted", err)
		}
		// Declared qcow2 but raw content.
		m, _ = pluginFixture(t, "qcow2")
		_ = os.WriteFile(m.staged(sharedID), bytes.Repeat([]byte{1}, 4096), 0o600)
		if err := m.convert(ctx, sharedID); err == nil || err.Error() != "Image format mismatch" {
			t.Fatal("mismatch accepted", err)
		}
	}
	m, _ := pluginFixture(t, "raw")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("payload"))
	_ = w.Close()
	_ = os.WriteFile(m.staged(sharedID), gz.Bytes(), 0o600)
	if err := m.decompress(sharedID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.staged(sharedID)); string(b) != "payload" {
		t.Fatal("gzip not decompressed", string(b))
	}
	var zb bytes.Buffer
	z := zip.NewWriter(&zb)
	fw, _ := z.Create("disk.img")
	_, _ = fw.Write([]byte("zipped"))
	_ = z.Close()
	_ = os.WriteFile(m.staged(sharedID), zb.Bytes(), 0o600)
	if err := m.decompress(sharedID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.staged(sharedID)); string(b) != "zipped" {
		t.Fatal("zip not decompressed", string(b))
	}
	m, repo := pluginFixture(t, "raw")
	if err := m.inject(ctx, auth.Identity{Roles: []string{"admin"}}, sharedID); err != nil || repo.image.Properties["injected"] != "" {
		t.Fatal("admin must be ignored", err)
	}
	if err := m.inject(ctx, auth.Identity{Roles: []string{"member"}}, sharedID); err != nil || repo.image.Properties["injected"] != "yes" {
		t.Fatal("not injected", err, repo.image.Properties)
	}
}
