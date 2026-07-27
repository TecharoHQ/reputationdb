// Package fetchv1 implements techaro.lol.reputationdb.fetch.v1.FetchService:
// the endpoints for listing the published database versions and getting a
// download URL for one of them.
//
// These endpoints are unauthenticated today, exactly like the free tier is.
// meta.proto declares an APIKey bearer scheme, but nothing validates it yet.
package fetchv1

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// indexCacheTTL is how long a fetched version index is served from memory.
// The index changes at most once a day, so reading it from the bucket on
// every request would be a round trip per API call for data that is almost
// never different.
const indexCacheTTL = time.Minute

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

// List returns every database version the index currently retains.
func (s *Server) List(ctx context.Context, req *connect.Request[fetchv1.ListRequest]) (*connect.Response[fetchv1.ListResponse], error) {
	idx, err := s.index(ctx)
	if err != nil {
		s.lg.ErrorContext(ctx, "can't read the version index", "err", err)
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}

	return connect.NewResponse(&fetchv1.ListResponse{Versions: idx.GetVersions()}), nil
}

// validVersionID reports whether id has the shape dbstore.VersionID produces:
// 86 characters of unpadded URL-safe base64.
//
// ObjectKey concatenates this straight into a bucket key. S3 keys have no path
// traversal, so a malformed ID is not an escape, but checking the shape before
// touching the bucket keeps a caller from steering the server at arbitrary
// keys and turns garbage input into a clear InvalidArgument instead of a
// puzzling NotFound.
func validVersionID(id string) bool {
	if len(id) != dbstore.VersionIDLength {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil
}

// findVersion returns the index entry for id, or nil if the index has none.
func findVersion(idx *fetchv1.ListResponse, id string) *fetchv1.DatabaseVersion {
	for _, v := range idx.GetVersions() {
		if v.GetVersionId() == id {
			return v
		}
	}
	return nil
}
