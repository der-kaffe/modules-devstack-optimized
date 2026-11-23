package mysql

import (
	"strings"
	"testing"
)

// The embedded schema is the original's 16 tables at its alembic head.
func TestCatalogSchemaStatements(t *testing.T) {
	stmts := catalogStatements()
	if len(stmts) != 16 {
		t.Fatal(len(stmts))
	}
	for _, s := range stmts {
		if !strings.HasPrefix(s, "CREATE TABLE `") || !strings.Contains(s, "ENGINE=InnoDB") {
			t.Fatal(s[:60])
		}
	}
	joined := strings.Join(stmts, "\n")
	for _, table := range []string{"alembic_version", "images", "image_members", "tasks", "task_info", "cached_images", "node_reference", "metadef_namespace_resource_types"} {
		if !strings.Contains(joined, "CREATE TABLE `"+table+"`") {
			t.Fatal("missing", table)
		}
	}
}
