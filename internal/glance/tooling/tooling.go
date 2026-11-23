// Package tooling is the shared start of the original's command-line tools
// (glance-cache-pruner, glance-cache-cleaner, glance-scrubber): the same
// --config-file/--config-dir options, the catalog database and repository.
package tooling

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
)

// Args splits the oslo options from the tool's own flags.
func Args(args []string) (files []string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, has := strings.Cut(a, "=")
		if (name == "--config-file" || name == "--config-dir") && !has && i+1 < len(args) {
			i++
			value, has = args[i], true
		}
		switch {
		case name == "--config-file" && has:
			files = append(files, value)
		case name == "--config-dir" && has:
			m, _ := filepath.Glob(filepath.Join(value, "*.conf"))
			sort.Strings(m)
			files = append(files, m...)
		default:
			rest = append(rest, a)
		}
	}
	return files, rest
}

// Open loads the configuration and opens the catalog repository.
func Open(files []string) (config.ToolSettings, *sql.DB, *mysql.Repository, error) {
	s, err := config.LoadTool(files)
	if err != nil {
		return s, nil, nil, err
	}
	if s.CatalogDSN == "" {
		return s, nil, nil, errors.New("[database] connection is not configured")
	}
	db, err := mysql.OpenCatalog(s.CatalogDSN)
	if err != nil {
		return s, nil, nil, err
	}
	store := s.StoreDir
	if store == "" {
		store = "/var/lib/glance/images"
	}
	r, err := mysql.New(db, 30*time.Second, s.Catalog, store)
	if err != nil {
		_ = db.Close()
		return s, nil, nil, err
	}
	return s, db, r, nil
}
