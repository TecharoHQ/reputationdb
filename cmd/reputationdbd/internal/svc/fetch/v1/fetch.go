// Package fetchv1 implements techaro.lol.reputationdb.fetch.v1.FetchService:
// the endpoints for listing the published database versions and getting a
// download URL for one of them.
//
// These endpoints are unauthenticated today, exactly like the free tier is.
// meta.proto declares an APIKey bearer scheme, but nothing validates it yet.
package fetchv1

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

const (
	// presignExpiry is how long a download URL stays usable. Long enough to
	// download a few hundred megabytes on a bad connection, short enough that a
	// leaked URL is not a permanent hole in a paid product.
	//
	//lint:ignore U1000 consumed by FetchService.Fetch, added in Task 8
	presignExpiry = time.Hour
	// clientLifetime is how long a client should wait before asking for a newer
	// version. It matches the free tier, and comfortably outlasts the daily
	// build in .github/workflows/build-database.yml.
	//
	//lint:ignore U1000 consumed by FetchService.Info, added in Task 7
	clientLifetime = 6 * time.Hour
	// indexCacheTTL is how long a fetched version index is served from memory.
	// The index changes at most once a day, so reading it from the bucket on
	// every request would be a round trip per API call for data that is almost
	// never different.
	indexCacheTTL = time.Minute
)

// Server implements fetchv1connect.FetchServiceHandler.
type Server struct {
	store dbstore.Store
	lg    *slog.Logger
	now   func() time.Time

	mu       sync.Mutex
	cached   *fetchv1.ListResponse
	cachedAt time.Time
}

// New builds a Server backed by the configured Tigris bucket.
func New(ctx context.Context, lg *slog.Logger, cfg *internal.Config) (*Server, error) {
	// An empty bucket name is left unset rather than passed through, so that
	// simplestorage falls back to TIGRIS_STORAGE_BUCKET instead of failing on a
	// bucket literally named "".
	var opts []simplestorage.Option
	if cfg.TigrisBucket != "" {
		opts = append(opts, simplestorage.WithBucket(cfg.TigrisBucket))
	}

	st, err := simplestorage.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating Tigris client: %w", err)
	}

	s := newServer(st, lg)

	// Read the index once at startup, so a bad credential or an unreachable
	// bucket surfaces here rather than on the first user request.
	idx, err := s.index(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't read the database version index: %w", err)
	}
	lg.InfoContext(ctx, "read the database version index", "versions", len(idx.GetVersions()))

	return s, nil
}

// newServer wraps an arbitrary store, so tests can supply a fake.
func newServer(store dbstore.Store, lg *slog.Logger) *Server {
	return &Server{
		store: store,
		lg:    lg.With("handler", "fetchv1"),
		now:   time.Now,
	}
}

// index returns the version index, reading it from the bucket at most once per
// indexCacheTTL.
//
// A refetch that fails returns the error rather than serving the stale copy: a
// client acting on a version list the server can no longer confirm is worse
// than a client that retries. The lock is held across the bucket read, which
// serializes requests during a refetch — at this request volume that costs
// nothing and it keeps a burst of traffic from stampeding the bucket.
//
// The returned message is shared with the cache and with every other caller in
// the same TTL window. It is read-only after decoding; nothing may mutate it.
func (s *Server) index(ctx context.Context) (*fetchv1.ListResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && s.now().Sub(s.cachedAt) < indexCacheTTL {
		return s.cached, nil
	}

	idx, err := dbstore.LoadIndex(ctx, s.store, s.lg)
	if err != nil {
		return nil, err
	}

	s.cached = idx
	s.cachedAt = s.now()

	return idx, nil
}
