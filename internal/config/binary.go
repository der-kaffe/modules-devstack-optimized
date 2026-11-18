package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Binary is operator policy. Neither paths, scope nor limits come from HTTP.
type Binary struct {
	Enabled bool
	// Backend is "filesystem" (default) or "rbd" (binary built with -tags rbd).
	Backend                                                                             string
	Root, Scope                                                                         string
	RBDConf, RBDUser, RBDPool                                                           string
	MaxUploadBytes                                                                      int64
	MaxConcurrent                                                                       int
	UploadTimeout, DownloadTimeout, ResolutionTimeout, RecoveryTimeout, ShutdownTimeout time.Duration
}

func loadBinary(c *Config, env func(string) string, mode string) error {
	// 16 concurrent transfers (was 2): official Glance has no such cap beyond its
	// workers; the campaigns ran with 16. Still bounded (1..64).
	b := Binary{MaxUploadBytes: 1 << 30, MaxConcurrent: 16, UploadTimeout: 10 * time.Minute, DownloadTimeout: 10 * time.Minute, ResolutionTimeout: 10 * time.Second, RecoveryTimeout: 5 * time.Minute, ShutdownTimeout: 30 * time.Second}
	if v := env("GLANCE_GO_BINARY_ENABLED"); v != "" {
		if v != "true" && v != "false" {
			return errors.New("GLANCE_GO_BINARY_ENABLED must be true or false")
		}
		b.Enabled = v == "true"
	}
	b.Root, b.Scope = env("GLANCE_GO_STORE_ROOT"), env("GLANCE_GO_STORE_SCOPE")
	b.Backend = env("GLANCE_GO_STORE_BACKEND")
	if b.Backend == "" {
		b.Backend = "filesystem"
	}
	b.RBDConf, b.RBDUser, b.RBDPool = env("GLANCE_GO_RBD_CONF"), env("GLANCE_GO_RBD_USER"), env("GLANCE_GO_RBD_POOL")
	if b.Backend != "filesystem" && b.Backend != "rbd" {
		return errors.New("GLANCE_GO_STORE_BACKEND must be filesystem or rbd")
	}
	if (mode == "serve" && b.Enabled) || mode == "store-init" {
		if b.Backend == "rbd" {
			if err := validateRBD(b); err != nil {
				return err
			}
		} else if !filepath.IsAbs(b.Root) || filepath.Clean(b.Root) != b.Root || b.Root == "/" {
			return errors.New("GLANCE_GO_STORE_ROOT must be a clean absolute private path")
		}
		id, err := uuid.Parse(b.Scope)
		if err != nil || id == uuid.Nil || id.String() != b.Scope {
			return errors.New("GLANCE_GO_STORE_SCOPE must be a canonical nonzero UUID")
		}
	}
	for _, p := range []struct {
		key    string
		target *time.Duration
		max    time.Duration
	}{
		{"GLANCE_GO_UPLOAD_TIMEOUT", &b.UploadTimeout, time.Hour},
		{"GLANCE_GO_DOWNLOAD_TIMEOUT", &b.DownloadTimeout, time.Hour},
		{"GLANCE_GO_RESOLUTION_TIMEOUT", &b.ResolutionTimeout, time.Minute},
		{"GLANCE_GO_RECOVERY_TIMEOUT", &b.RecoveryTimeout, 10 * time.Minute},
		{"GLANCE_GO_SHUTDOWN_TIMEOUT", &b.ShutdownTimeout, 5 * time.Minute},
	} {
		if v := env(p.key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > p.max {
				return fmt.Errorf("%s is outside its finite duration bounds", p.key)
			}
			*p.target = d
		}
	}
	if v := env("GLANCE_GO_MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > 1<<40 {
			return errors.New("GLANCE_GO_MAX_UPLOAD_BYTES must be 1..1099511627776")
		}
		b.MaxUploadBytes = n
	}
	if v := env("GLANCE_GO_MAX_CONCURRENT_TRANSFERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return errors.New("GLANCE_GO_MAX_CONCURRENT_TRANSFERS must be 1..64")
		}
		b.MaxConcurrent = n
	}
	if mode == "serve" && b.Enabled && (c.DependencyTimeout >= b.UploadTimeout || c.DependencyTimeout >= b.DownloadTimeout) {
		return errors.New("transfer timeouts must exceed dependency timeout")
	}
	// A root without the explicit switch must not silently look enabled.
	if mode == "serve" && !b.Enabled && (strings.TrimSpace(b.Root) != "" || b.Scope != "" || b.RBDPool != "") {
		return errors.New("store configuration requires GLANCE_GO_BINARY_ENABLED=true")
	}
	c.Binary = b
	return nil
}

var rbdIdentifier = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validateRBD keeps the two backends unambiguous: an RBD store never also names
// a filesystem root, and its cluster file, cephx user and pool are explicit.
func validateRBD(b Binary) error {
	if strings.TrimSpace(b.Root) != "" {
		return errors.New("GLANCE_GO_STORE_ROOT must be empty with the rbd backend")
	}
	if !filepath.IsAbs(b.RBDConf) || filepath.Clean(b.RBDConf) != b.RBDConf {
		return errors.New("GLANCE_GO_RBD_CONF must be a clean absolute path")
	}
	if !rbdIdentifier.MatchString(b.RBDUser) || !rbdIdentifier.MatchString(b.RBDPool) {
		return errors.New("GLANCE_GO_RBD_USER and GLANCE_GO_RBD_POOL must be plain names")
	}
	return nil
}
