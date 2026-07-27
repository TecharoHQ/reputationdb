package maat

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/TecharoHQ/reputationdb"
	freefetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/free/fetch/v1/fetchv1connect"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/maxmind/mmdbwriter"
	"go.uber.org/zap"
)

// buildDB writes a tiny in-memory database mapping CIDR -> Record so matching
// can be exercised without downloading anything.
func buildDB(t *testing.T, entries map[string]reputationdb.Record) *reputationdb.DB {
	t.Helper()

	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "Techaro-Veil-Datacenter",
		IPVersion:    6,
		RecordSize:   28,
	})
	if err != nil {
		t.Fatalf("mmdbwriter.New: %v", err)
	}

	for cidr, rec := range entries {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", cidr, err)
		}
		if err := tree.Insert(network, rec.DataType()); err != nil {
			t.Fatalf("Insert(%q): %v", cidr, err)
		}
	}

	var buf bytes.Buffer
	if _, err := tree.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	db, err := reputationdb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	return db
}

// testMatcher returns a matcher backed by a fixed database: 1.2.3.4 is a
// datacentre address, 2606:4700::/32 is a crawler, everything else is unknown.
func testMatcher(t *testing.T, categories ...string) *Matcher {
	t.Helper()

	datacenter := reputationdb.Record{}
	datacenter.Add(reputationdb.ListMembership{
		Repository: "github.com/hexydec/ip-ranges",
		List:       "output/datacentres.txt",
		Provider:   "datacentres",
		Category:   reputationdb.CategoryDatacenter,
	})

	crawler := reputationdb.Record{}
	crawler.Add(reputationdb.ListMembership{
		Repository: "github.com/hexydec/ip-ranges",
		List:       "output/crawlers.txt",
		Provider:   "crawlers",
		Category:   reputationdb.CategoryCrawler,
	})

	db := buildDB(t, map[string]reputationdb.Record{
		"1.2.3.4/32":     datacenter,
		"2606:4700::/32": crawler,
	})

	return &Matcher{
		Categories: categories,
		logger:     zap.NewNop(),
		store:      &dbStore{db: db},
	}
}

func requestFrom(remoteAddr, clientIP string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	if clientIP != "" {
		ctx := context.WithValue(r.Context(), caddyhttp.VarsCtxKey, map[string]any{
			caddyhttp.ClientIPVarKey: clientIP,
		})
		r = r.WithContext(ctx)
	}
	return r
}

func TestMatcherMatchWithError(t *testing.T) {
	for _, tt := range []struct {
		name       string
		categories []string
		remoteAddr string
		clientIP   string
		want       bool
	}{
		{
			name:       "datacenter address matches datacenter category",
			categories: []string{reputationdb.CategoryDatacenter},
			remoteAddr: "1.2.3.4:31337",
			want:       true,
		},
		{
			name:       "datacenter address does not match vpn category",
			categories: []string{reputationdb.CategoryVPN},
			remoteAddr: "1.2.3.4:31337",
			want:       false,
		},
		{
			name:       "any of several categories is enough",
			categories: []string{reputationdb.CategoryVPN, reputationdb.CategoryDatacenter},
			remoteAddr: "1.2.3.4:31337",
			want:       true,
		},
		{
			name:       "no categories matches anything in the database",
			categories: nil,
			remoteAddr: "1.2.3.4:31337",
			want:       true,
		},
		{
			name:       "address absent from the database never matches",
			categories: nil,
			remoteAddr: "8.8.8.8:31337",
			want:       false,
		},
		{
			name:       "ipv6 crawler",
			categories: []string{reputationdb.CategoryCrawler},
			remoteAddr: "[2606:4700::1]:31337",
			want:       true,
		},
		{
			name:       "client_ip wins over the socket peer",
			categories: []string{reputationdb.CategoryDatacenter},
			remoteAddr: "10.0.0.1:31337",
			clientIP:   "1.2.3.4",
			want:       true,
		},
		{
			name:       "v4-in-v6 client_ip is unmapped before lookup",
			categories: []string{reputationdb.CategoryDatacenter},
			remoteAddr: "10.0.0.1:31337",
			clientIP:   "::ffff:1.2.3.4",
			want:       true,
		},
		{
			name:       "unparseable address does not match",
			categories: nil,
			remoteAddr: "not-an-address",
			want:       false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := testMatcher(t, tt.categories...)

			got, err := m.MatchWithError(requestFrom(tt.remoteAddr, tt.clientIP))
			if err != nil {
				t.Fatalf("MatchWithError: %v", err)
			}
			if got != tt.want {
				t.Errorf("match = %v, want %v", got, tt.want)
			}
		})
	}
}

// A matcher whose database hasn't loaded must fail open rather than match
// every request.
func TestMatcherMatchWithoutDatabase(t *testing.T) {
	m := &Matcher{logger: zap.NewNop(), store: &dbStore{}}

	got, err := m.MatchWithError(requestFrom("1.2.3.4:31337", ""))
	if err != nil {
		t.Fatalf("MatchWithError: %v", err)
	}
	if got {
		t.Error("matched with no database loaded, want no match")
	}
}

// Which database gets downloaded is derived from the categories, so this is
// the difference between needing an API key and not.
func TestMatcherTier(t *testing.T) {
	for _, tt := range []struct {
		name       string
		categories []string
		want       tier
	}{
		{
			name:       "datacenter alone is served by the free database",
			categories: []string{reputationdb.CategoryDatacenter},
			want:       tierFree,
		},
		{
			name:       "datacenter plus anything else needs the full database",
			categories: []string{reputationdb.CategoryDatacenter, reputationdb.CategoryVPN},
			want:       tierFull,
		},
		{
			name:       "another category alone needs the full database",
			categories: []string{reputationdb.CategoryVPN},
			want:       tierFull,
		},
		{
			name:       "no categories means anything at all, so the full database",
			categories: nil,
			want:       tierFull,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &Matcher{Categories: tt.categories}
			if got := m.tier(); got != tt.want {
				t.Errorf("tier = %q, want %q", got, tt.want)
			}
		})
	}
}

// These all have to be caught before the matcher tries to download anything.
func TestMatcherProvisionConfigErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		matcher Matcher
	}{
		{
			name:    "no server",
			matcher: Matcher{Categories: []string{reputationdb.CategoryDatacenter}},
		},
		{
			name: "server is not a url",
			matcher: Matcher{
				Server:     "reputationdb.example.com",
				Categories: []string{reputationdb.CategoryDatacenter},
			},
		},
		{
			name: "unknown category",
			matcher: Matcher{
				Server:     "https://reputationdb.example.com",
				Categories: []string{"datacentre"},
			},
		},
		{
			name: "full database without an api key",
			matcher: Matcher{
				Server:     "https://reputationdb.example.com",
				Categories: []string{reputationdb.CategoryVPN},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
			defer cancel()

			m := tt.matcher
			if err := m.Provision(ctx); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

// TestMatcherProvision runs the real path end to end: provision against a
// stub server, match a request, and clean up.
func TestMatcherProvision(t *testing.T) {
	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	svc := &fakeFreeService{url: assetServer(t, &payload, &downloads)}
	svc.version.Store("v1")

	mux := http.NewServeMux()
	mux.Handle(freefetchv1connect.NewFetchServiceHandler(svc))
	api := httptest.NewServer(mux)
	defer api.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	// The trailing slash should be trimmed rather than doubled into the
	// Connect procedure path.
	m := &Matcher{
		Server:      api.URL + "/",
		Categories:  []string{"DATACENTER", reputationdb.CategoryDatacenter},
		StoragePath: t.TempDir(),
	}

	if err := m.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	})

	waitReady(t, m.store)

	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := []string{reputationdb.CategoryDatacenter}; !slices.Equal(m.Categories, want) {
		t.Errorf("Categories = %v, want %v: they should be lowercased and deduplicated", m.Categories, want)
	}
	if m.key.tier != tierFree {
		t.Errorf("tier = %q, want %q", m.key.tier, tierFree)
	}

	got, err := m.MatchWithError(requestFrom("1.2.3.4:31337", ""))
	if err != nil {
		t.Fatalf("MatchWithError: %v", err)
	}
	if !got {
		t.Error("1.2.3.4 did not match, want a match")
	}
}

// A server that can't be reached must not stop Caddy from coming up. The
// matcher provisions, fails open until the database lands, and keeps retrying
// in the background.
func TestMatcherProvisionWithUnreachableServer(t *testing.T) {
	// A server that's listening but refuses every call, so the failure is
	// immediate rather than a connection timeout.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer api.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	m := &Matcher{
		Server:      api.URL,
		Categories:  []string{reputationdb.CategoryDatacenter},
		StoragePath: t.TempDir(),
	}

	if err := m.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v, want it to succeed and load in the background", err)
	}
	t.Cleanup(func() {
		if err := m.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	})

	got, err := m.MatchWithError(requestFrom("1.2.3.4:31337", ""))
	if err != nil {
		t.Fatalf("MatchWithError: %v", err)
	}
	if got {
		t.Error("matched with no database loaded, want it to fail open")
	}

	select {
	case <-m.store.ready:
		t.Error("store reported a database loaded, but every fetch failed")
	default:
	}
}

// A matcher with no storage path configured must still land somewhere sane.
func TestMatcherDefaultStoragePath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var downloads atomic.Int64
	var payload atomic.Value
	payload.Store(compressedTestDB(t, "1.2.3.4/32"))

	svc := &fakeFreeService{url: assetServer(t, &payload, &downloads)}
	svc.version.Store("v1")

	mux := http.NewServeMux()
	mux.Handle(freefetchv1connect.NewFetchServiceHandler(svc))
	api := httptest.NewServer(mux)
	defer api.Close()

	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	m := &Matcher{Server: api.URL, Categories: []string{reputationdb.CategoryDatacenter}}
	if err := m.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	})

	waitReady(t, m.store)

	want := filepath.Join(caddy.AppDataDir(), "reputationdb")
	if m.StoragePath != want {
		t.Errorf("StoragePath = %q, want %q", m.StoragePath, want)
	}
	if _, err := os.Stat(m.store.currentPath()); err != nil {
		t.Errorf("database not on disk under the default path: %v", err)
	}
}

func TestMatcherUnmarshalCaddyfile(t *testing.T) {
	for _, tt := range []struct {
		name           string
		input          string
		wantServer     string
		wantCategories []string
		wantAPIKey     string
		wantStorage    string
		wantErr        bool
	}{
		{
			name: "every option",
			input: `maat {
				server https://reputationdb.example.com
				categories datacenter vpn
				api_key hunter2
				storage_path /var/lib/reputationdb
			}`,
			wantServer:     "https://reputationdb.example.com",
			wantCategories: []string{"datacenter", "vpn"},
			wantAPIKey:     "hunter2",
			wantStorage:    "/var/lib/reputationdb",
		},
		{
			name: "api_key twice is an error",
			input: `maat {
				api_key one
				api_key two
			}`,
			wantErr: true,
		},
		{
			name: "storage_path with no argument is an error",
			input: `maat {
				storage_path
			}`,
			wantErr: true,
		},
		{
			name:           "inline categories",
			input:          `maat datacenter vpn`,
			wantCategories: []string{"datacenter", "vpn"},
		},
		{
			name:           "no arguments at all",
			input:          `maat`,
			wantCategories: nil,
		},
		{
			name: "block form",
			input: `maat {
				server https://reputationdb.example.com
				categories datacenter vpn
			}`,
			wantServer:     "https://reputationdb.example.com",
			wantCategories: []string{"datacenter", "vpn"},
		},
		{
			name: "inline and block categories merge",
			input: `maat tor {
				server https://reputationdb.example.com
				categories datacenter
			}`,
			wantServer:     "https://reputationdb.example.com",
			wantCategories: []string{"tor", "datacenter"},
		},
		{
			name: "repeated matchers merge",
			input: `maat datacenter
			maat vpn`,
			wantCategories: []string{"datacenter", "vpn"},
		},
		{
			name: "server twice is an error",
			input: `maat {
				server https://one.example.com
				server https://two.example.com
			}`,
			wantErr: true,
		},
		{
			name: "unknown option is an error",
			input: `maat {
				sever https://reputationdb.example.com
			}`,
			wantErr: true,
		},
		{
			name: "categories with no arguments is an error",
			input: `maat {
				categories
			}`,
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var m Matcher
			err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tt.input))

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalCaddyfile: %v", err)
			}

			if m.Server != tt.wantServer {
				t.Errorf("Server = %q, want %q", m.Server, tt.wantServer)
			}
			if m.APIKey != tt.wantAPIKey {
				t.Errorf("APIKey = %q, want %q", m.APIKey, tt.wantAPIKey)
			}
			if m.StoragePath != tt.wantStorage {
				t.Errorf("StoragePath = %q, want %q", m.StoragePath, tt.wantStorage)
			}
			if len(m.Categories) != len(tt.wantCategories) {
				t.Fatalf("Categories = %v, want %v", m.Categories, tt.wantCategories)
			}
			for i, want := range tt.wantCategories {
				if m.Categories[i] != want {
					t.Errorf("Categories[%d] = %q, want %q", i, m.Categories[i], want)
				}
			}
		})
	}
}
