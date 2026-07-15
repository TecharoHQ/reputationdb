package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

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

// selectLists returns the entries of lists whose category is selected. An empty
// result means the whole repository can be skipped: nothing in it is wanted, so
// there is no reason to clone it.
func (cs categorySet) selectLists(lists []listSpec) []listSpec {
	out := make([]listSpec, 0, len(lists))
	for _, ls := range lists {
		if cs.has(ls.category) {
			out = append(out, ls)
		}
	}
	return out
}

// intersect returns the entries of categories that are selected, preserving
// their original order. It narrows an asnSource's categories: an AS tagged both
// datacenter and abuse still gets fetched for a datacentre-only build, but only
// its datacenter membership is folded. An empty result means the AS can be
// skipped entirely.
func (cs categorySet) intersect(categories []string) []string {
	out := make([]string, 0, len(categories))
	for _, c := range categories {
		if cs.has(c) {
			out = append(out, c)
		}
	}
	return out
}

// legacyDatabaseType is the database_type an unfiltered build has always
// written. It is preserved verbatim so existing consumers, which branch on this
// field, see no change.
//
// A hypothetical --category=vpn build would derive this same string. That
// collision is accepted: only the full and datacentre-only configurations ship.
const legacyDatabaseType = "Techaro-Veil-VPN"

// legacyDescription is the base description an unfiltered build has always
// written. It predates the abuse and tor categories and does not mention them;
// preserved as-is rather than churn the paid artifact's metadata.
const legacyDescription = "VPN, datacenter, crawler, and proxy IP addresses aggregated from public lists"

// displayNames renders category constants for database types and descriptions.
var displayNames = map[string]string{
	vpnip.CategoryAbuse:      "Abuse",
	vpnip.CategoryCrawler:    "Crawler",
	vpnip.CategoryDatacenter: "Datacenter",
	vpnip.CategoryProxy:      "Proxy",
	vpnip.CategoryTor:        "Tor",
	vpnip.CategoryVPN:        "VPN",
}

// displaySelected returns the display names of the selected categories, in
// allCategories order.
func (cs categorySet) displaySelected() []string {
	selected := cs.selected()
	out := make([]string, 0, len(selected))
	for _, c := range selected {
		out = append(out, displayNames[c])
	}
	return out
}

// databaseType derives the mmdb database_type for the selected categories, so
// that a consumer holding a file can tell which database it has.
func (cs categorySet) databaseType() string {
	if cs.all() {
		return legacyDatabaseType
	}
	return "Techaro-Veil-" + strings.Join(cs.displaySelected(), "-")
}

// baseDescription derives the human sentence describing what the database
// holds, before build provenance is appended by describe.
func (cs categorySet) baseDescription() string {
	if cs.all() {
		return legacyDescription
	}
	return strings.Join(cs.displaySelected(), ", ") + " IP addresses aggregated from public lists"
}

// describe returns the full text written into the database's Description
// metadata: what the database holds, plus which build produced it. Nothing else
// in the artifact records its provenance, which is what you want when someone
// reports a bad entry against a file they downloaded weeks ago.
//
// epoch is formatted rather than taken from the clock so that this string, like
// the build epoch itself, is a pure function of the source revision. See
// buildEpoch.
func describe(cs categorySet, version, revision string, epoch time.Time) string {
	if revision == "" {
		revision = "unknown"
	}
	return fmt.Sprintf("%s (built by mkdatabase %s from %s at %s)",
		cs.baseDescription(), version, revision, epoch.UTC().Format(time.RFC3339))
}

// categoryFlag collects repeated --category values. flag.Value's Set is called
// once per occurrence.
type categoryFlag []string

// String renders the collected values for flag's usage output.
func (c *categoryFlag) String() string { return strings.Join(*c, ",") }

// Set appends one occurrence's value.
func (c *categoryFlag) Set(value string) error {
	*c = append(*c, value)
	return nil
}
