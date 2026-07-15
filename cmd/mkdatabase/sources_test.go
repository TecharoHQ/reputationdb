package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	vpnip "github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/web/ripeasn"
	"github.com/gaissmai/bart"
)

// getRec returns the record stored at prefix, or nil if absent. A small helper
// to keep the bart-store assertions in the tests readable.
func getRec(store *bart.Table[*vpnip.Record], prefix netip.Prefix) *vpnip.Record {
	rec, ok := store.Get(prefix)
	if !ok {
		return nil
	}
	return rec
}

func TestParseIPLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "bare ipv4 one per line",
			in:   "103.86.99.99\n103.86.96.96\n",
			want: []string{"103.86.99.99/32", "103.86.96.96/32"},
		},
		{
			name: "whole line and inline comments",
			in:   "# NordVPN DNS\n91.230.225.200 # expressvpn\n\n  \n",
			want: []string{"91.230.225.200/32"},
		},
		{
			name: "cidr ranges are masked",
			in:   "3.5.140.0/22\n2001:4860:4801:10::/64\n",
			want: []string{"3.5.140.0/22", "2001:4860:4801:10::/64"},
		},
		{
			name: "ipv6 single address",
			in:   "2606:4700:4700::1111\n",
			want: []string{"2606:4700:4700::1111/128"},
		},
		{
			name: "garbage lines are skipped",
			in:   "not-an-ip\n10.0.0.1\nhttp://example.com\n",
			want: []string{"10.0.0.1/32"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIPLines([]byte(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				want := netip.MustParsePrefix(w)
				if got[i] != want {
					t.Errorf("prefix %d: got %s, want %s", i, got[i], want)
				}
			}
		})
	}
}

func TestParseProxyURLs(t *testing.T) {
	in := "socks5://208.102.51.6:58208\nhttp://192.252.208.67:14287\n# comment\n\nsocks4://[2606:4700::1]:1080\ngarbage-line\nhttps://10.0.0.1:8080 # trailing\n"
	want := []string{
		"208.102.51.6/32",
		"192.252.208.67/32",
		"2606:4700::1/128",
		"10.0.0.1/32",
	}

	got := parseProxyURLs([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}
}

func TestCollectHTTP(t *testing.T) {
	const body = "# ipinsights.io blocklist\n203.0.113.4\n198.51.100.0/24\n203.0.113.4\nnot-an-ip\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer srv.Close()

	src := httpSource{
		name:     "ipinsights.io",
		url:      srv.URL + "/downloads/blocklist-cidr.txt",
		provider: "ipinsights",
		category: vpnip.CategoryAbuse,
	}

	store := &bart.Table[*vpnip.Record]{}
	n, err := collectHTTP(context.Background(), srv.Client(), src, "", store)
	if err != nil {
		t.Fatalf("collectHTTP: %v", err)
	}
	// Three parseable prefixes (the duplicate is counted but deduped on Add).
	if n != 3 {
		t.Errorf("collectHTTP counted %d prefixes, want 3", n)
	}

	rec := getRec(store, netip.MustParsePrefix("203.0.113.4/32"))
	if rec == nil {
		t.Fatal("expected record for 203.0.113.4/32")
	}
	if len(rec.Sources) != 1 {
		t.Fatalf("203.0.113.4/32: got %d sources, want 1 (deduped): %+v", len(rec.Sources), rec.Sources)
	}
	m := rec.Sources[0]
	if m.Repository != "ipinsights.io" || m.List != "blocklist-cidr.txt" || m.Provider != "ipinsights" || m.Category != vpnip.CategoryAbuse {
		t.Errorf("membership = %+v, want repo/list/provider/category ipinsights.io/blocklist-cidr.txt/ipinsights/abuse", m)
	}

	if rec := getRec(store, netip.MustParsePrefix("198.51.100.0/24")); rec == nil {
		t.Error("expected masked CIDR record for 198.51.100.0/24")
	}
}

func TestCollectHTTPNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := httpSource{name: "example", url: srv.URL, provider: "example", category: vpnip.CategoryAbuse}
	if _, err := collectHTTP(context.Background(), srv.Client(), src, "", &bart.Table[*vpnip.Record]{}); err == nil {
		t.Fatal("expected error on non-200 response, got nil")
	}
}

func TestCollectFile(t *testing.T) {
	const body = "# fdo list\n203.0.113.4\n198.51.100.0/24\n203.0.113.4\nnot-an-ip\n"

	listPath := filepath.Join(t.TempDir(), "ips.txt")
	if err := os.WriteFile(listPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	src := fileSource{
		name:     "fdo",
		path:     listPath,
		provider: "fdo",
		category: vpnip.CategoryAbuse,
	}

	store := &bart.Table[*vpnip.Record]{}
	n, err := collectFile(src, store)
	if err != nil {
		t.Fatalf("collectFile: %v", err)
	}
	// Three parseable prefixes (the duplicate is counted but deduped on Add).
	if n != 3 {
		t.Errorf("collectFile counted %d prefixes, want 3", n)
	}

	rec := getRec(store, netip.MustParsePrefix("203.0.113.4/32"))
	if rec == nil {
		t.Fatal("expected record for 203.0.113.4/32")
	}
	if len(rec.Sources) != 1 {
		t.Fatalf("203.0.113.4/32: got %d sources, want 1 (deduped): %+v", len(rec.Sources), rec.Sources)
	}
	m := rec.Sources[0]
	if m.Repository != "fdo" || m.List != "ips.txt" || m.Provider != "fdo" || m.Category != vpnip.CategoryAbuse {
		t.Errorf("membership = %+v, want repo/list/provider/category fdo/ips.txt/fdo/abuse", m)
	}

	if rec := getRec(store, netip.MustParsePrefix("198.51.100.0/24")); rec == nil {
		t.Error("expected masked CIDR record for 198.51.100.0/24")
	}
}

// An unsmudged pointer parses as zero IP addresses, so without an explicit check
// collectFile would report success and quietly drop the whole list.
func TestCollectFileLFSPointer(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "unsmudged pointer",
			body: "version https://git-lfs.github.com/spec/v1\n" +
				"oid sha256:f352e8a61ec22157d7b12181d71d6ab162ffc34ad0bdc7418e55b73d9b0a2013\n" +
				"size 17938592\n",
			wantErr: true,
		},
		{
			name:    "real list is unaffected",
			body:    "203.0.113.4\n198.51.100.0/24\n",
			wantErr: false,
		},
		{
			name:    "list mentioning the spec url in a comment is not a pointer",
			body:    "# see version https://git-lfs.github.com/spec/v1 for details\n203.0.113.4\n",
			wantErr: false,
		},
		{
			name:    "empty file is not a pointer",
			body:    "",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listPath := filepath.Join(t.TempDir(), "ips.txt")
			if err := os.WriteFile(listPath, []byte(tt.body), 0o644); err != nil {
				t.Fatalf("write list: %v", err)
			}

			src := fileSource{name: "sourceware", path: listPath, provider: "sourceware", category: vpnip.CategoryCrawler}

			_, err := collectFile(src, &bart.Table[*vpnip.Record]{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("collectFile() error = nil, want an error naming the unsmudged pointer")
				}
				if !strings.Contains(err.Error(), "git-lfs") {
					t.Errorf("collectFile() error = %q, want it to mention git-lfs so the fix is obvious", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("collectFile() error = %v, want nil", err)
			}
		})
	}
}

func TestCollectFileMissing(t *testing.T) {
	src := fileSource{name: "missing", path: filepath.Join(t.TempDir(), "nope.txt"), provider: "missing", category: vpnip.CategoryAbuse}
	if _, err := collectFile(src, &bart.Table[*vpnip.Record]{}); err == nil {
		t.Fatal("expected error on missing file, got nil")
	}
}

func TestCacheRepo(t *testing.T) {
	src := repoSource{
		name: "github.com/example/repo",
		url:  "https://github.com/example/repo",
		lists: []listSpec{
			{glob: "data/input/ip/*.txt", category: vpnip.CategoryVPN},
			{glob: "output/datacentres.txt", category: vpnip.CategoryDatacenter},
		},
	}

	clone := fstest.MapFS{
		"data/input/ip/nordvpn.txt": {Data: []byte("1.2.3.4\n")},
		"output/datacentres.txt":    {Data: []byte("9.9.9.0/24\n")},
		"README.md":                 {Data: []byte("not a list, must not be cached")},
	}

	repoDir := filepath.Join(t.TempDir(), "sources", filepath.FromSlash(src.name))

	if err := cacheRepo(src, clone, repoDir); err != nil {
		t.Fatalf("cacheRepo: %v", err)
	}

	// Matched files are cached at their repo-relative paths; unmatched ones are not.
	for path, want := range map[string]bool{
		"data/input/ip/nordvpn.txt": true,
		"output/datacentres.txt":    true,
		"README.md":                 false,
	} {
		_, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(path)))
		if got := err == nil; got != want {
			t.Errorf("cached %q present = %v, want %v (err=%v)", path, got, want, err)
		}
	}

	// A fresh marker means the cache is usable; aging it past cacheMaxAge invalidates it.
	if !cachedRepoFresh(repoDir) {
		t.Error("freshly cached repo reported stale")
	}
	old := time.Now().Add(-cacheMaxAge - time.Minute)
	if err := os.Chtimes(filepath.Join(repoDir, fetchedMarker), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if cachedRepoFresh(repoDir) {
		t.Error("repo with aged marker reported fresh")
	}

	// The cached tree globs and folds identically to the original clone.
	want := &bart.Table[*vpnip.Record]{}
	if _, err := collect(src, src.lists, clone, want); err != nil {
		t.Fatalf("collect (clone): %v", err)
	}
	got := &bart.Table[*vpnip.Record]{}
	if _, err := collect(src, src.lists, os.DirFS(repoDir), got); err != nil {
		t.Fatalf("collect (cache): %v", err)
	}
	for _, p := range []string{"1.2.3.4/32", "9.9.9.0/24"} {
		prefix := netip.MustParsePrefix(p)
		if getRec(got, prefix) == nil {
			t.Errorf("cached collect missing %s", p)
		}
		if getRec(want, prefix) == nil {
			t.Errorf("clone collect missing %s (test bug)", p)
		}
	}
}

func TestFetchHTTPCache(t *testing.T) {
	const body = "203.0.113.4\n198.51.100.0/24\n"

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	src := httpSource{
		name:     "github.com/example/list",
		url:      srv.URL + "/lists/inbound.txt",
		provider: "example",
		category: vpnip.CategoryAbuse,
	}
	cachePath := filepath.Join(cacheDir, "sources", "github.com", "example", "list", "inbound.txt")

	// First fetch hits the network and populates the cache.
	data, err := fetchHTTP(context.Background(), srv.Client(), src, cacheDir)
	if err != nil {
		t.Fatalf("fetchHTTP (cold): %v", err)
	}
	if string(data) != body {
		t.Errorf("cold body = %q, want %q", data, body)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("after cold fetch, server hits = %d, want 1", got)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected cache file at %s: %v", cachePath, err)
	}

	// Second fetch is served from the fresh cache without touching the network.
	data, err = fetchHTTP(context.Background(), srv.Client(), src, cacheDir)
	if err != nil {
		t.Fatalf("fetchHTTP (warm): %v", err)
	}
	if string(data) != body {
		t.Errorf("warm body = %q, want %q", data, body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("after warm fetch, server hits = %d, want 1 (cache should serve it)", got)
	}

	// Age the cache past cacheMaxAge: the next fetch must refresh from the server.
	old := time.Now().Add(-cacheMaxAge - time.Minute)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, err := fetchHTTP(context.Background(), srv.Client(), src, cacheDir); err != nil {
		t.Fatalf("fetchHTTP (stale): %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("after stale fetch, server hits = %d, want 2 (stale cache should refetch)", got)
	}
}

func TestParseColonHost(t *testing.T) {
	in := "190.97.238.85:999\n200.34.227.28:8080:Brazil\n# comment\n\n  \nnot-an-ip:80\n140.82.59.192:1080\n"
	want := []string{
		"190.97.238.85/32",
		"200.34.227.28/32",
		"140.82.59.192/32",
	}

	got := parseColonHost([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}
}

func TestMatchFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"http.txt":                   {Data: []byte("a")},
		"socks5.txt":                 {Data: []byte("b")},
		"Countries/http/Brazil.txt":  {Data: []byte("c")},
		"Countries/socks5/Japan.txt": {Data: []byte("d")},
		"README.md":                  {Data: []byte("e")},
	}

	// Plain glob: top-level only.
	got, err := matchFiles(fsys, "*.txt")
	if err != nil {
		t.Fatalf("matchFiles plain: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("plain *.txt = %v, want 2 top-level files", got)
	}

	// Recursive: every .txt at any depth.
	got, err = matchFiles(fsys, "**/*.txt")
	if err != nil {
		t.Fatalf("matchFiles recursive: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("**/*.txt = %v, want 4 .txt files (README.md excluded)", got)
	}

	// Recursive under a prefix dir.
	got, err = matchFiles(fsys, "Countries/**/*.txt")
	if err != nil {
		t.Fatalf("matchFiles prefixed: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Countries/**/*.txt = %v, want 2 files under Countries/", got)
	}
}

func TestParseCSVFirstField(t *testing.T) {
	in := "171.25.193.25/32,\"TOR Exit node\"\n80.67.167.81,\"TOR Exit node\"\n2001:db8::/32,desc\n# comment line\n\nnot-an-ip,whatever\n"
	want := []string{
		"171.25.193.25/32",
		"80.67.167.81/32",
		"2001:db8::/32",
	}

	got := parseCSVFirstField([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}
}

func TestParseGoogleStyleJSON(t *testing.T) {
	in := `{
		"creationTime": "2026-06-21T02:28:21.000000",
		"prefixes": [
			{"ipv4Prefix": "1.2.3.0/24"},
			{"ipv4Prefix": "8.8.8.8/32"},
			{"ipv6Prefix": "2001:db8:1234::/48"},
			{"ipv4Prefix": "not-a-cidr"},
			{"other": "ignored"}
		]
	}`
	want := []string{
		"1.2.3.0/24",
		"8.8.8.8/32",
		"2001:db8:1234::/48",
	}

	got := parseGoogleStyleJSON([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseGoogleStyleJSON([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestParseAWSJSON(t *testing.T) {
	in := `{
		"syncToken": "1700000000",
		"createDate": "2026-06-21-00-00-00",
		"prefixes": [
			{"ip_prefix": "3.5.140.0/22", "region": "ap-northeast-2", "service": "AMAZON"},
			{"ip_prefix": "13.34.37.64/27", "region": "us-east-1", "service": "EC2"},
			{"ip_prefix": "not-a-cidr", "region": "x", "service": "AMAZON"}
		],
		"ipv6_prefixes": [
			{"ipv6_prefix": "2600:1f01:4860::/47", "region": "us-east-1", "service": "AMAZON"},
			{"ipv6_prefix": "garbage", "region": "x", "service": "AMAZON"}
		]
	}`
	want := []string{
		"3.5.140.0/22",
		"13.34.37.64/27",
		"2600:1f01:4860::/47",
	}

	got := parseAWSJSON([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseAWSJSON([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestParseOracleJSON(t *testing.T) {
	in := `{
		"last_updated_timestamp": "2026-05-25T08:40:08.970229",
		"regions": [
			{"region": "us-ashburn-1", "cidrs": [
				{"cidr": "1.2.3.0/24", "tags": ["OCI"]},
				{"cidr": "5.6.0.0/16", "tags": ["OCI"]}
			]},
			{"region": "eu-frankfurt-1", "cidrs": [
				{"cidr": "2001:db8::/32", "tags": ["OSN"]},
				{"cidr": "not-a-cidr", "tags": ["OCI"]}
			]}
		]
	}`
	want := []string{
		"1.2.3.0/24",
		"5.6.0.0/16",
		"2001:db8::/32",
	}

	got := parseOracleJSON([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseOracleJSON([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestParseAzureJSON(t *testing.T) {
	in := `{
		"changeNumber": 363,
		"cloud": "Public",
		"values": [
			{"name": "ActionGroup", "properties": {"addressPrefixes": [
				"4.145.74.52/30",
				"4.149.254.68/30"
			]}},
			{"name": "AzureCloud", "properties": {"addressPrefixes": [
				"2001:db8::/32",
				"not-a-cidr"
			]}}
		]
	}`
	want := []string{
		"4.145.74.52/30",
		"4.149.254.68/30",
		"2001:db8::/32",
	}

	got := parseAzureJSON([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseAzureJSON([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestParseGitHubActions(t *testing.T) {
	in := `[
		{"ip_address": "4.148.0.0/16", "ip_type": "IPv4", "service": "actions"},
		{"ip_address": "4.208.26.196/32", "ip_type": "IPv4", "service": "packages"},
		{"ip_address": "4.208.26.197/32", "ip_type": "IPv4", "service": "git"},
		{"ip_address": "20.1.2.0/24", "ip_type": "IPv4", "service": "actions"},
		{"ip_address": "not-a-cidr", "ip_type": "IPv4", "service": "actions"}
	]`
	want := []string{
		"4.148.0.0/16",
		"20.1.2.0/24",
	}

	got := parseGitHubActions([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseGitHubActions([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestParseIPAddressJSON(t *testing.T) {
	in := `[
		{"ip_address": "23.235.32.0/20", "ip_type": "IPv4"},
		{"ip_address": "5.10.96.0/19", "ip_type": "IPv4", "service": "ibmcloud-as36351", "region": "global"},
		{"ip_address": "2620:11a:a000::/40", "ip_type": "IPv6"},
		{"ip_address": "not-a-cidr", "ip_type": "IPv4"}
	]`
	want := []string{
		"23.235.32.0/20",
		"5.10.96.0/19",
		"2620:11a:a000::/40",
	}

	got := parseIPAddressJSON([]byte(in))
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}

	if got := parseIPAddressJSON([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestRegionGroupedParser(t *testing.T) {
	in := `[
		{"ip_address": "8.34.208.0/23", "ip_type": "IPv4", "service": "Google Cloud", "region": "europe-west1"},
		{"ip_address": "8.34.210.0/24", "ip_type": "IPv4", "service": "Google Cloud", "region": "us-central1"},
		{"ip_address": "8.34.211.0/24", "ip_type": "IPv4", "service": "Google Cloud", "region": "europe-west1"},
		{"ip_address": "8.228.224.0/20", "ip_type": "IPv4", "service": "Google Cloud", "region": ""},
		{"ip_address": "not-a-cidr", "ip_type": "IPv4", "service": "Google Cloud", "region": "us-central1"}
	]`

	got := regionGroupedParser("googlecloud")([]byte(in))
	want := map[string][]string{
		"googlecloud-europe-west1": {"8.34.208.0/23", "8.34.211.0/24"},
		"googlecloud-us-central1":  {"8.34.210.0/24"},
		"googlecloud-global":       {"8.228.224.0/20"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d region groups %v, want %d", len(got), got, len(want))
	}
	for provider, prefixes := range want {
		g := got[provider]
		if len(g) != len(prefixes) {
			t.Fatalf("%s: got %d prefixes %v, want %d %v", provider, len(g), g, len(prefixes), prefixes)
		}
		for i, w := range prefixes {
			if p := netip.MustParsePrefix(w); g[i] != p {
				t.Errorf("%s prefix %d: got %s, want %s", provider, i, g[i], p)
			}
		}
	}

	if got := regionGroupedParser("googlecloud")([]byte("not json")); got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestCollectHTTPGrouped(t *testing.T) {
	const body = `[
		{"ip_address": "8.34.208.0/23", "ip_type": "IPv4", "service": "Google Cloud", "region": "europe-west1"},
		{"ip_address": "8.34.210.0/24", "ip_type": "IPv4", "service": "Google Cloud", "region": "us-central1"}
	]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	defer srv.Close()

	src := httpSource{
		name:         "github.com/rezmoss/cloud-provider-ip-addresses",
		url:          srv.URL + "/googlecloud/googlecloud_ips.json",
		category:     vpnip.CategoryDatacenter,
		parseGrouped: regionGroupedParser("googlecloud"),
	}

	store := &bart.Table[*vpnip.Record]{}
	n, err := collectHTTP(context.Background(), srv.Client(), src, "", store)
	if err != nil {
		t.Fatalf("collectHTTP: %v", err)
	}
	if n != 2 {
		t.Errorf("collectHTTP counted %d prefixes, want 2", n)
	}

	rec := getRec(store, netip.MustParsePrefix("8.34.210.0/24"))
	if rec == nil {
		t.Fatal("expected record for 8.34.210.0/24")
	}
	if len(rec.Sources) != 1 {
		t.Fatalf("8.34.210.0/24: got %d sources, want 1: %+v", len(rec.Sources), rec.Sources)
	}
	m := rec.Sources[0]
	if m.Provider != "googlecloud-us-central1" || m.List != "googlecloud_ips.json" || m.Category != vpnip.CategoryDatacenter {
		t.Errorf("membership = %+v, want provider googlecloud-us-central1 / list googlecloud_ips.json / category datacenter", m)
	}
}

func TestMergeContained(t *testing.T) {
	store := &bart.Table[*vpnip.Record]{}
	fold(store, []netip.Prefix{netip.MustParsePrefix("1.2.0.0/16")},
		vpnip.ListMembership{Repository: "r16", List: "l16", Provider: "p16", Category: vpnip.CategoryAbuse})
	fold(store, []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")},
		vpnip.ListMembership{Repository: "r24", List: "l24", Provider: "p24", Category: vpnip.CategoryAbuse})
	fold(store, []netip.Prefix{netip.MustParsePrefix("1.2.3.4/32")},
		vpnip.ListMembership{Repository: "r32", List: "l32", Provider: "p32", Category: vpnip.CategoryVPN})
	// 9.9.9.9/32 is covered by nothing and must stay untouched.
	fold(store, []netip.Prefix{netip.MustParsePrefix("9.9.9.9/32")},
		vpnip.ListMembership{Repository: "r9", List: "l9", Provider: "p9", Category: vpnip.CategoryAbuse})

	mergeContained(store)

	// /32 inherits the /24 and /16 that contain it: 3 sources, both categories.
	rec := getRec(store, netip.MustParsePrefix("1.2.3.4/32"))
	if len(rec.Sources) != 3 {
		t.Fatalf("/32 sources = %d, want 3: %+v", len(rec.Sources), rec.Sources)
	}
	if cats := rec.Categories(); len(cats) != 2 || cats[0] != vpnip.CategoryAbuse || cats[1] != vpnip.CategoryVPN {
		t.Errorf("/32 categories = %v, want [abuse vpn]", cats)
	}

	// /24 inherits only the /16; it must NOT gain the narrower /32's membership.
	rec = getRec(store, netip.MustParsePrefix("1.2.3.0/24"))
	if len(rec.Sources) != 2 {
		t.Fatalf("/24 sources = %d, want 2 (inherits /16 only): %+v", len(rec.Sources), rec.Sources)
	}
	for _, s := range rec.Sources {
		if s.Provider == "p32" {
			t.Errorf("/24 must not inherit narrower /32 membership: %+v", rec.Sources)
		}
	}

	// /16 contains others but is contained by none: untouched.
	if rec := getRec(store, netip.MustParsePrefix("1.2.0.0/16")); len(rec.Sources) != 1 {
		t.Errorf("/16 sources = %d, want 1: %+v", len(rec.Sources), rec.Sources)
	}

	// Unrelated /32 keeps its single membership.
	if rec := getRec(store, netip.MustParsePrefix("9.9.9.9/32")); len(rec.Sources) != 1 {
		t.Errorf("9.9.9.9/32 sources = %d, want 1: %+v", len(rec.Sources), rec.Sources)
	}
}

func TestDeriveProvider(t *testing.T) {
	tests := map[string]string{
		"data/input/ip/nordvpn.txt":     "nordvpn",
		"data/input/ip/nordvpn_api.txt": "nordvpn",
		"nordvpn-ips.txt":               "nordvpn",
		"windscribevpn-ips.txt":         "windscribevpn",
		"tunnelbear_ips.txt":            "tunnelbear",
		"output/datacentres.txt":        "datacentres",
		"output/crawlers.txt":           "crawlers",
	}

	for in, want := range tests {
		if got := deriveProvider(in); got != want {
			t.Errorf("deriveProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAsnPrefixes(t *testing.T) {
	// A trimmed RIPEstat announced-prefixes response.
	const body = `{
		"data_call_name": "announced-prefixes",
		"data": {
			"prefixes": [
				{"prefix": "1.2.3.0/24"},
				{"prefix": "5.6.0.0/16"},
				{"prefix": "2001:db8::/32"}
			]
		},
		"status": "ok"
	}`

	var resp ripeasn.Response
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{"1.2.3.0/24", "5.6.0.0/16", "2001:db8::/32"}
	got := asnPrefixes(&resp)
	if len(got) != len(want) {
		t.Fatalf("got %d prefixes %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if p := netip.MustParsePrefix(w); got[i] != p {
			t.Errorf("prefix %d: got %s, want %s", i, got[i], p)
		}
	}
}

func TestFoldAS(t *testing.T) {
	src := asnSource{
		asn:        136907,
		provider:   "huawei-cloud",
		categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse},
	}
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("1.2.3.0/24"),
		netip.MustParsePrefix("5.6.0.0/16"),
	}

	store := &bart.Table[*vpnip.Record]{}
	// Two prefixes folded under two categories => four (prefix, category) pairs.
	if n := foldAS(store, src, src.categories, prefixes); n != 4 {
		t.Errorf("foldAS counted %d pairs, want 4", n)
	}

	rec := getRec(store, netip.MustParsePrefix("1.2.3.0/24"))
	if rec == nil {
		t.Fatal("expected record for 1.2.3.0/24")
	}
	// One membership per category, both under the same AS list and provider.
	if len(rec.Sources) != 2 {
		t.Fatalf("1.2.3.0/24: got %d sources, want 2: %+v", len(rec.Sources), rec.Sources)
	}
	for _, m := range rec.Sources {
		if m.Repository != "stat.ripe.net" || m.List != "AS136907" || m.Provider != "huawei-cloud" {
			t.Errorf("membership = %+v, want repo stat.ripe.net / list AS136907 / provider huawei-cloud", m)
		}
	}
	if cats := rec.Categories(); len(cats) != 2 || cats[0] != vpnip.CategoryAbuse || cats[1] != vpnip.CategoryDatacenter {
		t.Errorf("categories = %v, want [abuse datacenter]", cats)
	}
}

func TestCollectASCache(t *testing.T) {
	cacheDir := t.TempDir()
	src := asnSource{
		asn:        64500,
		provider:   "example",
		categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse},
	}
	cachePath := filepath.Join(cacheDir, "sources", "stat.ripe.net", "AS64500.json")

	// Seed a fresh cache so collectAS serves from disk and never touches the
	// network (RIPEstat is hardcoded in ripeasn.Fetch and unreachable in tests).
	if err := writeCache(cachePath, []byte(`["1.2.3.0/24","5.6.0.0/16"]`)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	store := &bart.Table[*vpnip.Record]{}
	n, err := collectAS(context.Background(), http.DefaultClient, src, src.categories, cacheDir, store)
	if err != nil {
		t.Fatalf("collectAS: %v", err)
	}
	// Two prefixes folded under two categories => four (prefix, category) pairs.
	if n != 4 {
		t.Errorf("collectAS counted %d pairs, want 4", n)
	}

	rec := getRec(store, netip.MustParsePrefix("1.2.3.0/24"))
	if rec == nil {
		t.Fatal("expected record for 1.2.3.0/24 from cache")
	}
	if cats := rec.Categories(); len(cats) != 2 || cats[0] != vpnip.CategoryAbuse || cats[1] != vpnip.CategoryDatacenter {
		t.Errorf("categories = %v, want [abuse datacenter]", cats)
	}

	// A stale cache (aged past cacheMaxAge) must not be served; with no network
	// the refetch fails, so an error here proves the stale copy was rejected.
	old := time.Now().Add(-cacheMaxAge - time.Minute)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, ok := readFreshCache(cachePath); ok {
		t.Error("aged cache reported fresh")
	}
}

func TestCollect(t *testing.T) {
	src := repoSource{
		name: "github.com/example/repo",
		url:  "https://github.com/example/repo",
		lists: []listSpec{
			{glob: "data/input/ip/*.txt", category: vpnip.CategoryVPN},
			{glob: "output/datacentres.txt", category: vpnip.CategoryDatacenter},
		},
	}

	fsys := fstest.MapFS{
		"data/input/ip/nordvpn.txt":       {Data: []byte("1.2.3.4\n5.6.7.8\n")},
		"data/input/ip/protonvpn_api.txt": {Data: []byte("5.6.7.8\n")},
		"output/datacentres.txt":          {Data: []byte("1.2.3.4/32\n9.9.9.0/24\n")},
		"README.md":                       {Data: []byte("ignored")},
	}

	store := &bart.Table[*vpnip.Record]{}
	n, err := collect(src, src.lists, fsys, store)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if n != 5 {
		t.Errorf("collect counted %d memberships, want 5", n)
	}

	// 1.2.3.4/32 appears on nordvpn (vpn) and datacentres (datacenter).
	rec := getRec(store, netip.MustParsePrefix("1.2.3.4/32"))
	if rec == nil {
		t.Fatal("expected record for 1.2.3.4/32")
	}
	if len(rec.Sources) != 2 {
		t.Fatalf("1.2.3.4/32: got %d sources, want 2: %+v", len(rec.Sources), rec.Sources)
	}
	if cats := rec.Categories(); len(cats) != 2 || cats[0] != vpnip.CategoryDatacenter || cats[1] != vpnip.CategoryVPN {
		t.Errorf("1.2.3.4/32 categories = %v, want [datacenter vpn]", cats)
	}

	// 5.6.7.8/32 appears on two vpn providers; providers should be deduped/sorted.
	rec = getRec(store, netip.MustParsePrefix("5.6.7.8/32"))
	if rec == nil {
		t.Fatal("expected record for 5.6.7.8/32")
	}
	if provs := rec.Providers(); len(provs) != 2 || provs[0] != "nordvpn" || provs[1] != "protonvpn" {
		t.Errorf("5.6.7.8/32 providers = %v, want [nordvpn protonvpn]", provs)
	}
}
