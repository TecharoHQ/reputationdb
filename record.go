package reputationdb

import (
	"slices"
	"sort"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// Category groups the kind of list an IP address was found on. It is stored on
// every [ListMembership] and surfaced as top-level booleans on the mmdb record
// so consumers can branch on it cheaply.
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
	// Category is one of the Category* constants describing what kind of list
	// this is.
	Category string `maxminddb:"category"`
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

// Categories returns the distinct, sorted set of categories this address
// belongs to.
func (r *Record) Categories() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range r.Sources {
		if !seen[s.Category] {
			seen[s.Category] = true
			out = append(out, s.Category)
		}
	}
	sort.Strings(out)
	return out
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
//	  "is_vpn":        bool,
//	  "is_datacenter": bool,
//	  "is_crawler":    bool,
//	  "is_proxy":      bool,
//	  "is_abuse":      bool,
//	  "is_tor":        bool,
//	  "categories":    []string,
//	  "providers":     []string,
//	  "sources":       [{repository, list, provider, category}, ...]
//	}
func (r *Record) DataType() mmdbtype.DataType {
	r.sort()

	categories := r.Categories()
	catSet := make(map[string]bool, len(categories))
	catSlice := make(mmdbtype.Slice, 0, len(categories))
	for _, c := range categories {
		catSet[c] = true
		catSlice = append(catSlice, mmdbtype.String(c))
	}

	providers := r.Providers()
	provSlice := make(mmdbtype.Slice, 0, len(providers))
	for _, p := range providers {
		provSlice = append(provSlice, mmdbtype.String(p))
	}

	sources := make(mmdbtype.Slice, 0, len(r.Sources))
	for _, s := range r.Sources {
		sources = append(sources, mmdbtype.Map{
			"repository": mmdbtype.String(s.Repository),
			"list":       mmdbtype.String(s.List),
			"provider":   mmdbtype.String(s.Provider),
			"category":   mmdbtype.String(s.Category),
		})
	}

	return mmdbtype.Map{
		"is_vpn":        mmdbtype.Bool(catSet[CategoryVPN]),
		"is_datacenter": mmdbtype.Bool(catSet[CategoryDatacenter]),
		"is_crawler":    mmdbtype.Bool(catSet[CategoryCrawler]),
		"is_proxy":      mmdbtype.Bool(catSet[CategoryProxy]),
		"is_abuse":      mmdbtype.Bool(catSet[CategoryAbuse]),
		"is_tor":        mmdbtype.Bool(catSet[CategoryTor]),
		"categories":    catSlice,
		"providers":     provSlice,
		"sources":       sources,
	}
}
