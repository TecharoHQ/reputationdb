package reputationdb

import (
	"slices"
	"sort"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// The name of each category, as a person writes it on a command line or in a
// configuration file.
//
// These names live at the edges of the system only. Nothing stores them.
// [FromCategories] turns a set of them into the [CategoryByte] that the rest of
// the code uses.
const (
	CategoryVPN        = "vpn"
	CategoryDatacenter = "datacenter"
	CategoryCrawler    = "crawler"
	CategoryProxy      = "proxy"
	CategoryAbuse      = "abuse"
	CategoryTor        = "tor"
)

// ListMembership is the high-level metadata describing one list that an IP
// address appeared on. A single address can belong to many lists across many
// source repositories, so each [Record] holds a slice of these.
type ListMembership struct {
	// Repository is the canonical name of the source repo, e.g.
	// "github.com/az0/vpn_ip".
	Repository string `maxminddb:"repository"`
	// List is the path of the file within the repository that the address was
	// read from, e.g. "data/input/ip/nordvpn.txt".
	List string `maxminddb:"list"`
	// Provider is the VPN/service name derived from the list file name, e.g.
	// "nordvpn", "tunnelbear", or "datacentres".
	Provider string `maxminddb:"provider"`
	// Category describes what kind of list this is. A single list is normally
	// exactly one category, but the type is a bitmask, so it can be more.
	Category CategoryByte `maxminddb:"category"`
}

// Record is the high-level metadata stored for a single IP address (or CIDR
// range). It is the in-memory representation that gets converted into the raw
// mmdb [mmdbtype.DataType] written to disk.
type Record struct {
	// Sources lists every list/file the address was found on.
	Sources []ListMembership
}

// Add appends a [ListMembership] to the record, skipping exact duplicates so
// the same (repository, list, provider) tuple is never recorded twice.
func (r *Record) Add(m ListMembership) {
	if slices.Contains(r.Sources, m) {
		return
	}
	r.Sources = append(r.Sources, m)
}

// sort orders the record's sources deterministically so that repeated builds of
// the same input produce byte-identical output.
func (r *Record) sort() {
	sort.Slice(r.Sources, func(i, j int) bool {
		a, b := r.Sources[i], r.Sources[j]
		switch {
		case a.Repository != b.Repository:
			return a.Repository < b.Repository
		case a.List != b.List:
			return a.List < b.List
		default:
			return a.Provider < b.Provider
		}
	})
}

// Categories returns the union of the categories of every source in the record.
func (r *Record) Categories() CategoryByte {
	var result CategoryByte
	for _, s := range r.Sources {
		result |= s.Category
	}
	return result
}

// Providers returns the distinct, sorted set of providers this address belongs
// to.
func (r *Record) Providers() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range r.Sources {
		if !seen[s.Provider] {
			seen[s.Provider] = true
			out = append(out, s.Provider)
		}
	}
	sort.Strings(out)
	return out
}

// DataType converts the high-level record into the raw mmdb value that gets
// inserted into the search tree. The schema is:
//
//	{
//	  "categories": uint16,
//	  "sources":    [{repository, list, provider, category}, ...]
//	}
//
// Both category fields are [CategoryByte] bitmasks, not strings. See
// [CategoryByte] for why.
//
// The top-level mask is the union of the masks of the sources. A caller that
// only needs to know what an address is does not have to read the sources.
//
// There is no top-level providers list. Every provider name is already on a
// source, and the writer stores each distinct source once for the whole
// database. A second list of the same names costs one array for each record,
// which no amount of deduplication removes.
func (r *Record) DataType() mmdbtype.DataType {
	r.sort()

	sources := make(mmdbtype.Slice, 0, len(r.Sources))
	for _, s := range r.Sources {
		sources = append(sources, mmdbtype.Map{
			"repository": mmdbtype.String(s.Repository),
			"list":       mmdbtype.String(s.List),
			"provider":   mmdbtype.String(s.Provider),
			"category":   mmdbtype.Uint16(s.Category),
		})
	}

	return mmdbtype.Map{
		"categories": mmdbtype.Uint16(r.Categories()),
		"sources":    sources,
	}
}
