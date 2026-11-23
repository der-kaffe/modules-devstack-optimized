// Command glance-cache-cleaner is the original's: it deletes invalid cached
// files and incomplete ones older than image_cache_stall_time.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/service"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/tooling"
)

func main() {
	files, _ := tooling.Args(os.Args[1:])
	s, db, repo, err := tooling.Open(files)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	stall := 86400
	if v := s.Get("DEFAULT", "image_cache_stall_time"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			stall = n
		}
	}
	cache, err := service.NewCacheMaintenance(context.Background(), s.Cache, repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	invalid, stalled, err := cache.Clean(time.Duration(stall) * time.Second)
	fmt.Fprintf(os.Stderr, "Cleaned %d invalid and %d stalled cache files\n", invalid, stalled)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
