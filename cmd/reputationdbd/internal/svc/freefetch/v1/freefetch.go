package freefetchv1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	freefetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1"
	"github.com/google/go-github/v89/github"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	cli *github.Client
	lg  *slog.Logger
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
	}

	return result, nil
}

func (s *Server) Fetch(ctx context.Context, req *connect.Request[freefetchv1.FetchRequest]) (*connect.Response[freefetchv1.FetchResponse], error) {
	releases, _, err := s.cli.Repositories.ListReleases(ctx, "TecharoHQ", "reputationdb", nil)
	if err != nil {
		s.lg.ErrorContext(ctx, "can't fetch releases", "err", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	var release *github.RepositoryRelease
	for _, r := range releases {
		if r.GetName() == "Free datacentre IP database (rolling)" {
			release = r
		}
	}

	if release == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot find rolling release"))
	}

	assets, _, err := s.cli.Repositories.ListReleaseAssets(ctx, "TecharoHQ", "reputationdb", release.GetID(), nil)
	if err != nil {
		s.lg.ErrorContext(ctx, "can't fetch release assets for free database", "releaseID", release.GetID(), "err", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	var asset *github.ReleaseAsset
	for _, a := range assets {
		if a.GetName() == "datacenter.mmdb.zstd" {
			asset = a
		}
	}

	if asset == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("cannot find rolling release asset"))
	}

	s.lg.InfoContext(ctx, "download URL got", "download_url", asset.GetBrowserDownloadURL())

	resp := connect.NewResponse[freefetchv1.FetchResponse](&freefetchv1.FetchResponse{
		Lifetime:     durationpb.New(6 * time.Hour),
		Version:      asset.GetDigest(),
		PresignedUrl: asset.GetBrowserDownloadURL(),
		CreatedAt:    timestamppb.New(asset.GetCreatedAt().Time),
	})

	return resp, nil
}
