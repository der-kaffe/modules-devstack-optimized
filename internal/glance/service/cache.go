package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/auth"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// CacheRepository is the original's centralized_db cache tables.
type CacheRepository interface {
	CacheNode(ctx context.Context, url string) (int64, error)
	CachedImages(ctx context.Context, node int64) ([]model.CachedImage, error)
	CacheInsert(ctx context.Context, node int64, imageID string, size int64, checksum *string, now time.Time) error
	CacheHit(ctx context.Context, node int64, imageID string, now time.Time) error
	CacheDelete(ctx context.Context, node int64, imageID string) error
}

// ImageCache is the original's image cache with the centralized_db driver,
// on the same image_cache_dir layout: complete copies named by image ID,
// incomplete/, invalid/ and queue/ (images queued for the prefetcher).
// A complete download is cached as the original's cache middleware does;
// here the copy is written in the background from the store, so the first
// download keeps sendfile instead of being teed through user space.
type ImageCache struct {
	cfg    config.CacheSettings
	repo   CacheRepository
	data   *UploadCoordinator
	node   int64
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	hits     map[string]int64 // accesses not yet written (update_hit_count)
	hitTimer *time.Timer
	inflight map[string]bool
	slots    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	work     sync.WaitGroup
}

func NewImageCache(ctx context.Context, cfg config.CacheSettings, repo CacheRepository, data *UploadCoordinator, logger *slog.Logger) (*ImageCache, error) {
	if data == nil {
		return nil, errors.New("invalid image cache configuration")
	}
	return newImageCache(ctx, cfg, repo, data, logger)
}

// NewCacheMaintenance is the cache of the glance-cache-pruner and
// glance-cache-cleaner tools: no downloads or prefetcher.
func NewCacheMaintenance(ctx context.Context, cfg config.CacheSettings, repo CacheRepository) (*ImageCache, error) {
	return newImageCache(ctx, cfg, repo, nil, nil)
}

func newImageCache(ctx context.Context, cfg config.CacheSettings, repo CacheRepository, data *UploadCoordinator, logger *slog.Logger) (*ImageCache, error) {
	if cfg.Dir == "" || cfg.NodeURL == "" || repo == nil {
		return nil, errors.New("invalid image cache configuration")
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "incomplete"), filepath.Join(cfg.Dir, "invalid"), filepath.Join(cfg.Dir, "queue")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, errors.New("cannot create the image cache directories")
		}
	}
	node, err := repo.CacheNode(ctx, cfg.NodeURL)
	if err != nil {
		return nil, err
	}
	c := &ImageCache{cfg: cfg, repo: repo, data: data, node: node, now: time.Now, logger: logger, inflight: map[string]bool{}, slots: make(chan struct{}, 2)}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c, nil
}

func (c *ImageCache) path(id string) string { return filepath.Join(c.cfg.Dir, id) }

// open returns the cached copy of an image of the given size, if any.
func (c *ImageCache) open(id string, size int64) (*os.File, bool) {
	f, err := os.Open(c.path(id))
	if err != nil {
		return nil, false
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		_ = f.Close()
		return nil, false
	}
	return f, true
}

// hitFlushEvery is how often the access counters reach the database. The
// original updates the row on every download; with many clients on one image
// that is a lock queue on a single row, so the counts are summed in memory
// and written once per interval (the totals are the same).
const hitFlushEvery = time.Second

// hitCounter is implemented by the MySQL repository: n accesses in one UPDATE.
type hitCounter interface {
	CacheHits(ctx context.Context, node int64, imageID string, n int64, now time.Time) error
}

func (c *ImageCache) hit(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hits == nil {
		c.hits = map[string]int64{}
	}
	c.hits[id]++
	if c.hitTimer == nil {
		c.work.Add(1)
		c.hitTimer = time.AfterFunc(hitFlushEvery, func() {
			defer c.work.Done()
			c.flushHits(c.ctx)
		})
	}
}

// flushHits writes the summed accesses.
func (c *ImageCache) flushHits(ctx context.Context) {
	c.mu.Lock()
	batch := c.hits
	c.hits, c.hitTimer = nil, nil
	c.mu.Unlock()
	for id, n := range batch {
		if hc, ok := c.repo.(hitCounter); ok {
			_ = hc.CacheHits(ctx, c.node, id, n, c.now())
			continue
		}
		for range n {
			_ = c.repo.CacheHit(ctx, c.node, id, c.now())
		}
	}
}

func (c *ImageCache) background(fn func(context.Context)) {
	c.work.Add(1)
	go func() {
		defer c.work.Done()
		fn(c.ctx)
	}()
}

// fill caches an image from src (opened in the background), verifying size
// and MD5 checksum as the original's get_caching_iter; a bad copy goes to
// invalid/. One copy per image at a time, two images at once.
func (c *ImageCache) fill(id string, size int64, checksum string, src func(context.Context) (io.ReadCloser, int64, string, error), done chan struct{}) {
	finish := func() {
		if done != nil {
			close(done)
		}
	}
	c.mu.Lock()
	if c.inflight[id] {
		c.mu.Unlock()
		finish()
		return
	}
	c.inflight[id] = true
	c.mu.Unlock()
	c.background(func(ctx context.Context) {
		defer finish()
		defer func() { c.mu.Lock(); delete(c.inflight, id); c.mu.Unlock() }()
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-c.slots }()
		if _, err := os.Stat(c.path(id)); err == nil {
			return
		}
		r, known, knownSum, err := src(ctx)
		if err != nil {
			return
		}
		defer r.Close()
		if size < 0 { // the source knows: a prefetch
			size, checksum = known, knownSum
		}
		incomplete := filepath.Join(c.cfg.Dir, "incomplete", id)
		f, err := os.Create(incomplete)
		if err != nil {
			return
		}
		h := md5.New()
		n, err := io.Copy(io.MultiWriter(f, h), r)
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil || n != size || checksum != "" && hex.EncodeToString(h.Sum(nil)) != checksum {
			_ = os.Rename(incomplete, filepath.Join(c.cfg.Dir, "invalid", id))
			return
		}
		if err = os.Rename(incomplete, c.path(id)); err != nil {
			_ = os.Remove(incomplete)
			return
		}
		_ = os.Remove(filepath.Join(c.cfg.Dir, "queue", id))
		var sum *string
		if checksum != "" {
			sum = &checksum
		}
		if err = c.repo.CacheInsert(ctx, c.node, id, size, sum, c.now()); err != nil && c.logger != nil {
			c.logger.Warn("cached image not recorded", "error_kind", "cache_db")
		}
	})
}

// Close stops the prefetcher and waits for background copies.
func (c *ImageCache) Close() {
	c.cancel()
	c.mu.Lock()
	if c.hitTimer != nil && c.hitTimer.Stop() {
		c.work.Done() // the pending flush will not run: do it now
		c.mu.Unlock()
		c.flushHits(context.Background())
	} else {
		c.mu.Unlock()
	}
	c.work.Wait()
}

// Prefetch runs the original's periodic prefetcher (cache_prefetcher_interval)
// over the queued images until Close.
func (c *ImageCache) Prefetch() {
	c.background(func(ctx context.Context) {
		t := time.NewTicker(c.cfg.PrefetchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.prefetchOnce(ctx)
			}
		}
	})
}

var cacheAdmin = auth.Identity{UserID: "image-cache", ProjectID: "image-cache", Roles: []string{"admin", "reader"}}

func (c *ImageCache) prefetchOnce(ctx context.Context) {
	queued, _ := c.Queued()
	for _, id := range queued {
		if _, err := os.Stat(c.path(id)); err == nil {
			_ = os.Remove(filepath.Join(c.cfg.Dir, "queue", id))
			continue
		}
		done := make(chan struct{})
		c.fill(id, -1, "", func(ctx context.Context) (io.ReadCloser, int64, string, error) {
			who := cacheAdmin
			who.ExpiresAt = c.now().Add(time.Hour)
			d, err := c.data.OpenDownload(ctx, who, id)
			if err != nil {
				_ = os.Remove(filepath.Join(c.cfg.Dir, "queue", id)) // gone or not active: unqueue
				return nil, 0, "", err
			}
			return d.Body, d.Size, d.Checksum, nil
		}, done)
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// Queued lists images queued for caching, oldest first.
func (c *ImageCache) Queued() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(c.cfg.Dir, "queue"))
	if err != nil {
		return nil, err
	}
	type q struct {
		id string
		at time.Time
	}
	list := []q{}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			if _, err := model.CanonicalID(e.Name()); err == nil {
				list = append(list, q{e.Name(), info.ModTime()})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.id)
	}
	return out, nil
}

func (c *ImageCache) Cached(ctx context.Context) ([]model.CachedImage, error) {
	return c.repo.CachedImages(ctx, c.node)
}

// Queue is queue_image: no-op when the image is already cached.
func (c *ImageCache) Queue(id string) error {
	if _, err := os.Stat(c.path(id)); err == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(c.cfg.Dir, "queue", id), nil, 0o640)
}

// Delete is delete_cached_image ("" for every cached image of this node).
func (c *ImageCache) Delete(ctx context.Context, id string) error {
	if id == "" {
		cached, err := c.repo.CachedImages(ctx, c.node)
		if err != nil {
			return err
		}
		for _, i := range cached {
			_ = os.Remove(c.path(i.ImageID))
		}
		return c.repo.CacheDelete(ctx, c.node, "")
	}
	_ = os.Remove(c.path(id))
	return c.repo.CacheDelete(ctx, c.node, id)
}

// Unqueue is delete_queued_image ("" for every queued image).
func (c *ImageCache) Unqueue(id string) error {
	if id != "" {
		err := os.Remove(filepath.Join(c.cfg.Dir, "queue", id))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	queued, err := c.Queued()
	for _, q := range queued {
		_ = os.Remove(filepath.Join(c.cfg.Dir, "queue", q))
	}
	return err
}

// SetCache serves and fills downloads through the image cache.
func (u *UploadCoordinator) SetCache(c *ImageCache) { u.cache = c }

// Size is get_cache_size: the bytes of the complete cached files.
func (c *ImageCache) Size() (int64, error) {
	entries, err := os.ReadDir(c.cfg.Dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

// Prune is the original's image_cache.prune (glance-cache-pruner): least
// recently accessed images are deleted until the cache fits max bytes.
func (c *ImageCache) Prune(ctx context.Context, max int64) (files int, bytes int64, err error) {
	current, err := c.Size()
	if err != nil || current <= max {
		return 0, 0, err
	}
	cached, err := c.repo.CachedImages(ctx, c.node)
	if err != nil {
		return 0, 0, err
	}
	sort.SliceStable(cached, func(i, j int) bool { return cached[i].LastAccessed.Before(cached[j].LastAccessed) })
	for _, image := range cached {
		if current <= max {
			break
		}
		if err = c.Delete(ctx, image.ImageID); err != nil {
			return files, bytes, err
		}
		files, bytes, current = files+1, bytes+image.Size, current-image.Size
	}
	return files, bytes, nil
}

// Clean is the original's image_cache.clean (glance-cache-cleaner): invalid
// files are deleted, and incomplete ones older than stall.
func (c *ImageCache) Clean(stall time.Duration) (invalid, stalled int, err error) {
	for _, sub := range []string{"invalid", "incomplete"} {
		entries, e := os.ReadDir(filepath.Join(c.cfg.Dir, sub))
		if e != nil {
			continue
		}
		for _, entry := range entries {
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() {
				continue
			}
			if sub == "incomplete" && c.now().Sub(info.ModTime()) < stall {
				continue
			}
			if e = os.Remove(filepath.Join(c.cfg.Dir, sub, entry.Name())); e != nil {
				err = e
				continue
			}
			if sub == "invalid" {
				invalid++
			} else {
				stalled++
			}
		}
	}
	return invalid, stalled, err
}
