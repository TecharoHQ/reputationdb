// Package reputationdbv1 implements
// techaro.lol.reputationdb.v1.ReputationService: the endpoint that answers
// reputation questions about a batch of IP addresses.
//
// Queries are served from a local memory-mapped copy of the newest published
// database, kept fresh by dbcache. Until that copy lands — which on a cold
// start is minutes after the process comes up — queries fail with Unavailable
// rather than reporting every address as clean.
//
// This endpoint is unauthenticated today, exactly like the fetch endpoints
// are. meta.proto declares an APIKey bearer scheme, but nothing validates it
// yet.
package reputationdbv1

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	reputationdbv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/v1"
	reputationdbv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/v1/reputationdbv1connect"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// parseAddrs turns the requested strings into the addresses to look up,
// dropping duplicates.
//
// inputs maps each parsed address back to the string the client wrote it as, so
// the response can echo what was asked about rather than a normalized form the
// client would then have to match up itself. When two spellings of the same
// address are requested — 1.2.3.4 and ::ffff:1.2.3.4 — the first one wins and
// there is one record, not two.
func parseAddrs(raw []string) (addrs []netip.Addr, inputs map[netip.Addr]string, err error) {
	inputs = make(map[netip.Addr]string, len(raw))

	for _, s := range raw {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, nil, fmt.Errorf("%q is not an IP address", s)
		}
		// Unmap so a v4-in-v6 address looks up as the IPv4 address it is.
		addr = addr.Unmap()

		if _, seen := inputs[addr]; seen {
			continue
		}
		inputs[addr] = s
		addrs = append(addrs, addr)
	}

	return addrs, inputs, nil
}

// toRecord converts a decoded database record into its protobuf form.
//
// ipAddress is the string the client asked about, not addr.String(): see
// parseAddrs.
//
// The database stores categories as a bitmask. The wire format keeps the
// booleans and the names, because a client reads them directly. A client also
// holds one record at a time, so it does not pay the memory cost that the
// bitmask removes. This function is where the two forms meet.
func toRecord(ipAddress string, res reputationdb.Result) *reputationdbv1.Record {
	sources := make([]*reputationdbv1.ListMembership, 0, len(res.Sources))
	for _, s := range res.Sources {
		sources = append(sources, &reputationdbv1.ListMembership{
			Repository: s.Repository,
			List:       s.List,
			Provider:   s.Provider,
			Category:   s.Category.String(),
		})
	}

	return &reputationdbv1.Record{
		IpAddress:    ipAddress,
		IsVpn:        res.Categories.Has(reputationdb.CategoryByteVPN),
		IsDatacenter: res.Categories.Has(reputationdb.CategoryByteDatacenter),
		IsCrawler:    res.Categories.Has(reputationdb.CategoryByteCrawler),
		IsProxy:      res.Categories.Has(reputationdb.CategoryByteProxy),
		Categories:   res.Categories.Strings(),
		Providers:    res.Providers(),
		Sources:      sources,
	}
}

// maxBatchSize is how many addresses one request may ask about. The proto tells
// protovalidate the same thing, so oversized requests are rejected at the
// interceptor before they reach here; the check is repeated because this
// handler is also called directly, without that interceptor, by tests and by
// any future in-process caller.
const maxBatchSize = 100

// Server implements reputationdbv1connect.ReputationServiceHandler.
type Server struct {
	cache *dbcache.Cache
	lg    *slog.Logger
}

// New builds a Server backed by a local, self-refreshing copy of the newest
// database in the configured Tigris bucket.
//
// It returns as soon as the cache is started; the first download runs in the
// background and takes minutes. Queries answer Unavailable until it lands.
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

	lg = lg.With("handler", "reputationdbv1")

	cache, err := dbcache.New(ctx, lg, dbcache.NewBucketSource(st, lg), cfg.DatabaseCacheDir)
	if err != nil {
		return nil, err
	}

	return newServer(cache, lg), nil
}

// newServer wraps an arbitrary cache, so tests can supply one backed by a fake
// source.
func newServer(cache *dbcache.Cache, lg *slog.Logger) *Server {
	return &Server{cache: cache, lg: lg}
}

// Query looks a batch of addresses up in the database.
//
// Every requested address gets a record, in the order it was asked about. An
// address the database has nothing on comes back carrying only its own
// ip_address, so a client can line responses up against its request one for
// one. That means the presence of a record says nothing: an empty categories
// and sources list is what "not listed" looks like.
func (s *Server) Query(ctx context.Context, req *connect.Request[reputationdbv1.QueryRequest]) (*connect.Response[reputationdbv1.QueryResponse], error) {
	raw := req.Msg.GetIpAddresses()
	if len(raw) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("ip_addresses must contain at least one IP address"))
	}
	if len(raw) > maxBatchSize {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("ip_addresses contains %d addresses, the limit is %d", len(raw), maxBatchSize))
	}

	addrs, inputs, err := parseAddrs(raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	matches, createdAt, loaded, err := s.cache.Query(addrs)
	if !loaded {
		// Saying "none of these are listed" out of a database that isn't there
		// would be a fail-open the caller can't detect.
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("no reputation database has been loaded yet, try again shortly"))
	}
	if err != nil {
		s.lg.ErrorContext(ctx, "can't query the reputation database", "err", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Index the hits so the response can be built by walking the requested
	// addresses instead of the matches: that keeps the records in the order
	// the client asked, and gives an address the database has nothing on a
	// zero Result, which toRecord renders as a record carrying only its
	// ip_address.
	hits := make(map[netip.Addr]reputationdb.Result, len(matches))
	for _, m := range matches {
		hits[m.Addr] = m.Result
	}

	records := make([]*reputationdbv1.Record, 0, len(addrs))
	for _, addr := range addrs {
		records = append(records, toRecord(inputs[addr], hits[addr]))
	}

	return connect.NewResponse(&reputationdbv1.QueryResponse{
		DatabaseCreatedAt: timestamppb.New(createdAt),
		Records:           records,
	}), nil
}

// Interface guards
var (
	_ reputationdbv1connect.ReputationServiceHandler = (*Server)(nil)
)
