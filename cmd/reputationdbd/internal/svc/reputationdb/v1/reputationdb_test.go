package reputationdbv1

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache/dbcachetest"
	reputationdbv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/v1"
	reputationdbv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/v1/reputationdbv1connect"
)

func TestParseAddrs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		raw       []string
		wantAddrs []string
		wantErr   bool
	}{
		{
			name:      "one IPv4 address",
			raw:       []string{"1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "one IPv6 address",
			raw:       []string{"2001:db8::1"},
			wantAddrs: []string{"2001:db8::1"},
		},
		{
			name:      "a v4-in-v6 address is unmapped for the lookup",
			raw:       []string{"::ffff:1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "duplicates collapse",
			raw:       []string{"1.2.3.4", "1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "a v4 address and its v4-in-v6 form are the same address",
			raw:       []string{"1.2.3.4", "::ffff:1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{name: "not an address", raw: []string{"nonsense"}, wantErr: true},
		{name: "a CIDR prefix", raw: []string{"1.2.3.0/24"}, wantErr: true},
		{name: "a host and port", raw: []string{"1.2.3.4:80"}, wantErr: true},
		{name: "empty string", raw: []string{""}, wantErr: true},
		{name: "one bad address among good ones", raw: []string{"1.2.3.4", "nope"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addrs, inputs, err := parseAddrs(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAddrs(%v) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAddrs(%v) error = %v", tt.raw, err)
			}

			if len(addrs) != len(tt.wantAddrs) {
				t.Fatalf("parseAddrs(%v) = %v, want %v", tt.raw, addrs, tt.wantAddrs)
			}
			for i, want := range tt.wantAddrs {
				if addrs[i].String() != want {
					t.Errorf("parseAddrs(%v)[%d] = %s, want %s", tt.raw, i, addrs[i], want)
				}
			}
			for _, addr := range addrs {
				if _, ok := inputs[addr]; !ok {
					t.Errorf("parseAddrs(%v) has no input string recorded for %s", tt.raw, addr)
				}
			}
		})
	}
}

// The response echoes what the client wrote, not the normalized form, so a
// client can match records to requests by string equality.
func TestParseAddrsEchoesTheRequestedForm(t *testing.T) {
	addrs, inputs, err := parseAddrs([]string{"::ffff:1.2.3.4"})
	if err != nil {
		t.Fatalf("parseAddrs error = %v", err)
	}
	if got := inputs[addrs[0]]; got != "::ffff:1.2.3.4" {
		t.Errorf("inputs[%s] = %q, want the string the client sent", addrs[0], got)
	}
}

func TestToRecord(t *testing.T) {
	res := reputationdb.Result{
		IsVPN:        true,
		IsDatacenter: true,
		IsCrawler:    false,
		IsProxy:      false,
		Categories:   []string{"datacenter", "vpn"},
		Providers:    []string{"datacentres", "nordvpn"},
		Sources: []reputationdb.ListMembership{
			{
				Repository: "github.com/az0/vpn_ip",
				List:       "data/input/ip/nordvpn.txt",
				Provider:   "nordvpn",
				Category:   "vpn",
			},
		},
	}

	got := toRecord("1.2.3.4", res)

	if got.GetIpAddress() != "1.2.3.4" {
		t.Errorf("IpAddress = %q, want %q", got.GetIpAddress(), "1.2.3.4")
	}
	if !got.GetIsVpn() || !got.GetIsDatacenter() {
		t.Errorf("IsVpn = %v, IsDatacenter = %v, want both true", got.GetIsVpn(), got.GetIsDatacenter())
	}
	if got.GetIsCrawler() || got.GetIsProxy() {
		t.Errorf("IsCrawler = %v, IsProxy = %v, want both false", got.GetIsCrawler(), got.GetIsProxy())
	}
	if len(got.GetCategories()) != 2 || got.GetCategories()[0] != "datacenter" {
		t.Errorf("Categories = %v, want [datacenter vpn]", got.GetCategories())
	}
	if len(got.GetProviders()) != 2 {
		t.Errorf("Providers = %v, want two entries", got.GetProviders())
	}

	sources := got.GetSources()
	if len(sources) != 1 {
		t.Fatalf("Sources = %v, want one entry", sources)
	}
	if sources[0].GetRepository() != "github.com/az0/vpn_ip" {
		t.Errorf("Sources[0].Repository = %q, want %q", sources[0].GetRepository(), "github.com/az0/vpn_ip")
	}
	if sources[0].GetList() != "data/input/ip/nordvpn.txt" {
		t.Errorf("Sources[0].List = %q, want %q", sources[0].GetList(), "data/input/ip/nordvpn.txt")
	}
	if sources[0].GetProvider() != "nordvpn" {
		t.Errorf("Sources[0].Provider = %q, want %q", sources[0].GetProvider(), "nordvpn")
	}
	if sources[0].GetCategory() != "vpn" {
		t.Errorf("Sources[0].Category = %q, want %q", sources[0].GetCategory(), "vpn")
	}
}

// An address with no list memberships still has to produce a well-formed
// record rather than a nil sources slice the JSON encoder renders as null.
func TestToRecordWithNoSources(t *testing.T) {
	got := toRecord("1.2.3.4", reputationdb.Result{})

	if got.GetSources() == nil {
		t.Error("Sources = nil, want an empty slice")
	}
	if len(got.GetSources()) != 0 {
		t.Errorf("Sources = %v, want it empty", got.GetSources())
	}
}

// Compile-time proof that Server can be handed to NewReputationServiceHandler.
var _ reputationdbv1connect.ReputationServiceHandler = (*Server)(nil)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// loadedServer returns a Server whose cache has finished loading a database
// containing the given CIDRs.
func loadedServer(t *testing.T, cidrs ...string) *Server {
	t.Helper()

	compressed, err := dbcachetest.CompressedDatabase(cidrs...)
	if err != nil {
		t.Fatalf("CompressedDatabase: %v", err)
	}

	src := dbcachetest.New()
	src.Publish("v1", time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC), compressed)

	return newServerWithCache(t, src)
}

// emptyServer returns a Server whose cache will never load anything.
func emptyServer(t *testing.T) *Server {
	t.Helper()
	return newServerWithCache(t, dbcachetest.New())
}

func newServerWithCache(t *testing.T, src *dbcachetest.Fake) *Server {
	t.Helper()

	cache, err := dbcache.New(t.Context(), discardLogger(), src, t.TempDir())
	if err != nil {
		t.Fatalf("dbcache.New: %v", err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("cache.Close() error = %v", err)
		}
	})

	return newServer(cache, discardLogger())
}

// waitLoaded blocks until the server's cache has a database.
func waitLoaded(t *testing.T, s *Server) {
	t.Helper()

	select {
	case <-s.cache.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the database to load")
	}
}

func TestQueryReturnsRecordsForListedAddresses(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	resp, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
		IpAddresses: []string{"1.2.3.4"},
	}))
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	records := resp.Msg.GetRecords()
	if len(records) != 1 {
		t.Fatalf("Query() returned %d records, want 1", len(records))
	}
	if got := records[0].GetIpAddress(); got != "1.2.3.4" {
		t.Errorf("Query() ip_address = %q, want %q", got, "1.2.3.4")
	}
	if !records[0].GetIsDatacenter() {
		t.Error("Query() is_datacenter = false, want true for a datacentre address")
	}

	want := time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC)
	if got := resp.Msg.GetDatabaseCreatedAt().AsTime(); !got.Equal(want) {
		t.Errorf("Query() database_created_at = %v, want %v", got, want)
	}
}

// Addresses with no record are omitted rather than returned empty, so a client
// can treat the presence of a record as the signal.
// An address the database has nothing on still gets a record, so a client can
// line the response up against its request one for one.
func TestQueryReturnsAnEmptyRecordForAnAddressWithNothingOnIt(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	resp, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
		IpAddresses: []string{"1.2.3.4", "9.9.9.9"},
	}))
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	records := resp.Msg.GetRecords()
	if len(records) != 2 {
		t.Fatalf("Query() returned %d records, want 2: one per requested address", len(records))
	}

	// Order follows the request, not the database.
	if got := records[0].GetIpAddress(); got != "1.2.3.4" {
		t.Errorf("Query() records[0] = %q, want 1.2.3.4", got)
	}
	if got := records[1].GetIpAddress(); got != "9.9.9.9" {
		t.Errorf("Query() records[1] = %q, want 9.9.9.9", got)
	}

	// The hit carries data; the miss carries nothing but its address. An empty
	// sources list is what "not listed" looks like now that every address gets
	// a record.
	if len(records[0].GetSources()) == 0 {
		t.Error("Query() records[0] has no sources, want the datacentre membership for 1.2.3.4")
	}
	if got := records[1].GetSources(); len(got) != 0 {
		t.Errorf("Query() records[1] sources = %v, want empty for an address with nothing on it", got)
	}
	if got := records[1].GetCategories(); len(got) != 0 {
		t.Errorf("Query() records[1] categories = %v, want empty", got)
	}
	if got := records[1].GetProviders(); len(got) != 0 {
		t.Errorf("Query() records[1] providers = %v, want empty", got)
	}
	if records[1].GetIsVpn() || records[1].GetIsDatacenter() || records[1].GetIsCrawler() || records[1].GetIsProxy() {
		t.Error("Query() records[1] has a category flag set, want all false for an address with nothing on it")
	}
}

// Every address misses, which is the case that used to return no records at
// all. The response must still carry one record each.
func TestQueryReturnsRecordsWhenNothingIsListed(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	resp, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
		IpAddresses: []string{"9.9.9.9", "8.8.4.4"},
	}))
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	records := resp.Msg.GetRecords()
	if len(records) != 2 {
		t.Fatalf("Query() returned %d records, want 2 even though neither address is listed", len(records))
	}
	for i, want := range []string{"9.9.9.9", "8.8.4.4"} {
		if got := records[i].GetIpAddress(); got != want {
			t.Errorf("Query() records[%d] = %q, want %q", i, got, want)
		}
	}
}

// Answering "nothing is listed" out of a database that hasn't loaded would be a
// fail-open the caller can't detect.
func TestQueryBeforeTheDatabaseLoadsIsUnavailable(t *testing.T) {
	s := emptyServer(t)

	_, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
		IpAddresses: []string{"1.2.3.4"},
	}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("Query() code = %v, want %v", got, connect.CodeUnavailable)
	}
}

func TestQueryRejectsMalformedAddresses(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	for _, raw := range []string{"nonsense", "1.2.3.0/24", "1.2.3.4:80", ""} {
		_, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
			IpAddresses: []string{raw},
		}))
		if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
			t.Errorf("Query(%q) code = %v, want %v", raw, got, connect.CodeInvalidArgument)
		}
	}
}

func TestQueryRejectsAnEmptyBatch(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	_, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("Query() code = %v, want %v", got, connect.CodeInvalidArgument)
	}
}

// oversizedBatch returns maxBatchSize+1 valid addresses, so that a request
// built from it is rejected only for its length. A batch of empty strings
// would also be rejected by the per-item address rule, which would leave the
// count untested.
func oversizedBatch() []string {
	raw := make([]string, maxBatchSize+1)
	for i := range raw {
		raw[i] = "1.2.3.4"
	}
	return raw
}

func TestQueryRejectsAnOversizedBatch(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	_, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{IpAddresses: oversizedBatch()}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("Query() code = %v, want %v", got, connect.CodeInvalidArgument)
	}
}

func TestQueryDedupesAddresses(t *testing.T) {
	s := loadedServer(t, "1.2.3.4/32")
	waitLoaded(t, s)

	resp, err := s.Query(context.Background(), connect.NewRequest(&reputationdbv1.QueryRequest{
		IpAddresses: []string{"1.2.3.4", "1.2.3.4", "::ffff:1.2.3.4"},
	}))
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if got := len(resp.Msg.GetRecords()); got != 1 {
		t.Errorf("Query() returned %d records for three spellings of one address, want 1", got)
	}
}

// The proto's per-item CEL rule is easy to break back into a field-level rule,
// which silently rejects every request. Guard it here.
func TestQueryRequestProtoValidation(t *testing.T) {
	v, err := protovalidate.New()
	if err != nil {
		t.Fatalf("protovalidate.New() error = %v", err)
	}

	for _, tt := range []struct {
		name    string
		msg     *reputationdbv1.QueryRequest
		wantErr bool
	}{
		{name: "one valid address", msg: &reputationdbv1.QueryRequest{IpAddresses: []string{"1.2.3.4"}}},
		{name: "an IPv6 address", msg: &reputationdbv1.QueryRequest{IpAddresses: []string{"2001:db8::1"}}},
		{name: "not an address", msg: &reputationdbv1.QueryRequest{IpAddresses: []string{"nonsense"}}, wantErr: true},
		{name: "no addresses", msg: &reputationdbv1.QueryRequest{}, wantErr: true},
		{
			name:    "more than the batch limit",
			msg:     &reputationdbv1.QueryRequest{IpAddresses: oversizedBatch()},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := v.Validate(tt.msg)
			if tt.wantErr && err == nil {
				t.Error("Validate() error = nil, want a validation failure")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() error = %v, want nil", err)
			}
		})
	}
}
