package main

import (
	"bytes"
	"net/netip"
	"testing"
	"testing/fstest"
	"time"

	vpnip "github.com/TecharoHQ/reputationdb"
	"github.com/gaissmai/bart"
	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// decoded mirrors the on-disk record schema for round-trip assertions. This
// package declares it again instead of reusing the reader type. Whoever changes
// the schema must then edit both types, so this test sees every change.
type decoded struct {
	Categories vpnip.CategoryByte `maxminddb:"categories"`
	Sources    []struct {
		Repository string             `maxminddb:"repository"`
		List       string             `maxminddb:"list"`
		Provider   string             `maxminddb:"provider"`
		Category   vpnip.CategoryByte `maxminddb:"category"`
	} `maxminddb:"sources"`
}

func TestWriterRoundTrip(t *testing.T) {
	epoch := time.Date(2026, 7, 14, 22, 42, 42, 0, time.UTC)
	w, err := NewWriter(legacyDatabaseType, legacyDescription, epoch)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	vpnRec := vpnip.Record{}
	vpnRec.Add(vpnip.ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: vpnip.CategoryByteVPN})
	vpnRec.Add(vpnip.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/datacentres.txt", Provider: "datacentres", Category: vpnip.CategoryByteDatacenter})

	if err := w.Insert(netip.MustParsePrefix("1.2.3.4/32"), vpnRec); err != nil {
		t.Fatalf("Insert v4: %v", err)
	}

	crawlerRec := vpnip.Record{}
	crawlerRec.Add(vpnip.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: vpnip.CategoryByteCrawler})
	if err := w.Insert(netip.MustParsePrefix("2606:4700::/32"), crawlerRec); err != nil {
		t.Fatalf("Insert v6: %v", err)
	}

	var buf bytes.Buffer
	if _, err := w.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	db, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer db.Close()

	if db.Metadata.DatabaseType != legacyDatabaseType {
		t.Errorf("DatabaseType = %q, want %q", db.Metadata.DatabaseType, legacyDatabaseType)
	}
	if got := int64(db.Metadata.BuildEpoch); got != epoch.Unix() {
		t.Errorf("BuildEpoch = %d, want %d", got, epoch.Unix())
	}

	// IPv4 lookup: should be vpn + datacenter, two sources.
	var got decoded
	if err := db.Lookup(netip.MustParseAddr("1.2.3.4")).Decode(&got); err != nil {
		t.Fatalf("Lookup v4: %v", err)
	}
	if want := vpnip.CategoryByteVPN | vpnip.CategoryByteDatacenter; got.Categories != want {
		t.Errorf("1.2.3.4 categories = %v, want %v", got.Categories, want)
	}
	if len(got.Sources) != 2 {
		t.Errorf("1.2.3.4 sources = %d, want 2: %+v", len(got.Sources), got.Sources)
	}

	// IPv6 lookup: should be crawler only.
	var gotV6 decoded
	if err := db.Lookup(netip.MustParseAddr("2606:4700::1")).Decode(&gotV6); err != nil {
		t.Fatalf("Lookup v6: %v", err)
	}
	if gotV6.Categories != vpnip.CategoryByteCrawler {
		t.Errorf("2606:4700::1 categories = %v, want %v", gotV6.Categories, vpnip.CategoryByteCrawler)
	}

	// Unlisted address should not be found.
	if res := db.Lookup(netip.MustParseAddr("8.8.8.8")); res.Found() {
		t.Errorf("8.8.8.8 unexpectedly found")
	}
}

// TestDatacenterBuildDoesNotLeak is the guard on the free database's core
// product property: an address that is both a datacentre range and a known VPN
// exit must appear in a datacentre-only build as a datacentre address only,
// with no trace of the VPN membership that the paid database sells.
func TestDatacenterBuildDoesNotLeak(t *testing.T) {
	src := repoSource{
		name: "github.com/example/lists",
		url:  "https://github.com/example/lists",
		lists: []listSpec{
			{glob: "vpn/*.txt", category: vpnip.CategoryVPN},
			{glob: "datacentres.txt", category: vpnip.CategoryDatacenter},
		},
	}

	// 1.2.3.4 is on both lists; 5.6.7.8 is VPN-only and must vanish entirely.
	fsys := fstest.MapFS{
		"vpn/nordvpn.txt": {Data: []byte("1.2.3.4\n5.6.7.8\n")},
		"datacentres.txt": {Data: []byte("1.2.3.4/32\n9.9.9.0/24\n")},
	}

	cats, err := parseCategories([]string{vpnip.CategoryDatacenter})
	if err != nil {
		t.Fatalf("parseCategories: %v", err)
	}

	store := &bart.Table[*vpnip.Record]{}
	if _, err := collect(src, cats.selectLists(src.lists), fsys, store); err != nil {
		t.Fatalf("collect: %v", err)
	}

	epoch := time.Date(2026, 7, 14, 22, 42, 42, 0, time.UTC)
	w, err := NewWriter(cats.databaseType(), describe(cats, "v0.0.1", "2e65f968", epoch), epoch)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	for prefix, rec := range store.All() {
		if err := w.Insert(prefix, *rec); err != nil {
			t.Fatalf("Insert %s: %v", prefix, err)
		}
	}

	var buf bytes.Buffer
	if _, err := w.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	db, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer db.Close()

	if db.Metadata.DatabaseType != "Techaro-Veil-Datacenter" {
		t.Errorf("DatabaseType = %q, want %q", db.Metadata.DatabaseType, "Techaro-Veil-Datacenter")
	}

	// 1.2.3.4 is on both lists upstream, but only its datacentre membership
	// may survive into the free database.
	var got decoded
	if err := db.Lookup(netip.MustParseAddr("1.2.3.4")).Decode(&got); err != nil {
		t.Fatalf("Lookup 1.2.3.4: %v", err)
	}
	if !got.Categories.Has(vpnip.CategoryByteDatacenter) {
		t.Error("1.2.3.4 is not a datacenter address, want it to be one")
	}
	if got.Categories.Has(vpnip.CategoryByteVPN) {
		t.Error("1.2.3.4 is a VPN address: the VPN membership leaked into the free database")
	}
	if len(got.Sources) != 1 {
		t.Errorf("1.2.3.4 sources = %d, want 1: %+v", len(got.Sources), got.Sources)
	}
	for _, s := range got.Sources {
		if s.Category != vpnip.CategoryByteDatacenter {
			t.Errorf("1.2.3.4 carries a %q source: %+v", s.Category, s)
		}
	}
	if got.Categories != vpnip.CategoryByteDatacenter {
		t.Errorf("1.2.3.4 categories = %v, want %v", got.Categories, vpnip.CategoryByteDatacenter)
	}

	// A datacentre-only address reached via a CIDR range rather than a host
	// route: 9.9.9.0/24 exercises a different path through the store than
	// 1.2.3.4/32 above, and must survive the filter intact.
	var gotDC decoded
	if err := db.Lookup(netip.MustParseAddr("9.9.9.9")).Decode(&gotDC); err != nil {
		t.Fatalf("Lookup 9.9.9.9: %v", err)
	}
	if gotDC.Categories != vpnip.CategoryByteDatacenter {
		t.Errorf("9.9.9.9 categories = %v, want %v", gotDC.Categories, vpnip.CategoryByteDatacenter)
	}

	// A VPN-only address must not be in the free database at all.
	if res := db.Lookup(netip.MustParseAddr("5.6.7.8")); res.Found() {
		t.Error("5.6.7.8 is in the datacentre database, but it is only on a VPN list")
	}
}

// TestASNBuildDoesNotLeak is the AS-path analogue of
// TestDatacenterBuildDoesNotLeak. An asnSource is the only source kind that
// folds a partial subset of a single source's categories: a dual-tagged AS is
// still fetched for a datacentre-only build, but only its datacenter membership
// may be folded. Every other source kind is all-or-nothing and so cannot leak
// partially.
//
// What this test guards: the intersect -> foldAS -> writer composition honours
// the narrowed category slice it is handed. It catches a regression where
// foldAS ignored its categories parameter and read src.categories internally,
// which would fold the abuse membership into a datacentre-only build. Neither
// TestCategorySetIntersect (which exercises intersect alone) nor TestFoldAS and
// TestCollectASCache (which both pass the unfiltered src.categories straight in)
// would notice that.
//
// What this test does NOT guard: that main.go actually passes the narrowed
// slice rather than src.categories. That wiring lives in run(), which fetches
// every real source over the network and is not unit-testable.
func TestASNBuildDoesNotLeak(t *testing.T) {
	// Mirrors a real dual-tagged AS: AS136907 (huawei-cloud) is tagged both
	// datacenter and abuse.
	src := asnSource{
		asn:        64500,
		provider:   "example",
		categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse},
	}

	cats, err := parseCategories([]string{vpnip.CategoryDatacenter})
	if err != nil {
		t.Fatalf("parseCategories: %v", err)
	}

	prefix := netip.MustParsePrefix("9.9.9.0/24")
	store := &bart.Table[*vpnip.Record]{}
	foldAS(store, src, cats.intersect(src.categories), []netip.Prefix{prefix})

	epoch := time.Date(2026, 7, 14, 22, 42, 42, 0, time.UTC)
	w, err := NewWriter(cats.databaseType(), describe(cats, "v0.0.1", "2e65f968", epoch), epoch)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	for prefix, rec := range store.All() {
		if err := w.Insert(prefix, *rec); err != nil {
			t.Fatalf("Insert %s: %v", prefix, err)
		}
	}

	var buf bytes.Buffer
	if _, err := w.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	db, err := maxminddb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer db.Close()

	var got decoded
	if err := db.Lookup(netip.MustParseAddr("9.9.9.9")).Decode(&got); err != nil {
		t.Fatalf("Lookup 9.9.9.9: %v", err)
	}
	if !got.Categories.Has(vpnip.CategoryByteDatacenter) {
		t.Error("9.9.9.9 is_datacenter = false, want true")
	}
	if got.Categories.Has(vpnip.CategoryByteAbuse) {
		t.Error("9.9.9.9 is_abuse = true: the AS's abuse membership leaked into the free database")
	}
	if len(got.Sources) != 1 {
		t.Errorf("9.9.9.9 sources = %d, want 1: %+v", len(got.Sources), got.Sources)
	}
	for _, s := range got.Sources {
		if s.Category != vpnip.CategoryByteDatacenter {
			t.Errorf("9.9.9.9 carries a %q source: %+v", s.Category, s)
		}
	}
	if got.Categories != vpnip.CategoryByteDatacenter {
		t.Errorf("9.9.9.9 categories = %v, want %v", got.Categories, vpnip.CategoryByteDatacenter)
	}
}
