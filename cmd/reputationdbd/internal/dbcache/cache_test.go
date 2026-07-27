package dbcache_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache/dbcachetest"
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
