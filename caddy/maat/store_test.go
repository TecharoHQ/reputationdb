package maat

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	fetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1/fetchv1connect"
	freefetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1"
	freefetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1/fetchv1connect"
	"github.com/klauspost/compress/zstd"
	"github.com/maxmind/mmdbwriter"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/durationpb"
)

// fakeFreeService serves whatever version and download URL the test sets,
// standing in for the free fetch service.
type fakeFreeService struct {
	version atomic.Value // string
	url     string
	calls   atomic.Int64
}

func (f *fakeFreeService) Fetch(ctx context.Context, req *connect.Request[freefetchv1.FetchRequest]) (*connect.Response[freefetchv1.FetchResponse], error) {
	f.calls.Add(1)
	return connect.NewResponse(&freefetchv1.FetchResponse{
		Lifetime:     durationpb.New(6 * time.Hour),
		Version:      f.version.Load().(string),
		PresignedUrl: f.url,
	}), nil
}

// fakeFullService stands in for the paid fetch service, which takes two calls
// to answer: List for the newest version, then Fetch for its download URL.
type fakeFullService struct {
	fetchv1connect.UnimplementedFetchServiceHandler

	version  atomic.Value // string
	url      string
	authSeen atomic.Value // string
}

func (f *fakeFullService) List(ctx context.Context, req *connect.Request[fetchv1.ListRequest]) (*connect.Response[fetchv1.ListResponse], error) {
	f.authSeen.Store(req.Header().Get("Authorization"))
	return connect.NewResponse(&fetchv1.ListResponse{
		Versions: []*fetchv1.DatabaseVersion{
			{VersionId: f.version.Load().(string)},
			{VersionId: "older-version"},
		},
	}), nil
}

func (f *fakeFullService) Fetch(ctx context.Context, req *connect.Request[fetchv1.FetchRequest]) (*connect.Response[fetchv1.FetchResponse], error) {
	f.authSeen.Store(req.Header().Get("Authorization"))
	if got, want := req.Msg.GetVersionId(), f.version.Load().(string); got != want {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("asked for version %q, newest is %q", got, want))
	}
	return connect.NewResponse(&fetchv1.FetchResponse{
		Version:      &fetchv1.DatabaseVersion{VersionId: req.Msg.GetVersionId()},
		PresignedUrl: f.url,
		Lifetime:     durationpb.New(6 * time.Hour),
	}), nil
}

// compressedTestDB returns a zstd-compressed database containing exactly one
// datacentre address.
func compressedTestDB(t *testing.T, cidr string) []byte {
	t.Helper()

	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "Techaro-Veil-Datacenter",
		IPVersion:    6,
		RecordSize:   28,
	})
	if err != nil {
		t.Fatalf("mmdbwriter.New: %v", err)
	}

	rec := reputationdb.Record{}
	rec.Add(reputationdb.ListMembership{
		Repository: "github.com/hexydec/ip-ranges",
		List:       "output/datacentres.txt",
		Provider:   "datacentres",
		Category:   reputationdb.CategoryDatacenter,
	})

	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	if err := tree.Insert(network, rec.DataType()); err != nil {
		t.Fatalf("Insert(%q): %v", cidr, err)
	}

	var raw bytes.Buffer
	if _, err := tree.WriteTo(&raw); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer enc.Close()

	return enc.EncodeAll(raw.Bytes(), nil)
}

// assetServer serves whatever database payload the test has stored, counting
// how many times it was downloaded.
func assetServer(t *testing.T, payload *atomic.Value, downloads *atomic.Int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(payload.Load().([]byte))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitReady blocks until the store has loaded a database. Loading happens in
// the background now, so every assertion about the loaded database has to wait
// for it first.
func waitReady(t *testing.T, s *dbStore) {
	t.Helper()
	select {
	case <-s.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the database to load")
	}
}

// has reports whether the store's current database contains addr.
func has(t *testing.T, s *dbStore, addr string) bool {
	t.Helper()
	_, found, loaded, err := s.lookup(mustAddr(t, addr))
	if err != nil {
		t.Fatalf("lookup(%s): %v", addr, err)
	}
	if !loaded {
		t.Fatalf("lookup(%s): no database loaded", addr)
	}
	return found
}

// TestStoreRefresh walks a free-tier store through a first load, a no-op
// refresh where the version is unchanged, and a refresh that swaps in
// different data.
func TestStoreRefresh(t *testing.T) {
	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	svc := &fakeFreeService{url: assetServer(t, &payload, &downloads)}
	svc.version.Store("v1")

	mux := http.NewServeMux()
	mux.Handle(freefetchv1connect.NewFetchServiceHandler(svc))
	api := httptest.NewServer(mux)
	defer api.Close()

	dir := t.TempDir()
	key := storeKey{server: api.URL, tier: tierFree}

	store, err := loadStore(key, "", dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	t.Cleanup(func() {
		if _, err := stores.Delete(key); err != nil {
			t.Errorf("tearing down store: %v", err)
		}
	})
	waitReady(t, store)

	if !has(t, store, "1.2.3.4") {
		t.Fatal("1.2.3.4 missing from the freshly loaded database")
	}
	if got := downloads.Load(); got != 1 {
		t.Fatalf("downloads = %d after first load, want 1", got)
	}

	firstPath := store.currentPath()
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("database was not written to disk: %v", err)
	}
	if filepath.Dir(firstPath) != dir {
		t.Errorf("database written to %s, want it under %s", firstPath, dir)
	}

	t.Run("unchanged version skips the download", func(t *testing.T) {
		before := svc.calls.Load()

		if _, err := store.refresh(); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := svc.calls.Load(); got != before+1 {
			t.Errorf("Fetch calls = %d, want %d: the version check itself must still happen", got, before+1)
		}
		if got := downloads.Load(); got != 1 {
			t.Errorf("downloads = %d, want 1: an unchanged version should not re-download", got)
		}
		if got := store.currentPath(); got != firstPath {
			t.Errorf("path = %s, want it unchanged at %s", got, firstPath)
		}
	})

	t.Run("new version swaps the database and retires the old file", func(t *testing.T) {
		payload.Store(compressedTestDB(t, "5.6.7.8/32"))
		svc.version.Store("v2")

		if _, err := store.refresh(); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := downloads.Load(); got != 2 {
			t.Fatalf("downloads = %d, want 2", got)
		}

		if !has(t, store, "5.6.7.8") {
			t.Error("5.6.7.8 missing after the swap")
		}
		if has(t, store, "1.2.3.4") {
			t.Error("1.2.3.4 still present, the old database is still being served")
		}

		newPath := store.currentPath()
		if newPath == firstPath {
			t.Fatal("the new build reused the old path, so versions aren't distinguished on disk")
		}
		if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
			t.Errorf("old database file still on disk at %s (stat err %v)", firstPath, err)
		}
	})
}

// TestStoreReusesCachedFile checks that a store starting up against a version
// it already has on disk maps the cached file instead of downloading it again.
func TestStoreReusesCachedFile(t *testing.T) {
	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	svc := &fakeFreeService{url: assetServer(t, &payload, &downloads)}
	svc.version.Store("v1")

	mux := http.NewServeMux()
	mux.Handle(freefetchv1connect.NewFetchServiceHandler(svc))
	api := httptest.NewServer(mux)
	defer api.Close()

	dir := t.TempDir()
	key := storeKey{server: api.URL, tier: tierFree}

	first, err := loadStore(key, "", dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore (first): %v", err)
	}
	waitReady(t, first)
	cached := first.currentPath()

	if _, err := stores.Delete(key); err != nil {
		t.Fatalf("tearing down the first store: %v", err)
	}
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("cached database was removed on teardown: %v", err)
	}

	second, err := loadStore(key, "", dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore (second): %v", err)
	}
	t.Cleanup(func() {
		if _, err := stores.Delete(key); err != nil {
			t.Errorf("tearing down the second store: %v", err)
		}
	})
	waitReady(t, second)

	if got := downloads.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1: the cached file should have been reused", got)
	}
	if got := second.currentPath(); got != cached {
		t.Errorf("path = %s, want the cached %s", got, cached)
	}
	if !has(t, second, "1.2.3.4") {
		t.Error("1.2.3.4 missing from the database loaded off disk")
	}
}

// TestStoreFullTier exercises the paid path: List then Fetch, with the API key
// attached.
func TestStoreFullTier(t *testing.T) {
	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	svc := &fakeFullService{url: assetServer(t, &payload, &downloads)}
	svc.version.Store("version-one")
	svc.authSeen.Store("")

	mux := http.NewServeMux()
	mux.Handle(fetchv1connect.NewFetchServiceHandler(svc))
	api := httptest.NewServer(mux)
	defer api.Close()

	key := storeKey{server: api.URL, tier: tierFull}
	store, err := loadStore(key, "hunter2", t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	t.Cleanup(func() {
		if _, err := stores.Delete(key); err != nil {
			t.Errorf("tearing down store: %v", err)
		}
	})
	waitReady(t, store)

	if got, want := svc.authSeen.Load().(string), "Bearer hunter2"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if !has(t, store, "1.2.3.4") {
		t.Error("1.2.3.4 missing from the full database")
	}
	if got := downloads.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1", got)
	}

	if _, err := store.refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := downloads.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1: an unchanged version should not re-download", got)
	}
}

// TestStoreShared checks that matchers wanting the same database share one
// store and one download, that the store survives until the last one goes
// away, and that the two tiers don't share.
func TestStoreShared(t *testing.T) {
	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	free := &fakeFreeService{url: assetServer(t, &payload, &downloads)}
	free.version.Store("v1")
	full := &fakeFullService{url: free.url}
	full.version.Store("v1")
	full.authSeen.Store("")

	mux := http.NewServeMux()
	mux.Handle(freefetchv1connect.NewFetchServiceHandler(free))
	mux.Handle(fetchv1connect.NewFetchServiceHandler(full))
	api := httptest.NewServer(mux)
	defer api.Close()

	dir := t.TempDir()
	key := storeKey{server: api.URL, tier: tierFree}

	first, err := loadStore(key, "", dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore (first): %v", err)
	}
	second, err := loadStore(key, "", dir, zap.NewNop())
	if err != nil {
		t.Fatalf("loadStore (second): %v", err)
	}
	waitReady(t, first)

	if first != second {
		t.Error("two matchers wanting the same database got different stores")
	}
	if got := downloads.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1: the store should be shared", got)
	}

	t.Run("a different tier gets its own store", func(t *testing.T) {
		fullKey := storeKey{server: api.URL, tier: tierFull}
		other, err := loadStore(fullKey, "hunter2", dir, zap.NewNop())
		if err != nil {
			t.Fatalf("loadStore (full): %v", err)
		}
		t.Cleanup(func() {
			if _, err := stores.Delete(fullKey); err != nil {
				t.Errorf("tearing down the full store: %v", err)
			}
		})
		waitReady(t, other)

		if other == first {
			t.Error("the free and full tiers shared a store")
		}
		if other.currentPath() == first.currentPath() {
			t.Error("the free and full tiers shared a file on disk")
		}
	})

	if _, err := stores.Delete(key); err != nil {
		t.Fatalf("releasing the first reference: %v", err)
	}
	if first.currentPath() == "" {
		t.Error("store was torn down while still referenced")
	}

	if _, err := stores.Delete(key); err != nil {
		t.Fatalf("releasing the last reference: %v", err)
	}
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Error("refresh goroutine did not stop after the last reference went away")
	}
}

func TestStoreDownload(t *testing.T) {
	compressed := compressedTestDB(t, "1.2.3.4/32")

	for _, tt := range []struct {
		name     string
		handler  http.HandlerFunc
		emptyURL bool
		wantErr  bool
	}{
		{
			name:    "well-formed database",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write(compressed) },
		},
		{
			name:     "empty url",
			emptyURL: true,
			wantErr:  true,
		},
		{
			name:    "not found",
			handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", http.StatusNotFound) },
			wantErr: true,
		},
		{
			name:    "body is not zstd",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("this is not a database")) },
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			url := ""
			if !tt.emptyURL {
				srv := httptest.NewServer(tt.handler)
				defer srv.Close()
				url = srv.URL
			}

			dir := t.TempDir()
			s := &dbStore{dir: dir, tier: tierFree, logger: zap.NewNop()}
			path := filepath.Join(dir, "test.mmdb")

			err := s.download(t.Context(), build{url: url}, path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				// A failed download must not leave a partial file behind for
				// a later run to mistake for a cached database.
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("failed download left %s behind (stat err %v)", path, statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("download: %v", err)
			}

			db, err := reputationdb.Open(path)
			if err != nil {
				t.Fatalf("downloaded file is not a usable database: %v", err)
			}
			db.Close()
		})
	}
}

func TestDatabasePath(t *testing.T) {
	dir := t.TempDir()
	free := &dbStore{dir: dir, tier: tierFree}
	full := &dbStore{dir: dir, tier: tierFull}

	v1 := free.databasePath("version-one")
	if got := free.databasePath("version-one"); got != v1 {
		t.Errorf("databasePath is not deterministic: %s then %s", v1, got)
	}
	if got := free.databasePath("version-two"); got == v1 {
		t.Error("two versions mapped to the same file")
	}
	if got := full.databasePath("version-one"); got == v1 {
		t.Error("the two tiers mapped the same version to the same file")
	}
	if filepath.Dir(v1) != dir {
		t.Errorf("databasePath = %s, want it under %s", v1, dir)
	}

	// A server that reports no version gets one fixed path rather than a
	// hash of the empty string, so successive builds overwrite it.
	if got := free.databasePath(""); got != filepath.Join(dir, "free.mmdb") {
		t.Errorf("databasePath(\"\") = %s, want %s", got, filepath.Join(dir, "free.mmdb"))
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return addr
}

func TestRefreshDelay(t *testing.T) {
	for _, tt := range []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		{name: "unset falls back to the default", lifetime: 0, want: defaultRefreshInterval},
		{name: "negative falls back to the default", lifetime: -time.Hour, want: defaultRefreshInterval},
		{name: "implausibly short is floored", lifetime: time.Second, want: minRefreshInterval},
		{name: "server value is honoured", lifetime: 6 * time.Hour, want: 6 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshDelay(tt.lifetime); got != tt.want {
				t.Errorf("refreshDelay(%v) = %v, want %v", tt.lifetime, got, tt.want)
			}
		})
	}
}
