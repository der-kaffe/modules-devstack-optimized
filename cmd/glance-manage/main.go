// Command glance-manage is the original's glance-manage in Go: the same
// commands, options, messages and exit codes over the catalog database
// named by [database] connection.
//
//	glance-manage [--config-file F]... [--config-dir D] db {version|check|sync|upgrade|version_control|expand|migrate|contract}
//	glance-manage ... db load_metadefs [--path P] [--merge] [--prefer_new] [--overwrite]
//	glance-manage ... db {unload_metadefs|export_metadefs [--path P]}
//	glance-manage ... db {purge|purge_images_table} [--age_in_days N] [--max_rows N]
//	glance-manage ... db_sync | db_version | db_load_metadefs [path [merge [prefer_new [overwrite]]]] | ...
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/repository/mysql"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type invocation struct {
	configFiles []string
	action      string
	positional  []string
	flags       map[string]string
}

func parse(args []string) (invocation, error) {
	inv := invocation{flags: map[string]string{}}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "--config-file":
			v, err := value()
			if err != nil {
				return inv, err
			}
			inv.configFiles = append(inv.configFiles, v)
		case "--config-dir":
			v, err := value()
			if err != nil {
				return inv, err
			}
			matches, _ := filepath.Glob(filepath.Join(v, "*.conf"))
			sort.Strings(matches)
			inv.configFiles = append(inv.configFiles, matches...)
		case "--path", "--age_in_days", "--max_rows", "--version":
			v, err := value()
			if err != nil {
				return inv, err
			}
			inv.flags[strings.TrimPrefix(name, "--")] = v
		case "--merge", "--prefer_new", "--overwrite":
			inv.flags[strings.TrimPrefix(name, "--")] = "true"
		case "--debug", "-d", "--verbose", "-v":
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return inv, fmt.Errorf("a command is required")
	}
	if rest[0] == "db" {
		if len(rest) < 2 {
			return inv, fmt.Errorf("Available actions for 'db': version check sync upgrade version_control expand migrate contract load_metadefs unload_metadefs export_metadefs purge purge_images_table")
		}
		inv.action, inv.positional = rest[1], rest[2:]
	} else if action, ok := strings.CutPrefix(rest[0], "db_"); ok {
		inv.action, inv.positional = action, rest[1:]
		// Legacy positional form: db_load_metadefs [path [merge [prefer_new [overwrite]]]].
		if action == "load_metadefs" || action == "export_metadefs" {
			for n, key := range []string{"path", "merge", "prefer_new", "overwrite"} {
				if n < len(inv.positional) && inv.flags[key] == "" {
					inv.flags[key] = inv.positional[n]
				}
			}
		}
	} else {
		return inv, fmt.Errorf("unknown command %s", rest[0])
	}
	return inv, nil
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func run(args []string, stdout, stderr io.Writer) int {
	inv, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	settings, err := config.LoadTool(inv.configFiles)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if settings.CatalogDSN == "" {
		fmt.Fprintln(stderr, "[database] connection is not configured")
		return 1
	}
	db, err := mysql.OpenCatalog(settings.CatalogDSN)
	if err != nil {
		fmt.Fprintln(stderr, "cannot open the catalog database")
		return 1
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	say := func(s string) { fmt.Fprintln(stdout, s) }
	fail := func(s string) int { fmt.Fprintln(stderr, s); return 1 }
	switch inv.action {
	case "version":
		rev, err := mysql.CatalogRevision(ctx, db)
		if err != nil {
			return fail(err.Error())
		}
		if rev == "" {
			say(`Database is either not under migration control or under legacy migration control, please run "glance-manage db sync" to place the database under alembic migration control.`)
			return 0
		}
		say(rev)
	case "check":
		rev, err := mysql.CatalogRevision(ctx, db)
		if err != nil {
			return fail(err.Error())
		}
		if rev == mysql.CatalogHead {
			say("Database is up to date. No upgrades needed.")
			return 0
		}
		say("Your database is not up to date. Your first step is to run `glance-manage db expand`.")
		return 3
	case "sync", "upgrade", "expand", "migrate", "contract", "version_control":
		if v := inv.flags["version"]; v != "" && v != "heads" && v != mysql.CatalogHead {
			return fail("Failed to sync database: ERROR: only revision " + mysql.CatalogHead + " is available")
		}
		created, err := mysql.SyncCatalog(ctx, db)
		if err != nil {
			return fail("Failed to sync database: ERROR: " + err.Error())
		}
		switch {
		case !created:
			say("Database is up to date. No migrations needed.")
		case inv.action == "sync":
			say("Database is synced successfully.")
		case inv.action == "upgrade":
			say("Upgraded database, current revision(s): ['" + mysql.CatalogHead + "']")
		case inv.action == "version_control":
			say("Placed database under migration control at revision: " + mysql.CatalogHead)
		default:
			say("Database is up to date. No migrations needed.")
		}
	case "load_metadefs":
		path := settings.MetadataPath
		if p := inv.flags["path"]; p != "" {
			path = p
		}
		o := mysql.MetadefLoadOptions{Merge: truthy(inv.flags["merge"]), PreferNew: truthy(inv.flags["prefer_new"]), Overwrite: truthy(inv.flags["overwrite"])}
		if err = mysql.LoadMetadefs(ctx, db, path, o, func(s string) { fmt.Fprintln(stderr, s) }); err != nil {
			return fail(err.Error())
		}
	case "unload_metadefs":
		if err = mysql.UnloadMetadefs(ctx, db); err != nil {
			return fail(err.Error())
		}
	case "export_metadefs":
		path := settings.MetadataPath
		if p := inv.flags["path"]; p != "" {
			path = p
		}
		files, err := mysql.ExportMetadefs(ctx, db, path)
		if err != nil {
			return fail(err.Error())
		}
		for _, f := range files {
			fmt.Fprintln(stderr, "Namespace saved in "+f)
		}
	case "purge", "purge_images_table":
		age, max := 30, 100
		if inv.action == "purge_images_table" {
			age = 180
		}
		if v, ok := inv.flags["age_in_days"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail("Invalid int value for age_in_days: " + v)
			}
			age = n
		}
		if v, ok := inv.flags["max_rows"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail("Invalid int value for max_rows: " + v)
			}
			max = n
		}
		if age < 0 {
			return fail("Must supply a non-negative value for age.")
		}
		if age >= int(time.Now().Unix()/86400) {
			return fail("Maximal age is count of days since epoch.")
		}
		if max < -1 {
			return fail("Minimal rows limit is -1.")
		}
		if inv.action == "purge" {
			rows, err := mysql.Purge(ctx, db, age, max)
			if err != nil {
				return fail(err.Error())
			}
			for _, t := range append([]string{"task_info"}, "image_properties", "image_tags", "image_locations", "image_members", "tasks") {
				fmt.Fprintf(stderr, "Deleted %d row(s) from table %s\n", rows[t], t)
			}
		} else {
			n, err := mysql.PurgeImages(ctx, db, age, max)
			if err != nil {
				return fail(err.Error())
			}
			fmt.Fprintf(stderr, "Deleted %d row(s) from table images\n", n)
		}
	default:
		return fail("unknown db action " + inv.action)
	}
	return 0
}
