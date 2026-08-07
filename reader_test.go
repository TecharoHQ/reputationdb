package reputationdb

import (
	"bytes"
	"maps"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/maxmind/mmdbwriter"
)

// buildDB writes a tiny in-memory database mapping the given CIDR -> Record so
// the reader can be exercised without depending on the command's Writer.
func buildDB(t *testing.T, entries map[string]Record) *DB {
	t.Helper()

	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "Techaro-Veil-VPN",
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

	db, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDBLookup(t *testing.T) {
	vpnAndDC := Record{}
	vpnAndDC.Add(ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: CategoryByteVPN})
	vpnAndDC.Add(ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/datacentres.txt", Provider: "datacentres", Category: CategoryByteVPN})

	crawler := Record{}
	crawler.Add(ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: CategoryByteCrawler})

	db := buildDB(t, map[string]Record{
		"1.2.3.4/32":     vpnAndDC,
		"2606:4700::/32": crawler,
	})

	t.Run("ipv4 vpn and datacenter", func(t *testing.T) {
		got, found, err := db.Lookup(netip.MustParseAddr("1.2.3.4"))
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !found {
			t.Fatal("expected 1.2.3.4 to be found")
		}
		if !got.IsVPN || !got.IsDatacenter || got.IsCrawler {
			t.Errorf("flags vpn=%v dc=%v crawler=%v, want true,true,false", got.IsVPN, got.IsDatacenter, got.IsCrawler)
		}
		if !got.HasProvider("nordvpn") || !got.HasCategory(CategoryDatacenter) {
			t.Errorf("missing expected provider/category: %+v", got)
		}
		if len(got.Sources) != 2 {
			t.Errorf("sources = %d, want 2: %+v", len(got.Sources), got.Sources)
		}
	})

	t.Run("ipv6 crawler", func(t *testing.T) {
		got, found, err := db.Lookup(netip.MustParseAddr("2606:4700::1"))
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !found {
			t.Fatal("expected 2606:4700::1 to be found")
		}
		if got.IsVPN || got.IsDatacenter || !got.IsCrawler {
			t.Errorf("flags vpn=%v dc=%v crawler=%v, want false,false,true", got.IsVPN, got.IsDatacenter, got.IsCrawler)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, found, err := db.Lookup(netip.MustParseAddr("8.8.8.8"))
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if found {
			t.Error("8.8.8.8 unexpectedly found")
		}
	})
}

func TestDBNetworks(t *testing.T) {
	vpn := Record{}
	vpn.Add(ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: CategoryByteVPN})

	crawler := Record{}
	crawler.Add(ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: CategoryByteCrawler})

	db := buildDB(t, map[string]Record{
		"1.2.3.4/32":     vpn,
		"9.9.9.0/24":     crawler,
		"2606:4700::/32": crawler,
	})

	got := map[string]Result{}
	for network, err := range db.Networks() {
		if err != nil {
			t.Fatalf("Networks: %v", err)
		}
		if _, dupe := got[network.Prefix.String()]; dupe {
			// An IPv4 network lives in the ::ffff:0:0/96 subtree of an IPv6
			// database and is aliased into 2001::/32 and 2002::/16. Walking
			// those aliases would report every IPv4 prefix three times over.
			t.Fatalf("prefix %s yielded more than once", network.Prefix)
		}
		res, err := network.Result()
		if err != nil {
			t.Fatalf("Result for %s: %v", network.Prefix, err)
		}
		got[network.Prefix.String()] = res
	}

	// IPv4 prefixes must come back in their IPv4 spelling rather than as the
	// ::ffff:1.2.3.4/128 the tree actually stores, or the output is unusable as
	// an IP list.
	want := []string{"1.2.3.4/32", "9.9.9.0/24", "2606:4700::/32"}
	gotPrefixes := slices.Sorted(maps.Keys(got))
	if !slices.Equal(gotPrefixes, slices.Sorted(slices.Values(want))) {
		t.Logf("want: %v", want)
		t.Logf("got:  %v", gotPrefixes)
		t.Fatal("got wrong set of prefixes")
	}

	if res := got["1.2.3.4/32"]; !res.IsVPN || !res.HasProvider("nordvpn") {
		t.Errorf("record at 1.2.3.4/32 did not decode: %+v", res)
	}
	if res := got["2606:4700::/32"]; !res.IsCrawler {
		t.Errorf("record at 2606:4700::/32 did not decode: %+v", res)
	}
}

// TestDBNetworksSkipsEmptyDatabase checks that walking a database with nothing
// in it terminates without yielding anything, rather than reporting the whole
// address space as one record.
func TestDBNetworksSkipsEmptyDatabase(t *testing.T) {
	db := buildDB(t, nil)

	for network, err := range db.Networks() {
		if err != nil {
			t.Fatalf("Networks: %v", err)
		}
		t.Errorf("empty database yielded %s", network.Prefix)
	}
}
