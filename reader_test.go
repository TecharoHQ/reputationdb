package reputationdb

import (
	"bytes"
	"net"
	"net/netip"
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
	vpnAndDC.Add(ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: CategoryVPN})
	vpnAndDC.Add(ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/datacentres.txt", Provider: "datacentres", Category: CategoryDatacenter})

	crawler := Record{}
	crawler.Add(ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: CategoryCrawler})

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
