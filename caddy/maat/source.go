package maat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	fetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1/fetchv1connect"
	freefetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1"
	freefetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1/fetchv1connect"
)

// tier identifies which of the two published databases a matcher needs.
type tier string

const (
	// tierFree is the datacentre-only database. It is served without
	// credentials and is a few tens of megabytes.
	tierFree tier = "free"
	// tierFull is the complete database across every category. It requires an
	// API key and is on the order of 800 MiB uncompressed.
	tierFull tier = "full"
)

// build describes one published database as the server currently sees it.
type build struct {
	// version identifies the contents, and is empty if the server didn't say.
	// Both tiers derive it from the database bytes, so an unchanged version
	// means unchanged data.
	version string
	// url is a time-limited download URL for the zstd-compressed database. It
	// carries its own authorization, so it is fetched without credentials.
	url string
	// lifetime is how long the server wants clients to wait before checking
	// for a newer build.
	lifetime time.Duration
	// createdAt is when the build was published, or the zero time if the
	// server didn't say.
	createdAt time.Time
}

// databaseSource resolves the current build for one tier.
type databaseSource interface {
	current(ctx context.Context) (build, error)
}

// newSource returns the source for t. The API key is ignored for the free
// tier, which has nothing to authorize.
func newSource(t tier, server, apiKey string) databaseSource {
	if t == tierFree {
		return &freeSource{cli: freefetchv1connect.NewFetchServiceClient(http.DefaultClient, server)}
	}

	var opts []connect.ClientOption
	if apiKey != "" {
		opts = append(opts, connect.WithInterceptors(bearerAuth(apiKey)))
	}
	return &fullSource{cli: fetchv1connect.NewFetchServiceClient(http.DefaultClient, server, opts...)}
}

// freeSource serves the datacentre-only database. Its Fetch call always
// describes the newest build, so one round trip answers everything.
type freeSource struct {
	cli freefetchv1connect.FetchServiceClient
}

func (s *freeSource) current(ctx context.Context) (build, error) {
	resp, err := s.cli.Fetch(ctx, connect.NewRequest(&freefetchv1.FetchRequest{}))
	if err != nil {
		return build{}, fmt.Errorf("can't look up free database download URL: %w", err)
	}

	msg := resp.Msg
	return build{
		version:   msg.GetVersion(),
		url:       msg.GetPresignedUrl(),
		lifetime:  msg.GetLifetime().AsDuration(),
		createdAt: msg.GetCreatedAt().AsTime(),
	}, nil
}

// fullSource serves the complete database. Unlike the free tier it is
// versioned, so finding the newest build takes a List before the Fetch.
type fullSource struct {
	cli fetchv1connect.FetchServiceClient
}

func (s *fullSource) current(ctx context.Context) (build, error) {
	list, err := s.cli.List(ctx, connect.NewRequest(&fetchv1.ListRequest{}))
	if err != nil {
		return build{}, fmt.Errorf("can't list database versions: %w", err)
	}

	versions := list.Msg.GetVersions()
	if len(versions) == 0 {
		return build{}, errors.New("server has no published database versions")
	}

	// The list is ordered newest first.
	newest := versions[0]
	if newest.GetVersionId() == "" {
		return build{}, errors.New("newest database version has no version ID")
	}

	resp, err := s.cli.Fetch(ctx, connect.NewRequest(&fetchv1.FetchRequest{
		VersionId: newest.GetVersionId(),
	}))
	if err != nil {
		return build{}, fmt.Errorf("can't look up download URL for version %s: %w", newest.GetVersionId(), err)
	}

	msg := resp.Msg
	// Prefer the version the fetch call reports, falling back to the listing:
	// they should agree, but the fetch response is the one describing the URL
	// we're about to download.
	version := msg.GetVersion().GetVersionId()
	if version == "" {
		version = newest.GetVersionId()
	}

	return build{
		version:   version,
		url:       msg.GetPresignedUrl(),
		lifetime:  msg.GetLifetime().AsDuration(),
		createdAt: newest.GetCreatedAt().AsTime(),
	}, nil
}

// bearerAuth attaches the API key to outgoing requests.
func bearerAuth(apiKey string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().IsClient {
				req.Header().Set("Authorization", "Bearer "+apiKey)
			}
			return next(ctx, req)
		}
	}
}

// Interface guards
var (
	_ databaseSource = (*freeSource)(nil)
	_ databaseSource = (*fullSource)(nil)
)
