// Command glance-scrubber is the original's: images left pending_delete by
// delayed_delete longer than scrub_time have their data removed and become
// deleted. With --daemon (or daemon = True) it repeats every wakeup_time.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/tooling"
)

func main() {
	files, rest := tooling.Args(os.Args[1:])
	s, db, repo, err := tooling.Open(files)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	seconds := func(key string, def int) time.Duration {
		if v := s.Get("DEFAULT", key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return time.Duration(n) * time.Second
			}
		}
		return time.Duration(def) * time.Second
	}
	daemon := false
	for _, a := range rest {
		daemon = daemon || a == "--daemon" || a == "-D"
	}
	if v := s.Get("DEFAULT", "daemon"); v == "True" || v == "true" {
		daemon = true
	}
	scrubTime, wakeup := seconds("scrub_time", 0), seconds("wakeup_time", 300)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		n, err := repo.Scrub(ctx, scrubTime, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "scrub failed:", err)
			if !daemon {
				os.Exit(1)
			}
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "Scrubbed %d image(s)\n", n)
		}
		if !daemon {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wakeup):
		}
	}
}
