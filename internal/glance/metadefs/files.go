// Package metadefs holds the original's metadata definition files
// (etc/metadefs) so a Go-only deployment can install and load them.
package metadefs

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed data/*.json
var files embed.FS

// Install writes the embedded files into dir (metadata_source_path),
// keeping files that already exist there, as DevStack copies them.
func Install(dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	entries, err := fs.ReadDir(files, "data")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := files.ReadFile("data/" + e.Name())
		if err != nil {
			return n, err
		}
		if err = os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
