package dbcache

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/TecharoHQ/reputationdb"
	"github.com/klauspost/compress/zstd"
)

const (
	// maxDatabaseSize caps the decompressed database, guarding against a
	// decompression bomb in the bucket.
	maxDatabaseSize = 4 << 30
)

const (
	// refreshInterval is how long to wait between checks for a newer build.
	// The database is rebuilt daily, so this is far more often than it needs
	// to be; a check reads one small gzipped index object, and picking up a new
	// build promptly is worth more than the round trips cost.
	refreshInterval = 15 * time.Minute
	// retryInterval is how long to wait after a failed refresh.
	retryInterval = time.Minute

	// fetchTimeout bounds a single refresh: the index read plus the download.
	// The full database is on the order of 800 MiB, so this is generous.
	fetchTimeout = 30 * time.Minute
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

	// refreshMu serializes Refresh. It is exported behaviour, so two callers
	// could otherwise race two downloads of the same build into place.
	refreshMu sync.Mutex

	// ready is closed once a database has been loaded for the first time.
	// Nothing waits on it in production — the point of loading in the
	// background is that nobody has to — but it gives tests and future
	// readiness probes a signal to wait on instead of polling.
	ready     chan struct{}
	readyOnce sync.Once

	// mu guards the fields below and is held for the duration of a query, so
	// that a refresh can close the old mapping as soon as it swaps: taking the
	// write lock means every in-flight query has finished.
	mu        sync.RWMutex
	db        *reputationdb.DB
	path      string
	version   string
	createdAt time.Time
}

// New returns a cache that refreshes itself in the background until ctx is
// cancelled or Close is called.
//
// Those two are not interchangeable for teardown: cancelling ctx stops the
// refresh loop, but only Close releases the memory mapping. A caller that
// wants the ~800 MiB mapping back before the process exits must call Close.
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

	c := &Cache{
		src:    src,
		dir:    dir,
		lg:     lg.With("component", "dbcache"),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}

	go c.run()

	return c, nil
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

// databasePath returns where a build of the given version belongs on disk.
//
// The version is hashed rather than used directly because it comes from the
// bucket, and an 86-character base64 filename is unwieldy besides. Hashing
// keeps the name short and keeps remote data out of the path.
func (c *Cache) databasePath(version string) string {
	sum := sha256.Sum256([]byte(version))
	return filepath.Join(c.dir, fmt.Sprintf("reputationdb-%s.mmdb", base64.RawURLEncoding.EncodeToString(sum[:16])))
}

// Close stops the refresh goroutine and releases the mapping.
//
// The database file is deliberately left on disk: it is the cache that lets the
// next start skip the download.
func (c *Cache) Close() error {
	c.cancel()
	<-c.done

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.db == nil {
		return nil
	}
	db := c.db
	c.db, c.path, c.version = nil, "", ""
	return db.Close()
}

// Refresh asks the source for the newest build and loads it if it differs from
// what's already mapped.
//
// It is safe to call concurrently, but calls are serialized: two downloads of
// the same build racing each other into place would be pure waste.
func (c *Cache) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	b, err := c.src.Current(ctx)
	if err != nil {
		return err
	}

	c.mu.RLock()
	current := c.version
	c.mu.RUnlock()

	// Source.Current guarantees a non-empty version ID, so this can't mistake
	// "nothing loaded" for "already current".
	if b.VersionID == current {
		c.lg.DebugContext(ctx, "the reputation database is already current", "version_id", b.VersionID)
		return nil
	}

	path := c.databasePath(b.VersionID)

	// A build we already have on disk from a previous run needs no download.
	// Version IDs are content-addressed, so a matching filename means matching
	// data.
	if _, err := os.Stat(path); err == nil {
		if err := c.load(path, b, true); err == nil {
			return nil
		}
		// A cached file that won't open is a truncated or corrupt download from
		// a previous run. Fall through and fetch it again.
		c.lg.WarnContext(ctx, "the cached database is unusable, downloading it again", "path", path)
	}

	if err := c.download(ctx, b, path); err != nil {
		return err
	}
	if err := c.load(path, b, false); err != nil {
		// The bytes are on disk but unusable. Remove them rather than leave a
		// file the next start would try to map as a cached database.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			c.lg.WarnContext(ctx, "can't remove the unusable database", "path", path, "err", rmErr)
		}
		return err
	}
	return nil
}

// download copies the database at b.Key out of the source and decompresses it
// to path, staging through a temporary file so that path is only ever a
// complete database.
//
// Unlike caddy/maat there is no "unmap before renaming" dance here: paths are
// content-addressed and Refresh returns early when the version is unchanged, so
// the destination is never the file currently mapped.
func (c *Cache) download(ctx context.Context, b Build, path string) error {
	body, err := c.src.Open(ctx, b.Key)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(c.dir, "reputationdb-*.mmdb.tmp")
	if err != nil {
		return fmt.Errorf("can't create a temporary database file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	dec, err := zstd.NewReader(body)
	if err != nil {
		return fmt.Errorf("can't create a zstd decoder: %w", err)
	}
	defer dec.Close()

	// Copy one byte past the cap so a database that hits the limit exactly is
	// reported as too large instead of silently truncated into a corrupt mmdb.
	written, err := io.Copy(tmp, io.LimitReader(dec, maxDatabaseSize+1))
	if err != nil {
		return fmt.Errorf("can't decompress the database to %s: %w", tmp.Name(), err)
	}
	if written > maxDatabaseSize {
		return fmt.Errorf("the database is larger than the %d byte limit", int64(maxDatabaseSize))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("can't flush the database to %s: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("can't move the database into place at %s: %w", path, err)
	}

	c.lg.DebugContext(ctx, "downloaded the reputation database", "path", path, "bytes", written)

	return nil
}

// load maps the database at path and swaps it in, releasing whatever was mapped
// before. cached reports whether path came from a previous run rather than a
// download just now, and only affects logging.
func (c *Cache) load(path string, b Build, cached bool) error {
	db, err := reputationdb.Open(path)
	if err != nil {
		return fmt.Errorf("can't open the database at %s: %w", path, err)
	}

	metadata := db.Metadata()

	// The index says when the build was published. An entry written before
	// created_at existed doesn't, so fall back to the build epoch baked into
	// the mmdb: a query response should always carry some idea of how stale
	// its answer is.
	createdAt := b.CreatedAt
	if createdAt.IsZero() {
		createdAt = metadata.BuildTime()
	}

	c.mu.Lock()
	old, oldPath := c.db, c.path
	c.db, c.path, c.version, c.createdAt = db, path, b.VersionID, createdAt
	c.mu.Unlock()

	// Past this point nothing can reach the old database: taking the write lock
	// above waited out every in-flight query, and new ones see the replacement.
	// Closing it now releases the mapping instead of leaking it.
	c.release(old, oldPath, path)

	// release only ever retires the file this process had mapped, which is
	// nothing on a process's first load. Sweeping the whole directory here
	// instead catches whatever an earlier process left behind too, which is
	// the case that actually matters: the cache directory is meant to survive
	// restarts, and a restart that lands on a new build is exactly when a
	// previous run's ~800 MiB file would otherwise sit there forever.
	c.sweep(path)

	c.readyOnce.Do(func() { close(c.ready) })

	c.lg.Info("loaded the reputation database",
		"version_id", b.VersionID,
		"path", path,
		"cached", cached,
		"database_type", metadata.DatabaseType,
		"node_count", metadata.NodeCount,
		"created_at", createdAt)

	return nil
}

// release closes a retired database and deletes the file behind it, unless that
// file is the one now in use.
func (c *Cache) release(db *reputationdb.DB, path, keep string) {
	if db == nil {
		return
	}
	if err := db.Close(); err != nil {
		c.lg.Warn("can't close the retired database", "path", path, "err", err)
	}
	if path == "" || path == keep {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.lg.Warn("can't remove the retired database", "path", path, "err", err)
	}
}

// sweep removes every database file this package owns other than keep.
//
// The glob only matches complete databases (reputationdb-<hash>.mmdb), never
// the reputationdb-*.mmdb.tmp staging files download creates: a name ending
// in .mmdb.tmp does not satisfy a pattern that requires the string to end in
// .mmdb, so a download in progress is never touched. Anything else in the
// directory — an operator's own files on a shared volume, this package's own
// sentinel files in tests — is left alone because it doesn't match the glob
// at all.
//
// Failures are logged and swallowed rather than returned: a database that
// just loaded successfully must keep serving even if cleanup is denied by
// permissions, and a restart will get another chance to sweep.
func (c *Cache) sweep(keep string) {
	matches, err := filepath.Glob(filepath.Join(c.dir, "reputationdb-*.mmdb"))
	if err != nil {
		c.lg.Warn("can't list stale databases to sweep", "dir", c.dir, "err", err)
		return
	}
	for _, path := range matches {
		if path == keep {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.lg.Warn("can't remove a stale database", "path", path, "err", err)
		}
	}
}

// loaded reports whether a database is currently mapped.
func (c *Cache) loaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.db != nil
}

// run loads the database and then keeps it fresh until the cache is closed. The
// first attempt happens immediately, which is what makes the database show up
// shortly after the server starts rather than before it.
func (c *Cache) run() {
	defer close(c.done)

	for {
		delay := refreshInterval

		ctx, cancel := context.WithTimeout(c.ctx, fetchTimeout)
		err := c.Refresh(ctx)
		cancel()

		if err != nil {
			// Retry sooner than the normal cadence either way, but say which
			// situation this is: with a database already mapped this is a
			// stale-data warning, and without one it means queries are failing.
			if c.loaded() {
				c.lg.Error("can't refresh the reputation database, continuing with the one already loaded",
					"retry_in", retryInterval, "err", err)
			} else {
				c.lg.Error("can't load the reputation database, queries will fail until this succeeds",
					"retry_in", retryInterval, "err", err)
			}
			delay = retryInterval
		}

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}
