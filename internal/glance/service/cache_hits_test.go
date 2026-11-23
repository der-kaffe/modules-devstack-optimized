package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

type hitRepo struct {
	mu    sync.Mutex
	calls int
	total int64
}

func (r *hitRepo) CacheNode(context.Context, string) (int64, error)                 { return 1, nil }
func (r *hitRepo) CachedImages(context.Context, int64) ([]model.CachedImage, error) { return nil, nil }
func (r *hitRepo) CacheInsert(context.Context, int64, string, int64, *string, time.Time) error {
	return nil
}
func (r *hitRepo) CacheDelete(context.Context, int64, string) error { return nil }
func (r *hitRepo) CacheHit(ctx context.Context, node int64, id string, now time.Time) error {
	return r.CacheHits(ctx, node, id, 1, now)
}
func (r *hitRepo) CacheHits(_ context.Context, _ int64, _ string, n int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.total += n
	return nil
}

func TestCacheHitsAreSummedAndNeverLost(t *testing.T) {
	repo := &hitRepo{}
	c, err := NewCacheMaintenance(context.Background(), config.CacheSettings{Dir: t.TempDir(), NodeURL: "http://node"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 250 {
				c.hit("img")
			}
		}()
	}
	wg.Wait()
	c.Close() // flushes what is pending, without waiting for the interval
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.total != 4000 {
		t.Fatal("accesses written", repo.total, "of 4000")
	}
	if repo.calls > 2 {
		t.Fatal("4000 accesses took", repo.calls, "UPDATEs")
	}
}

func TestCacheHitsReachTheDatabaseOnTheInterval(t *testing.T) {
	repo := &hitRepo{}
	c, _ := NewCacheMaintenance(context.Background(), config.CacheSettings{Dir: t.TempDir(), NodeURL: "http://node"}, repo)
	defer c.Close()
	c.hit("a")
	c.hit("a")
	c.hit("b")
	time.Sleep(hitFlushEvery + 400*time.Millisecond)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.total != 3 || repo.calls != 2 {
		t.Fatal("after the interval", repo.total, repo.calls)
	}
}
