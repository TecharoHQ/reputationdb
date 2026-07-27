// Package maat provides a Caddy request matcher that looks the client IP up in
// a reputationdb database.
//
// It is a matcher rather than a handler so that the response is the operator's
// choice. Blocking outright is one option, but so is serving a challenge,
// tightening a rate limit, or just tagging the request for the access log:
//
//	@datacenter {
//		maat {
//			server https://reputationdb.example.com
//			categories datacenter
//		}
//	}
//	respond @datacenter "no datacentre traffic please" 403
//
// That example is served by the free datacentre-only database, which needs no
// credentials. Matching on anything else means the full database, which needs
// a Thoth API key:
//
//	@bad {
//		maat {
//			server https://reputationdb.example.com
//			categories datacenter vpn abuse
//			api_key {env.THOTH_API_KEY}
//		}
//	}
//
// Either way the database is cached on disk and memory-mapped rather than held
// on the heap. The full build is around 800 MiB uncompressed, so plan for that
// much room under storage_path.
//
// Caddy does not wait for the download. It comes up immediately and the
// matcher starts matching once the database lands, which on a cold start can
// be minutes later. Until then it fails open: nothing matches, so whatever the
// matcher guards is unguarded. If that isn't acceptable, pair it with a
// stricter default that the matcher relaxes rather than one it tightens.
package maat

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TecharoHQ/reputationdb"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Matcher{})
}

// knownCategories is every category the database can report. Matching on
// anything else is a typo, so configuration is rejected rather than silently
// never matching.
var knownCategories = []string{
	reputationdb.CategoryAbuse,
	reputationdb.CategoryCrawler,
	reputationdb.CategoryDatacenter,
	reputationdb.CategoryProxy,
	reputationdb.CategoryTor,
	reputationdb.CategoryVPN,
}

// Matcher matches requests whose client IP appears in the reputationdb
// database under at least one of the configured categories.
//
// Which database gets downloaded follows from Categories. Asking for nothing
// but "datacenter" is served by the free datacentre-only build, which needs no
// credentials; anything else needs the full build, and so an APIKey.
//
// Matchers wanting the same database from the same server share one download
// and one refresh goroutine, so listing this matcher on a dozen sites still
// only fetches the database once.
type Matcher struct {
	// Server is the base URL of the reputationdb API that hands out database
	// download URLs, e.g. https://reputationdb.example.com. Required.
	Server string `json:"server,omitempty"`

	// Categories is the set of reputation categories that count as a match:
	// any of "abuse", "crawler", "datacenter", "proxy", "tor", or "vpn". An
	// empty list matches any address present in the database at all.
	Categories []string `json:"categories,omitempty"`

	// APIKey is the Thoth API key authorizing access to the full database.
	// Required unless Categories is exactly ["datacenter"], which the free
	// database covers on its own.
	APIKey string `json:"api_key,omitempty"`

	// StoragePath is the directory the downloaded database is cached in. It
	// defaults to a "reputationdb" directory under Caddy's data directory.
	//
	// The full database is around 800 MiB uncompressed, so this needs to be
	// somewhere with room for it, and somewhere that survives restarts if you
	// don't want to download it again on every start.
	StoragePath string `json:"storage_path,omitempty"`

	key    storeKey
	store  *dbStore
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Matcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.maat",
		New: func() caddy.Module { return new(Matcher) },
	}
}

// Provision implements caddy.Provisioner. It validates the configuration and
// starts the database loading, but does not wait for it: see [loadStore].
func (m *Matcher) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger()

	repl := caddy.NewReplacer()
	m.Server = strings.TrimSuffix(repl.ReplaceAll(m.Server, ""), "/")
	if m.Server == "" {
		return fmt.Errorf("maat: no server configured")
	}
	// Nothing talks to the server until after provisioning, so a typo here
	// would otherwise only ever show up as a log line. Checking the shape of
	// the URL costs nothing and keeps that a config error.
	if u, err := url.Parse(m.Server); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("maat: server %q must be an http:// or https:// URL", m.Server)
	}

	for i, category := range m.Categories {
		category = strings.ToLower(repl.ReplaceAll(category, ""))
		if !slices.Contains(knownCategories, category) {
			return fmt.Errorf("maat: unknown category %q, must be one of %s", category, strings.Join(knownCategories, ", "))
		}
		m.Categories[i] = category
	}
	slices.Sort(m.Categories)
	m.Categories = slices.Compact(m.Categories)

	m.APIKey = repl.ReplaceAll(m.APIKey, "")
	m.key = storeKey{server: m.Server, tier: m.tier()}
	if m.key.tier == tierFull && m.APIKey == "" {
		return fmt.Errorf("maat: matching on %s needs the full database, so an api_key is required; the free database only covers %q",
			strings.Join(m.Categories, ", "), reputationdb.CategoryDatacenter)
	}

	dir := repl.ReplaceAll(m.StoragePath, "")
	if dir == "" {
		dir = filepath.Join(caddy.AppDataDir(), "reputationdb")
	}
	m.StoragePath = dir

	store, err := loadStore(m.key, m.APIKey, m.StoragePath, m.logger)
	if err != nil {
		return fmt.Errorf("maat: %w", err)
	}
	m.store = store

	return nil
}

// tier reports which database this matcher's categories need. Only a matcher
// that asks for datacentre addresses and nothing else can be served by the
// free build; an empty category list means "anything in the database", which
// is the full one.
func (m *Matcher) tier() tier {
	if len(m.Categories) == 1 && m.Categories[0] == reputationdb.CategoryDatacenter {
		return tierFree
	}
	return tierFull
}

// Validate implements caddy.Validator.
func (m *Matcher) Validate() error {
	if m.store == nil {
		return fmt.Errorf("maat: matcher was not provisioned")
	}
	return nil
}

// Cleanup implements caddy.CleanerUpper. The shared database is only torn down
// once the last matcher using it goes away.
func (m *Matcher) Cleanup() error {
	if m.store == nil {
		return nil
	}
	_, err := stores.Delete(m.key)
	return err
}

// MatchWithError implements caddyhttp.RequestMatcherWithError.
//
// Every failure path here reports "no match" rather than an error: a
// reputation lookup is advisory, and an unparseable address or a database that
// hasn't finished (re)loading should not turn into a failed request.
func (m *Matcher) MatchWithError(r *http.Request) (bool, error) {
	addr, ok := clientAddr(r)
	if !ok {
		return false, nil
	}

	result, found, loaded, err := m.store.lookup(addr)
	if !loaded {
		m.logger.Debug("no reputation database loaded yet, not matching")
		return false, nil
	}
	if err != nil {
		m.logger.Error("reputation database lookup failed",
			zap.String("client_ip", addr.String()),
			zap.Error(err))
		return false, nil
	}
	if !found {
		return false, nil
	}

	if len(m.Categories) == 0 {
		return true, nil
	}
	return slices.ContainsFunc(m.Categories, result.HasCategory), nil
}

// Match implements the deprecated caddyhttp.RequestMatcher interface, kept for
// compatibility with callers that haven't moved to MatchWithError. Discarding
// the error is safe here because MatchWithError never returns one.
func (m *Matcher) Match(r *http.Request) bool {
	match, _ := m.MatchWithError(r)
	return match
}

// clientAddr returns the address to look up, preferring Caddy's client_ip
// (which honours trusted_proxies) and falling back to the socket peer.
func clientAddr(r *http.Request) (netip.Addr, bool) {
	raw, _ := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string)
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return netip.Addr{}, false
		}
		raw = host
	}

	// Strip any zone identifier; it is meaningless for a database lookup and
	// only appears on link-local addresses, which are never in the database.
	if idx := strings.IndexByte(raw, '%'); idx >= 0 {
		raw = raw[:idx]
	}

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	// Unmap so that a v4-in-v6 peer address looks up as the IPv4 address it is.
	return addr.Unmap(), true
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
// Categories may be given as inline arguments or in the block, so both of
// these work:
//
//	@bots maat datacenter vpn
//
//	@bots {
//		maat {
//			server https://reputationdb.example.com
//			categories datacenter vpn
//		}
//	}
//
// The inline form has nowhere to put the server URL, so it is only useful when
// the server is set in the JSON config directly.
func (m *Matcher) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Iterate to merge repeated matchers into one, as the stock IP matchers do.
	for d.Next() {
		m.Categories = append(m.Categories, d.RemainingArgs()...)

		for d.NextBlock(0) {
			switch d.Val() {
			case "server":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if m.Server != "" {
					return d.Err("server specified more than once")
				}
				m.Server = d.Val()
			case "categories":
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				m.Categories = append(m.Categories, args...)
			case "api_key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if m.APIKey != "" {
					return d.Err("api_key specified more than once")
				}
				m.APIKey = d.Val()
			case "storage_path":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if m.StoragePath != "" {
					return d.Err("storage_path specified more than once")
				}
				m.StoragePath = d.Val()
			default:
				return d.Errf("unrecognized maat option %q", d.Val())
			}
		}
	}
	return nil
}

// Interface guards
var (
	_ caddy.Provisioner                 = (*Matcher)(nil)
	_ caddy.Validator                   = (*Matcher)(nil)
	_ caddy.CleanerUpper                = (*Matcher)(nil)
	_ caddyhttp.RequestMatcherWithError = (*Matcher)(nil)
	_ caddyfile.Unmarshaler             = (*Matcher)(nil)
)
