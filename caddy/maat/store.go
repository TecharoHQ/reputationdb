package maat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/TecharoHQ/reputationdb"
	"github.com/caddyserver/caddy/v2"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

const (
	// defaultRefreshInterval is how long to wait between checks when the
	// server doesn't say.
	defaultRefreshInterval = 6 * time.Hour
	// minRefreshInterval floors what the server can ask for, so a bad
	// lifetime can't turn every Caddy instance into a polling loop.
	minRefreshInterval = 5 * time.Minute
	// retryInterval is how long to wait after a failed refresh.
	retryInterval = 5 * time.Minute

	// fetchTimeout bounds a single refresh: the metadata calls plus the
	// download. The full database is on the order of 800 MiB, so this is
	// generous.
	fetchTimeout = 30 * time.Minute
	// maxDatabaseSize caps the decompressed database, guarding against a
	// decompression bomb at the other end of the download URL.
	maxDatabaseSize = 4 << 30
)

// stores holds one dbStore per (server, tier), shared across every matcher
// that wants it and reference-counted so the last one out does the teardown.
var stores = caddy.NewUsagePool()

// storeKey identifies a shared store. Two matchers that want the same database
// from the same server share one download and one refresh goroutine, even if
// they match on different categories.
type storeKey struct {
	server string
	tier   tier
}

// dbStore owns one copy of a database on disk, the memory mapping over it, and
// the goroutine that keeps it fresh.
//
// The database is written to disk and mapped rather than held in memory
// because the full build is around 800 MiB: paging it in on demand costs one
// mapping instead of one heap allocation the size of the file, and it survives
// a Caddy restart without being downloaded again.
type dbStore struct {
	src    databaseSource
	dir    string
	tier   tier
	logger *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// ready is closed once a database has been loaded for the first time.
	// Nothing waits on it in production — the point of loading in the
	// background is that nobody has to — but it gives tests a signal to wait
	// on instead of polling.
	ready     chan struct{}
	readyOnce sync.Once

	// mu guards the fields below and is held for the duration of a lookup, so
	// that a refresh can close the old mapping as soon as it swaps: taking the
	// write lock means every in-flight lookup has finished.
	mu      sync.RWMutex
	db      *reputationdb.DB
	path    string
	version string
}

// loadStore returns the store for key, creating it if this is the first
// matcher to ask for it.
//
// It does not wait for the database: the full build is around 800 MiB, and
// holding up Caddy's config load for that long — or refusing the config
// outright because the download failed — is worse than matching nothing for
// the first few minutes. Until a database lands, lookups against this store
// simply don't match.
func loadStore(key storeKey, apiKey, dir string, logger *zap.Logger) (*dbStore, error) {
	val, _, err := stores.LoadOrNew(key, func() (caddy.Destructor, error) {
		// The directory is the one part worth failing the config over: it's a
		// local operation, and a path Caddy can't write to is a typo rather
		// than a transient failure.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("can't create database directory %s: %w", dir, err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		s := &dbStore{
			src:    newSource(key.tier, key.server, apiKey),
			dir:    dir,
			tier:   key.tier,
			logger: logger.With(zap.String("server", key.server), zap.String("tier", string(key.tier))),
			ctx:    ctx,
			cancel: cancel,
			done:   make(chan struct{}),
			ready:  make(chan struct{}),
		}

		go s.run()

		return s, nil
	})
	if err != nil {
		return nil, err
	}
	return val.(*dbStore), nil
}

// lookup runs addr against the currently loaded database. It reports
// loaded=false when there is no database to consult yet.
//
// The read lock is held across the lookup so that a concurrent refresh can't
// unmap the database out from under it.
func (s *dbStore) lookup(addr netip.Addr) (result reputationdb.Result, found, loaded bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.db == nil {
		return reputationdb.Result{}, false, false, nil
	}

	result, found, err = s.db.Lookup(addr)
	return result, found, true, err
}

// run loads the database and then keeps it fresh forever. The first attempt
// happens immediately, which is what makes the database show up shortly after
// Caddy starts rather than before it.
func (s *dbStore) run() {
	defer close(s.done)

	for {
		lifetime, err := s.refresh()
		delay := refreshDelay(lifetime)

		if err != nil {
			// Retry sooner than the normal cadence either way, but say which
			// situation this is: with a database already mapped this is a
			// stale-data warning, and without one it means nothing matches.
			if s.loaded() {
				s.logger.Error("can't refresh reputation database, continuing with the one already loaded",
					zap.Duration("retry_in", retryInterval),
					zap.Error(err))
			} else {
				s.logger.Error("can't load reputation database, nothing will match until this succeeds",
					zap.Duration("retry_in", retryInterval),
					zap.Error(err))
			}
			delay = retryInterval
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// loaded reports whether a database is currently mapped.
func (s *dbStore) loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db != nil
}

// refresh asks the server for the current build and loads it if it differs
// from what's already mapped. It returns the lifetime the server reported,
// which is valid even when the database itself was unchanged.
func (s *dbStore) refresh() (time.Duration, error) {
	ctx, cancel := context.WithTimeout(s.ctx, fetchTimeout)
	defer cancel()

	b, err := s.src.current(ctx)
	if err != nil {
		return 0, err
	}

	s.mu.RLock()
	current := s.version
	s.mu.RUnlock()

	if b.version != "" && b.version == current {
		s.logger.Debug("reputation database is already current",
			zap.String("version", b.version))
		return b.lifetime, nil
	}

	path := s.databasePath(b.version)

	// A build we already have on disk from a previous run needs no download.
	// Both tiers version by content, so a matching filename means matching
	// data.
	if b.version != "" {
		if _, err := os.Stat(path); err == nil {
			if err := s.load(path, b, true); err == nil {
				return b.lifetime, nil
			}
			// A cached file that won't open is a truncated or corrupt
			// download from a previous run. Fall through and fetch it again.
			s.logger.Warn("cached database is unusable, downloading it again",
				zap.String("path", path))
		}
	}

	if err := s.download(ctx, b, path); err != nil {
		return b.lifetime, err
	}
	if err := s.load(path, b, false); err != nil {
		return b.lifetime, err
	}

	return b.lifetime, nil
}

// download fetches the compressed database at b.url and decompresses it to
// path, staging through a temporary file so that path is only ever a complete
// database.
func (s *dbStore) download(ctx context.Context, b build, path string) error {
	if b.url == "" {
		return errors.New("server returned an empty download URL")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return fmt.Errorf("can't build database download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't fetch release database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("can't fetch release database: unexpected status %s", resp.Status)
	}

	tmp, err := os.CreateTemp(s.dir, string(s.tier)+"-*.mmdb.tmp")
	if err != nil {
		return fmt.Errorf("can't create temporary database file: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	dec, err := zstd.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("can't create zstd decoder: %w", err)
	}
	defer dec.Close()

	// Copy one byte past the cap so a database that hits the limit exactly is
	// reported as too large instead of silently truncated into a corrupt mmdb.
	written, err := io.Copy(tmp, io.LimitReader(dec, maxDatabaseSize+1))
	if err != nil {
		return fmt.Errorf("can't decompress database to %s: %w", tmp.Name(), err)
	}
	if written > maxDatabaseSize {
		return fmt.Errorf("database is larger than the %d byte limit", int64(maxDatabaseSize))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("can't flush database to %s: %w", tmp.Name(), err)
	}

	// Renaming onto a mapped file fails on Windows, so give up the mapping
	// first when the new build lands on the path we're already serving. That
	// only happens when the server reports no version at all.
	if path == s.currentPath() {
		s.unload()
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("can't move database into place at %s: %w", path, err)
	}

	s.logger.Debug("downloaded reputation database",
		zap.String("path", path),
		zap.Int64("bytes", written))

	return nil
}

// load maps the database at path and swaps it in, releasing whatever was
// mapped before. cached reports whether path came from a previous run rather
// than a download just now, and only affects logging.
func (s *dbStore) load(path string, b build, cached bool) error {
	db, err := reputationdb.Open(path)
	if err != nil {
		return fmt.Errorf("can't open database at %s: %w", path, err)
	}

	s.mu.Lock()
	old, oldPath := s.db, s.path
	s.db, s.path, s.version = db, path, b.version
	s.mu.Unlock()

	// Past this point nothing can reach the old database: taking the write
	// lock above waited out every in-flight lookup, and new ones see the
	// replacement. Closing it now releases the mapping instead of leaking it.
	s.release(old, oldPath, path)

	s.readyOnce.Do(func() { close(s.ready) })

	metadata := db.Metadata()
	s.logger.Info("loaded reputation database",
		zap.String("version", b.version),
		zap.String("path", path),
		zap.Bool("cached", cached),
		zap.String("database_type", metadata.DatabaseType),
		zap.Uint("node_count", metadata.NodeCount),
		zap.Time("published_at", b.createdAt))

	return nil
}

// unload drops the current database, leaving the store with nothing to serve
// until the next load.
func (s *dbStore) unload() {
	s.mu.Lock()
	old, oldPath := s.db, s.path
	s.db, s.path, s.version = nil, "", ""
	s.mu.Unlock()

	s.release(old, oldPath, "")
}

// release closes a retired database and deletes the file behind it, unless
// that file is the one now in use.
func (s *dbStore) release(db *reputationdb.DB, path, keep string) {
	if db == nil {
		return
	}
	if err := db.Close(); err != nil {
		s.logger.Warn("can't close retired database", zap.String("path", path), zap.Error(err))
	}
	if path == "" || path == keep {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logger.Warn("can't remove retired database", zap.String("path", path), zap.Error(err))
	}
}

// currentPath returns the file currently mapped, or "" if there isn't one.
func (s *dbStore) currentPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// databasePath returns where a build of the given version belongs on disk.
//
// The version is hashed rather than used directly because it is server-chosen
// and can contain anything; hashing keeps the filename short and safe. A
// server that reports no version gets one fixed path that later builds
// overwrite.
func (s *dbStore) databasePath(version string) string {
	if version == "" {
		return filepath.Join(s.dir, string(s.tier)+".mmdb")
	}
	sum := sha256.Sum256([]byte(version))
	return filepath.Join(s.dir, fmt.Sprintf("%s-%s.mmdb", s.tier, base64.RawURLEncoding.EncodeToString(sum[:16])))
}

// Destruct implements caddy.Destructor. It is called by the usage pool once
// the last matcher referencing this store has been cleaned up.
//
// The database file is deliberately left on disk: it is the cache that lets
// the next start skip the download.
func (s *dbStore) Destruct() error {
	s.cancel()
	<-s.done

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	db := s.db
	s.db, s.path, s.version = nil, "", ""
	return db.Close()
}

// refreshDelay turns a server-supplied lifetime into a wait, applying the
// default when it's unset and the floor when it's implausibly short.
func refreshDelay(lifetime time.Duration) time.Duration {
	if lifetime <= 0 {
		return defaultRefreshInterval
	}
	return max(lifetime, minRefreshInterval)
}

// Interface guards
var (
	_ caddy.Destructor = (*dbStore)(nil)
)
