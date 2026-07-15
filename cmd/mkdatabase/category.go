package main

import (
	"fmt"
	"slices"
	"strings"

	vpnip "github.com/TecharoHQ/reputationdb"
)

// allCategories is every category a build may select, in sorted order. The
// order is load-bearing: derived names (see databaseType) join these in slice
// order, so a stable order keeps a given selection's metadata stable too.
var allCategories = []string{
	vpnip.CategoryAbuse,
	vpnip.CategoryCrawler,
	vpnip.CategoryDatacenter,
	vpnip.CategoryProxy,
	vpnip.CategoryTor,
	vpnip.CategoryVPN,
}

// categorySet is the set of categories a build collects. Sources tagged only
// with unselected categories are never fetched, and unselected memberships are
// never folded, so a filtered build's records carry selected categories and
// nothing else. That is what keeps the free datacentre database from leaking
// the paid database's VPN/abuse/crawler signal.
type categorySet map[string]bool

// parseCategories turns repeated --category values into a categorySet. No
// values selects every category, reproducing an unfiltered build. An
// unrecognised value is an error rather than a silent no-op: a typo'd
// --category=datacentre must fail loudly instead of building an empty database.
func parseCategories(flags []string) (categorySet, error) {
	if len(flags) == 0 {
		cs := make(categorySet, len(allCategories))
		for _, c := range allCategories {
			cs[c] = true
		}
		return cs, nil
	}

	cs := make(categorySet, len(flags))
	for _, f := range flags {
		if !slices.Contains(allCategories, f) {
			return nil, fmt.Errorf("unknown category %q: valid categories are %s", f, strings.Join(allCategories, ", "))
		}
		cs[f] = true
	}
	return cs, nil
}

// has reports whether category is selected.
func (cs categorySet) has(category string) bool { return cs[category] }

// all reports whether every known category is selected. It counts selections
// rather than map entries so that it agrees with has and selected no matter how
// the set was built: an entry explicitly set to false is unselected, and the
// map[string]bool type cannot rule one out.
func (cs categorySet) all() bool { return len(cs.selected()) == len(allCategories) }

// selected returns the selected categories in allCategories order.
func (cs categorySet) selected() []string {
	out := make([]string, 0, len(cs))
	for _, c := range allCategories {
		if cs[c] {
			out = append(out, c)
		}
	}
	return out
}
