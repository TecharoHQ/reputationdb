package main

import (
	"bytes"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/TecharoHQ/reputationdb"
	"github.com/maxmind/mmdbwriter"
)

// buildDB writes a tiny in-memory database mapping the given CIDR -> Record so
// that dump can be exercised against a real mmdb file rather than a stand-in.
func buildDB(t *testing.T, entries map[string]reputationdb.Record) *reputationdb.DB {
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

	db, err := reputationdb.OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// record builds a Record with one membership per (provider, category) pair.
func record(t *testing.T, memberships ...reputationdb.ListMembership) reputationdb.Record {
	t.Helper()

	var rec reputationdb.Record
	for _, m := range memberships {
		rec.Add(m)
	}
	return rec
}

// testDB is the fixture every dump test walks: one VPN address, one datacenter
// range, one address that is both, and one crawler range.
func testDB(t *testing.T) *reputationdb.DB {
	t.Helper()

	nordvpn := reputationdb.ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "nordvpn-ips.txt", Provider: "nordvpn", Category: reputationdb.CategoryByteVPN}
	mullvad := reputationdb.ListMembership{Repository: "github.com/coocoobau/vpn-ip-lists", List: "mullvad-ips.txt", Provider: "mullvad", Category: reputationdb.CategoryByteVPN}
	datacentre := reputationdb.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/datacentres.txt", Provider: "datacentres", Category: reputationdb.CategoryByteDatacenter}
	crawlers := reputationdb.ListMembership{Repository: "github.com/hexydec/ip-ranges", List: "output/crawlers.txt", Provider: "crawlers", Category: reputationdb.CategoryByteCrawler}

	return buildDB(t, map[string]reputationdb.Record{
		"1.2.3.4/32":     record(t, nordvpn),
		"9.9.9.0/24":     record(t, datacentre),
		"45.32.0.0/16":   record(t, mullvad, datacentre),
		"2606:4700::/32": record(t, crawlers),
	})
}

func TestDump(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		filter filter
		want   []string
		stats  stats
	}{
		{
			// No filter means no record ever has to be decoded, which is what
			// makes an unfiltered dump several times faster than a filtered one.
			name:   "no filter emits every prefix without decoding",
			filter: filter{},
			want:   []string{"1.2.3.4/32", "9.9.9.0/24", "45.32.0.0/16", "2606:4700::/32"},
			stats:  stats{prefixes: 4, written: 4},
		},
		{
			name:   "single category",
			filter: filter{categories: reputationdb.CategoryByteVPN},
			want:   []string{"1.2.3.4/32", "45.32.0.0/16"},
			stats:  stats{prefixes: 4, decoded: 4, written: 2},
		},
		{
			// Two values of the same flag widen the selection: an address on
			// either list is wanted.
			name:   "two categories are a union",
			filter: filter{categories: reputationdb.CategoryByteVPN | reputationdb.CategoryByteCrawler},
			want:   []string{"1.2.3.4/32", "45.32.0.0/16", "2606:4700::/32"},
			stats:  stats{prefixes: 4, decoded: 4, written: 3},
		},
		{
			name:   "single provider",
			filter: filter{providers: []string{"datacentres"}},
			want:   []string{"9.9.9.0/24", "45.32.0.0/16"},
			stats:  stats{prefixes: 4, decoded: 4, written: 2},
		},
		{
			// Two different flags narrow the selection instead: each one is a
			// further condition the record has to meet.
			name:   "category and provider must both match",
			filter: filter{categories: reputationdb.CategoryByteDatacenter, providers: []string{"mullvad"}},
			want:   []string{"45.32.0.0/16"},
			stats:  stats{prefixes: 4, decoded: 4, written: 1},
		},
		{
			name:   "filter matching nothing emits nothing",
			filter: filter{providers: []string{"nosuchprovider"}},
			want:   nil,
			stats:  stats{prefixes: 4, decoded: 4},
		},
		{
			name:   "category present in no record",
			filter: filter{categories: reputationdb.CategoryByteTor},
			want:   nil,
			stats:  stats{prefixes: 4, decoded: 4},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			got, err := dump(&out, testDB(t), tt.filter)
			if err != nil {
				t.Fatalf("dump: %v", err)
			}

			if got != tt.stats {
				t.Logf("want: %+v", tt.stats)
				t.Logf("got:  %+v", got)
				t.Error("got wrong stats")
			}

			// The tree decides the order prefixes come back in, so compare as a
			// set; what matters is which prefixes were emitted.
			gotLines := strings.Fields(out.String())
			if len(gotLines) != len(tt.want) {
				t.Logf("want: %v", tt.want)
				t.Logf("got:  %v", gotLines)
				t.Fatal("got wrong number of prefixes")
			}
			for _, want := range tt.want {
				if !slices.Contains(gotLines, want) {
					t.Logf("want: %v", tt.want)
					t.Logf("got:  %v", gotLines)
					t.Errorf("missing prefix %s", want)
				}
			}
		})
	}
}

// TestDumpWritesOnePrefixPerLine pins the output format: an IP list is only
// useful to the tools that consume it if every line is exactly one prefix.
func TestDumpWritesOnePrefixPerLine(t *testing.T) {
	t.Parallel()

	db := buildDB(t, map[string]reputationdb.Record{
		"1.2.3.4/32": record(t, reputationdb.ListMembership{Repository: "r", List: "l", Provider: "p", Category: reputationdb.CategoryByteVPN}),
	})

	var out bytes.Buffer
	if _, err := dump(&out, db, filter{}); err != nil {
		t.Fatalf("dump: %v", err)
	}

	if want := "1.2.3.4/32\n"; out.String() != want {
		t.Logf("want: %q", want)
		t.Logf("got:  %q", out.String())
		t.Error("got wrong output")
	}
}

func TestParseFilter(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		categories  []string
		providers   []string
		want        filter
		errContains string
	}{
		{
			name: "no values is an empty filter",
			want: filter{},
		},
		{
			name:       "known categories are kept",
			categories: []string{reputationdb.CategoryVPN, reputationdb.CategoryTor},
			want:       filter{categories: reputationdb.CategoryByteVPN | reputationdb.CategoryByteTor},
		},
		{
			// A typo'd category would otherwise dump an empty file and look
			// like the database simply held nothing of that kind.
			name:        "unknown category is rejected",
			categories:  []string{"datacentre"},
			errContains: `unknown category "datacentre"`,
		},
		{
			name:      "providers are taken as given",
			providers: []string{"nordvpn"},
			want:      filter{providers: []string{"nordvpn"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseFilter(tt.categories, tt.providers)
			if tt.errContains != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got none", tt.errContains)
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Logf("want error containing: %q", tt.errContains)
					t.Logf("got error:             %v", err)
					t.Error("got wrong error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFilter: %v", err)
			}
			if !filtersEqual(got, tt.want) {
				t.Logf("want: %+v", tt.want)
				t.Logf("got:  %+v", got)
				t.Error("got wrong filter")
			}
		})
	}
}

// filtersEqual compares two filters field by field, treating a nil slice and an
// empty one as the same thing.
func filtersEqual(a, b filter) bool {
	if a.categories != b.categories || len(a.providers) != len(b.providers) {
		return false
	}
	for i := range a.providers {
		if a.providers[i] != b.providers[i] {
			return false
		}
	}
	return true
}
