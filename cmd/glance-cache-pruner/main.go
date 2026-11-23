// Command glance-cache-pruner is the original's: it deletes the least
// recently accessed cached images until the cache fits image_cache_max_size.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

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
	max := int64(10 << 30)
	if v := s.Get("DEFAULT", "image_cache_max_size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			max = n
		}
	}
	ctx := context.Background()
	cache, err := service.NewCacheMaintenance(ctx, s.Cache, repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	n, bytes, err := cache.Prune(ctx, max)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Pruned %d files removing %d bytes\n", n, bytes)
}
