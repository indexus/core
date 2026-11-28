package indexus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// ReadMode is independent of write authority. A cached result never grants
// ownership. Fresh checks complete durable inventories; it is not a distributed
// transaction snapshot or a linearizable multi-zone read.
type ReadMode string

const (
	ReadFresh        ReadMode = "fresh"
	ReadCached       ReadMode = "cached"
	ReadCacheOnly    ReadMode = "cache-only"
	ReadStaleIfError ReadMode = "stale-if-error"
)

type ReadOptions struct {
	Mode ReadMode
	// MaxAge is required for cache modes. It bounds time since this node obtained
	// the result, not the age of remote contributions in an aggregate summary.
	MaxAge time.Duration
}

type ReadResult struct {
	Contact    Contact   `json:"-"`
	Set        *Set      `json:"set"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	// FallbackError exposes the failed fresh attempt when stale data is used.
	FallbackError string `json:"fallback_error,omitempty"`
}

// ErrReadCacheMiss means the requested zone has no recent, locally retained answer.
var ErrReadCacheMiss = errors.New("zone read cache miss")

const readCacheEntries = 32
const readCacheBytes = 16 << 20

type cachedRead struct {
	set   *Set
	at    time.Time
	bytes int
}
type readCache struct {
	mu      sync.Mutex
	entries map[Key]cachedRead
	bytes   int
}

func (c *readCache) put(key Key, set *Set, at time.Time) {
	copy, size := set.Clone(readCacheBytes)
	if copy == nil {
		return
	}
	size += len(key.Collection) + len(key.Location)
	if size > readCacheBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[Key]cachedRead)
	}
	if old, ok := c.entries[key]; ok {
		c.bytes -= old.bytes
		delete(c.entries, key)
	}
	for len(c.entries) >= readCacheEntries || c.bytes+size > readCacheBytes {
		var oldest Key
		var stamp time.Time
		for k, v := range c.entries {
			if stamp.IsZero() || v.at.Before(stamp) {
				oldest, stamp = k, v.at
			}
		}
		c.bytes -= c.entries[oldest].bytes
		delete(c.entries, oldest)
	}
	c.entries[key] = cachedRead{copy, at, size}
	c.bytes += size
}

// forget drops every cached read of a collection.
func (c *readCache) forget(collection string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if key.Collection == collection {
			c.bytes -= entry.bytes
			delete(c.entries, key)
		}
	}
}

func (c *readCache) get(key Key, maxAge time.Duration, now time.Time) (*Set, time.Time) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if !ok || now.Before(entry.at) || now.Sub(entry.at) > maxAge {
		return nil, time.Time{}
	}
	copy, _ := entry.set.Clone(readCacheBytes)
	return copy, entry.at
}

// ReadContext exposes availability/freshness choices explicitly. Fresh waits
// through recoverable handoff states until ctx's deadline (at most 30s). Cached
// serves a bounded previous result first, then tries a fresh read. StaleIfError
// tries a fresh inventory once and falls back only on failure. Cancellation is
// never hidden by a cached success. There is no fabricated empty cache miss.
func (n *Node) ReadContext(ctx context.Context, collection, location string, options ReadOptions) (ReadResult, error) {
	if err := options.validate(); err != nil {
		return ReadResult{}, err
	}
	zone, err := NewZoneKey(collection, location)
	if err != nil {
		return ReadResult{}, err
	}
	if n.mesh == nil {
		return ReadResult{}, ErrNotReady
	}
	ctx, done := n.mesh.clientActivity.Begin(ctx)
	defer done()
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	key := Key{Collection: zone.Collection, Location: zone.Location}
	cached := func() (ReadResult, bool) {
		if ctx.Err() != nil {
			return ReadResult{}, false
		}
		select {
		case n.mesh.readSlots <- struct{}{}:
		case <-ctx.Done():
			return ReadResult{}, false
		}
		defer func() { <-n.mesh.readSlots }()
		set, at := n.mesh.reads.get(key, options.MaxAge, time.Now())
		if set == nil || ctx.Err() != nil {
			return ReadResult{}, false
		}
		return ReadResult{Set: set, Source: "cache", ObservedAt: at}, true
	}
	if options.Mode == ReadCached || options.Mode == ReadCacheOnly {
		if out, ok := cached(); ok {
			return out, nil
		}
		if options.Mode == ReadCacheOnly {
			if err := ctx.Err(); err != nil {
				return ReadResult{}, err
			}
			return ReadResult{}, ErrReadCacheMiss
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var contact Contact
	var set *Set
	for attempt := 0; ; attempt++ {
		contact, set, err = n.GetSetContext(readCtx, zone.Collection, zone.Location, true, nil, true)
		if err == nil {
			if err := ctx.Err(); err != nil {
				return ReadResult{}, err
			}
			return ReadResult{Contact: contact, Set: set, Source: "inventory", ObservedAt: time.Now()}, nil
		}
		if options.Mode == ReadStaleIfError {
			if out, ok := cached(); ok {
				out.FallbackError = err.Error()
				return out, nil
			}
			break
		}
		if !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, causal.ErrDependency) {
			break
		}
		timer := time.NewTimer(min(5*time.Millisecond<<min(attempt, 5), 100*time.Millisecond))
		select {
		case <-readCtx.Done():
			timer.Stop()
			return ReadResult{}, readCtx.Err()
		case <-timer.C:
		}
	}
	if e := ctx.Err(); e != nil {
		return ReadResult{}, e
	}
	return ReadResult{}, err
}

func (o ReadOptions) validate() error {
	if o.Mode != ReadFresh && o.Mode != ReadCached && o.Mode != ReadCacheOnly && o.Mode != ReadStaleIfError {
		return fmt.Errorf("unknown read mode %q", o.Mode)
	}
	if o.Mode != ReadFresh && o.MaxAge <= 0 {
		return fmt.Errorf("cache read requires positive max age")
	}
	return nil
}
