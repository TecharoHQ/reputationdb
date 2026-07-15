package main

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	vpnip "github.com/TecharoHQ/reputationdb"
	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// decoded mirrors the on-disk record schema for round-trip assertions.
type decoded struct {
	IsVPN        bool     `maxminddb:"is_vpn"`
	IsDatacenter bool     `maxminddb:"is_datacenter"`
	IsCrawler    bool     `maxminddb:"is_crawler"`
	IsProxy      bool     `maxminddb:"is_proxy"`
	IsAbuse      bool     `maxminddb:"is_abuse"`
	IsTor        bool     `maxminddb:"is_tor"`
	Categories   []string `maxminddb:"categories"`
	Providers    []string `maxminddb:"providers"`
	Sources      []struct {
		Repository string `maxminddb:"repository"`
		List       string `maxminddb:"list"`
		Provider   string `maxminddb:"provider"`
		Category   string `maxminddb:"category"`
	} `maxminddb:"sources"`
}

func TestWriterRoundTrip(t *testing.T) {
	epoch := time.Date(2026, 7, 14, 22, 42, 42, 0, time.UTC)
	w, err := NewWriter(legacyDatabaseType, legacyDescription, epoch)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	vpnRec := vpnip.Record{}
	vpnRec.Add(vpnip.ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: vpnip.CategoryVPN})
	vpnRec.Add(vpnip.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/datacentres.txt", Provider: "datacentres", Category: vpnip.CategoryDatacenter})

	if err := w.Insert(netip.MustParsePrefix("1.2.3.4/32"), vpnRec); err != nil {
		t.Fatalf("Insert v4: %v", err)
	}

	crawlerRec := vpnip.Record{}
	crawlerRec.Add(vpnip.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: vpnip.CategoryCrawler})
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
	if !got.IsVPN || !got.IsDatacenter || got.IsCrawler {
		t.Errorf("1.2.3.4 flags: vpn=%v datacenter=%v crawler=%v, want true,true,false", got.IsVPN, got.IsDatacenter, got.IsCrawler)
	}
	if len(got.Sources) != 2 {
		t.Errorf("1.2.3.4 sources = %d, want 2: %+v", len(got.Sources), got.Sources)
	}
	if len(got.Categories) != 2 {
		t.Errorf("1.2.3.4 categories = %v, want 2", got.Categories)
	}

	// IPv6 lookup: should be crawler only.
	var gotV6 decoded
	if err := db.Lookup(netip.MustParseAddr("2606:4700::1")).Decode(&gotV6); err != nil {
		t.Fatalf("Lookup v6: %v", err)
	}
	if gotV6.IsVPN || gotV6.IsDatacenter || !gotV6.IsCrawler {
		t.Errorf("2606:4700::1 flags: vpn=%v datacenter=%v crawler=%v, want false,false,true", gotV6.IsVPN, gotV6.IsDatacenter, gotV6.IsCrawler)
	}

	// Unlisted address should not be found.
	if res := db.Lookup(netip.MustParseAddr("8.8.8.8")); res.Found() {
		t.Errorf("8.8.8.8 unexpectedly found")
	}
}
