package freefetchv1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	freefetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1"
	"github.com/google/go-github/v89/github"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// clientLifetime is how long a client should wait before asking for a newer
	// copy of the free database. It comfortably outlasts the daily build in
	// .github/workflows/build-database.yml.
	clientLifetime = 6 * time.Hour
	// assetCacheTTL is how long the resolved release asset is served from memory.
	// The rolling release is rebuilt at most once a day, so hitting GitHub on
	// every request spends rate limit on an answer that is almost never
	// different. It matches clientLifetime, so a well-behaved client and the
	// cache turn over on the same schedule.
	assetCacheTTL = 6 * time.Hour
)

type Server struct {
	cli *github.Client
	lg  *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	cached   *github.ReleaseAsset
	cachedAt time.Time
}

func New(ctx context.Context, lg *slog.Logger, cfg *internal.Config) (*Server, error) {
	cli, err := github.NewClient(github.WithAuthToken(cfg.GitHubToken))
	if err != nil {
		return nil, err
	}

	if _, _, err := cli.Users.Get(ctx, ""); err != nil {
		return nil, fmt.Errorf("can't fetch info about self: %w", err)
	}

	result := &Server{
		cli: cli,
		lg:  lg.With("handler", "freefetchv1"),
		now: time.Now,
	}

	return result, nil
}

// asset resolves the rolling free database asset, asking GitHub at most once
// per assetCacheTTL.
//
// A refetch that fails returns the error rather than serving the stale asset: a
// browser download URL outlives the release it came from, but a client acting
// on an asset the server can no longer confirm is worse than a client that
// retries. The lock is held across the two API calls, which serializes requests
// during a refetch — at this request volume that costs nothing and it keeps a
// burst of traffic from stampeding GitHub.
//
// The returned asset is shared with the cache and with every other caller in
// the same TTL window. Nothing may mutate it.
func (s *Server) asset(ctx context.Context) (*github.ReleaseAsset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && s.now().Sub(s.cachedAt) < assetCacheTTL {
		return s.cached, nil
	}

	releases, _, err := s.cli.Repositories.ListReleases(ctx, "TecharoHQ", "reputationdb", nil)
	if err != nil {
		s.lg.ErrorContext(ctx, "can't fetch releases", "err", err)
		return nil, err
	}

	var release *github.RepositoryRelease
	for _, r := range releases {
		if r.GetName() == "Free datacentre IP database (rolling)" {
			release = r
		}
	}

	if release == nil {
		return nil, errors.New("cannot find rolling release")
	}

	assets, _, err := s.cli.Repositories.ListReleaseAssets(ctx, "TecharoHQ", "reputationdb", release.GetID(), nil)
	if err != nil {
		s.lg.ErrorContext(ctx, "can't fetch release assets for free database", "releaseID", release.GetID(), "err", err)
		return nil, err
	}

	var asset *github.ReleaseAsset
	for _, a := range assets {
		if a.GetName() == "datacenter.mmdb.zstd" {
			asset = a
		}
	}

	if asset == nil {
		return nil, errors.New("cannot find rolling release asset")
	}

	s.lg.InfoContext(ctx, "download URL got", "download_url", asset.GetBrowserDownloadURL())

	s.cached = asset
	s.cachedAt = s.now()

	return asset, nil
}

func (s *Server) Fetch(ctx context.Context, req *connect.Request[freefetchv1.FetchRequest]) (*connect.Response[freefetchv1.FetchResponse], error) {
	asset, err := s.asset(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	resp := connect.NewResponse[freefetchv1.FetchResponse](&freefetchv1.FetchResponse{
		Lifetime:     durationpb.New(clientLifetime),
		Version:      asset.GetDigest(),
		PresignedUrl: asset.GetBrowserDownloadURL(),
		CreatedAt:    timestamppb.New(asset.GetCreatedAt().Time),
	})

	return resp, nil
}
