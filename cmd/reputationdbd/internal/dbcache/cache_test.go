package dbcache_test

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache/dbcachetest"
	"github.com/klauspost/compress/zstd"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return addr
}

// Nothing has been loaded yet, so the cache must say so rather than reporting
// every address as absent from the database.
func TestCacheQueryBeforeAnythingIsLoaded(t *testing.T) {
	c, err := dbcache.New(t.Context(), discardLogger(), dbcachetest.New(), t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	matches, createdAt, loaded, err := c.Query([]netip.Addr{mustAddr(t, "1.2.3.4")})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if loaded {
		t.Error("Query() loaded = true before any database was loaded, want false")
	}
	if matches != nil {
		t.Errorf("Query() matches = %v, want nil", matches)
	}
	if !createdAt.IsZero() {
		t.Errorf("Query() created_at = %v, want the zero time", createdAt)
	}
}

func TestCacheCreatesItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")

	c, err := dbcache.New(t.Context(), discardLogger(), dbcachetest.New(), dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("New() did not create %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
}

func TestCacheCloseWithNothingLoaded(t *testing.T) {
	c, err := dbcache.New(t.Context(), discardLogger(), dbcachetest.New(), t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil when nothing was ever loaded", err)
	}
}

func TestCachePathIsEmptyBeforeAnythingIsLoaded(t *testing.T) {
	c, err := dbcache.New(t.Context(), discardLogger(), dbcachetest.New(), t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	if got := c.Path(); got != "" {
		t.Errorf("Path() = %q, want \"\" before any database was loaded", got)
	}
}

// publishedAt is a fixed timestamp for tests that assert on the build date.
var publishedAt = time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC)

// newCache returns a cache over src rooted at dir.
func newCache(t *testing.T, src dbcache.Source, dir string) *dbcache.Cache {
	t.Helper()

	c, err := dbcache.New(t.Context(), discardLogger(), src, dir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return c
}

// publish stores a compressed database of the given CIDRs as versionID.
func publish(t *testing.T, src *dbcachetest.Fake, versionID string, cidrs ...string) {
	t.Helper()

	compressed, err := dbcachetest.CompressedDatabase(cidrs...)
	if err != nil {
		t.Fatalf("CompressedDatabase(%v): %v", cidrs, err)
	}
	src.Publish(versionID, publishedAt, compressed)
}

// has reports whether the cache's current database contains addr.
func has(t *testing.T, c *dbcache.Cache, addr string) bool {
	t.Helper()

	matches, _, loaded, err := c.Query([]netip.Addr{mustAddr(t, addr)})
	if err != nil {
		t.Fatalf("Query(%s): %v", addr, err)
	}
	if !loaded {
		t.Fatalf("Query(%s): no database loaded", addr)
	}
	return len(matches) == 1
}

// TestCacheRefresh walks a cache through a first load, a no-op refresh where
// the version is unchanged, and a refresh that swaps in different data.
func TestCacheRefresh(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	dir := t.TempDir()
	c := newCache(t, src, dir)

	waitReady(t, c)
	if !has(t, c, "1.2.3.4") {
		t.Fatal("1.2.3.4 missing from the freshly loaded database")
	}
	if got := src.Opens(); got != 1 {
		t.Fatalf("downloads = %d after the first load, want 1", got)
	}

	firstPath := c.Path()
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("the database was not written to disk: %v", err)
	}
	if filepath.Dir(firstPath) != dir {
		t.Errorf("the database was written to %s, want it under %s", firstPath, dir)
	}

	t.Run("the build date comes from the index", func(t *testing.T) {
		_, createdAt, loaded, err := c.Query([]netip.Addr{mustAddr(t, "1.2.3.4")})
		if err != nil || !loaded {
			t.Fatalf("Query() error = %v, loaded = %v", err, loaded)
		}
		if !createdAt.Equal(publishedAt) {
			t.Errorf("Query() created_at = %v, want %v", createdAt, publishedAt)
		}
	})

	t.Run("an unchanged version skips the download", func(t *testing.T) {
		before := src.Currents()

		if err := c.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh() error = %v", err)
		}
		if got := src.Currents(); got != before+1 {
			t.Errorf("Current calls = %d, want %d: the version check itself must still happen", got, before+1)
		}
		if got := src.Opens(); got != 1 {
			t.Errorf("downloads = %d, want 1: an unchanged version should not re-download", got)
		}
		if got := c.Path(); got != firstPath {
			t.Errorf("Path() = %s, want it unchanged at %s", got, firstPath)
		}
	})

	t.Run("a new version swaps the database and retires the old file", func(t *testing.T) {
		publish(t, src, "v2", "5.6.7.8/32")

		if err := c.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh() error = %v", err)
		}
		if got := src.Opens(); got != 2 {
			t.Fatalf("downloads = %d, want 2", got)
		}

		if !has(t, c, "5.6.7.8") {
			t.Error("5.6.7.8 missing after the swap")
		}
		if has(t, c, "1.2.3.4") {
			t.Error("1.2.3.4 is still present, so the old database is still being served")
		}

		newPath := c.Path()
		if newPath == firstPath {
			t.Fatal("the new build reused the old path, so versions aren't distinguished on disk")
		}
		if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
			t.Errorf("the old database file is still on disk at %s (stat err %v)", firstPath, err)
		}
	})
}

// A cache starting up against a version it already has on disk must map the
// cached file instead of downloading it again.
func TestCacheReusesTheCachedFile(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	dir := t.TempDir()

	first := newCache(t, src, dir)
	waitReady(t, first)
	cached := first.Path()
	if err := first.Close(); err != nil {
		t.Fatalf("Close() (first) error = %v", err)
	}
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("the cached database was removed on teardown: %v", err)
	}

	second := newCache(t, src, dir)
	waitReady(t, second)

	if got := src.Opens(); got != 1 {
		t.Errorf("downloads = %d, want 1: the cached file should have been reused", got)
	}
	if got := second.Path(); got != cached {
		t.Errorf("Path() = %s, want the cached %s", got, cached)
	}
	if !has(t, second, "1.2.3.4") {
		t.Error("1.2.3.4 missing from the database loaded off disk")
	}
}

// A restart that lands on a new build must not leave the previous run's file
// behind: old is nil on a process's first load, so release alone never sweeps
// it, and nothing else in the package reads the cache directory.
func TestCacheSweepsStaleDatabaseFilesOnLoad(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	dir := t.TempDir()

	stale := filepath.Join(dir, "reputationdb-stale.mmdb")
	if err := os.WriteFile(stale, []byte("a previous run's build"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", stale, err)
	}
	// A file this package didn't create must survive the sweep: an operator
	// may have mounted a shared volume, and a stray "reputationdb-*.mmdb"
	// match is the only thing sweep is allowed to touch.
	unrelated := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(unrelated, nil, 0o600); err != nil {
		t.Fatalf("seeding %s: %v", unrelated, err)
	}

	c := newCache(t, src, dir)
	waitReady(t, c)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale database %s is still on disk (stat err %v), want it swept", stale, err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("the unrelated file %s was removed by the sweep: %v", unrelated, err)
	}
	if _, err := os.Stat(c.Path()); err != nil {
		t.Errorf("the newly loaded database %s is missing from disk: %v", c.Path(), err)
	}
}

// An index entry with no created_at falls back to the build epoch baked into
// the mmdb, so a response always carries some idea of how stale it is.
func TestCacheFallsBackToTheMmdbBuildTime(t *testing.T) {
	compressed, err := dbcachetest.CompressedDatabase("1.2.3.4/32")
	if err != nil {
		t.Fatalf("CompressedDatabase: %v", err)
	}

	src := dbcachetest.New()
	src.Publish("v1", time.Time{}, compressed)

	c := newCache(t, src, t.TempDir())
	waitReady(t, c)

	_, createdAt, loaded, err := c.Query([]netip.Addr{mustAddr(t, "1.2.3.4")})
	if err != nil || !loaded {
		t.Fatalf("Query() error = %v, loaded = %v", err, loaded)
	}
	if createdAt.IsZero() {
		t.Error("Query() created_at is the zero time, want the mmdb's build epoch")
	}
}

// countCacheFiles returns how many files of any kind — finished databases and
// partial .tmp ones alike — are sitting in dir.
func countCacheFiles(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	return len(entries)
}

func TestCacheRefreshFailures(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, src *dbcachetest.Fake)
	}{
		{
			name:  "nothing has been published",
			setup: func(t *testing.T, src *dbcachetest.Fake) {},
		},
		{
			name: "the index can't be read",
			setup: func(t *testing.T, src *dbcachetest.Fake) {
				publish(t, src, "v1", "1.2.3.4/32")
				src.SetCurrentErr(errors.New("network is on fire"))
			},
		},
		{
			name: "the object can't be read",
			setup: func(t *testing.T, src *dbcachetest.Fake) {
				publish(t, src, "v1", "1.2.3.4/32")
				src.SetOpenErr(errors.New("no credentials"))
			},
		},
		{
			name: "the object is not zstd",
			setup: func(t *testing.T, src *dbcachetest.Fake) {
				src.Publish("v1", publishedAt, []byte("this is not a database"))
			},
		},
		{
			name: "the object is zstd but not an mmdb",
			setup: func(t *testing.T, src *dbcachetest.Fake) {
				enc, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatalf("zstd.NewWriter: %v", err)
				}
				defer enc.Close()
				src.Publish("v1", publishedAt, enc.EncodeAll([]byte("not an mmdb"), nil))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := dbcachetest.New()
			tt.setup(t, src)

			dir := t.TempDir()

			// A file Refresh has no business touching. Seeding it makes the
			// residue assertion below meaningful even on failure paths that
			// bail out before anything could be written: the count can then
			// actually fall (cleanup overreached) or rise (residue), instead
			// of being a foregone 0 == 0.
			sentinel := filepath.Join(dir, "sentinel")
			if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
				t.Fatalf("seeding %s: %v", sentinel, err)
			}

			c := newCache(t, src, dir)

			if err := c.Refresh(t.Context()); err == nil {
				t.Fatal("Refresh() error = nil, want a failure")
			}

			// Nothing may be mapped, and no partial file may survive for a
			// later run to mistake for a cached database.
			if _, _, loaded, _ := c.Query(nil); loaded {
				t.Error("Query() loaded = true after a failed refresh, want false")
			}
			if got := countCacheFiles(t, dir); got != 1 {
				t.Errorf("the cache directory holds %d files after a failed refresh, want 1 (the sentinel alone)", got)
			}
		})
	}
}

// A refresh that fails while a database is already mapped must keep serving the
// one it has rather than dropping it.
func TestCacheKeepsServingAfterAFailedRefresh(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	c := newCache(t, src, t.TempDir())
	waitReady(t, c)
	loadedPath := c.Path()

	publish(t, src, "v2", "5.6.7.8/32")
	src.SetOpenErr(errors.New("no credentials"))

	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("Refresh() error = nil, want a failure")
	}
	if got := c.Path(); got != loadedPath {
		t.Errorf("Path() = %s after a failed refresh, want the previously loaded %s", got, loadedPath)
	}
	if !has(t, c, "1.2.3.4") {
		t.Error("1.2.3.4 missing after a failed refresh: the working database was dropped")
	}
}

// waitReady blocks until the cache has loaded a database. Loading happens in
// the background, so anything asserting on a loaded database has to wait first.
func waitReady(t *testing.T, c *dbcache.Cache) {
	t.Helper()

	select {
	case <-c.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the database to load")
	}
}

// Nobody calls Refresh in production; the loop does. This is the test that the
// loop exists at all.
func TestCacheLoadsInTheBackground(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	c := newCache(t, src, t.TempDir())
	waitReady(t, c)

	if !has(t, c, "1.2.3.4") {
		t.Error("1.2.3.4 missing from the database the loop loaded")
	}
}

func TestCacheCloseStopsTheLoop(t *testing.T) {
	src := dbcachetest.New()
	publish(t, src, "v1", "1.2.3.4/32")

	c, err := dbcache.New(t.Context(), discardLogger(), src, t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	waitReady(t, c)

	// Close returns only once the loop has stopped, so a Close that returns at
	// all is the assertion here; the deadline catches a loop that never exits.
	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close() error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close() did not return: the refresh loop is still running")
	}
}
