package dbcache

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/TecharoHQ/reputationdb"
)

// Match is one address that was found in the database.
type Match struct {
	// Addr is the address that was looked up.
	Addr netip.Addr
	// Result is the record stored for it.
	Result reputationdb.Result
}

// Cache owns one copy of the database on disk, the memory mapping over it, and
// the goroutine that keeps it fresh.
//
// The database is written to disk and mapped rather than held on the heap
// because the full build is around 800 MiB: mapping it lets the kernel page in
// only what queries actually touch, and it survives a restart without being
// downloaded again.
type Cache struct {
	src Source
	dir string
	lg  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// ready is closed once a database has been loaded for the first time.
	// Nothing waits on it in production — the point of loading in the
	// background is that nobody has to — but it gives tests and future
	// readiness probes a signal to wait on instead of polling.
	ready chan struct{}

	// mu guards the fields below and is held for the duration of a query, so
	// that a refresh can close the old mapping as soon as it swaps: taking the
	// write lock means every in-flight query has finished.
	mu        sync.RWMutex
	db        *reputationdb.DB
	path      string
	version   string
	createdAt time.Time
}

// New returns a cache that keeps itself fresh until ctx is cancelled or Close
// is called.
//
// It does not wait for the database: the full build is around 800 MiB, and
// holding the whole server up for that long — or refusing to start because the
// bucket was briefly unreachable — is worse than answering queries with
// Unavailable for the first few minutes.
func New(ctx context.Context, lg *slog.Logger, src Source, dir string) (*Cache, error) {
	// The directory is the one part worth failing startup over: it's a local
	// operation, and a path the server can't write to is a misconfiguration
	// rather than a transient failure.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("can't create the database cache directory %s: %w", dir, err)
	}

	ctx, cancel := context.WithCancel(ctx)

	return &Cache{
		src:    src,
		dir:    dir,
		lg:     lg.With("component", "dbcache"),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}, nil
}

// Ready returns a channel that is closed once a database has been loaded for
// the first time.
func (c *Cache) Ready() <-chan struct{} { return c.ready }

// Path returns the database file currently mapped, or "" if there isn't one.
func (c *Cache) Path() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// Query looks every address in addrs up against one consistent snapshot of the
// database, returning a Match for each address that has a record and skipping
// the ones that don't.
//
// It reports loaded=false when no database has been mapped yet; callers must
// treat that as "I don't know", never as "none of these are listed". createdAt
// is when the mapped build was published.
//
// The read lock is held across the whole batch so that a concurrent refresh
// can't unmap the database mid-query, and so that every record in one response
// came from the same build. Decoded results are copies rather than views into
// the mapping, so they stay valid after the lock is dropped.
func (c *Cache) Query(addrs []netip.Addr) (matches []Match, createdAt time.Time, loaded bool, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.db == nil {
		return nil, time.Time{}, false, nil
	}

	for _, addr := range addrs {
		result, found, err := c.db.Lookup(addr)
		if err != nil {
			return nil, c.createdAt, true, fmt.Errorf("can't look %s up: %w", addr, err)
		}
		if !found {
			continue
		}
		matches = append(matches, Match{Addr: addr, Result: result})
	}

	return matches, c.createdAt, true, nil
}

// Close releases the mapping.
//
// The database file is deliberately left on disk: it is the cache that lets the
// next start skip the download.
func (c *Cache) Close() error {
	c.cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.db == nil {
		return nil
	}
	db := c.db
	c.db, c.path, c.version = nil, "", ""
	return db.Close()
}
