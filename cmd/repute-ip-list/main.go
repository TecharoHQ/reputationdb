// Command ipassess loads a vpnip .mmdb database and checks a newline-delimited
// list of IP addresses against it, reporting how many of the listed addresses
// are present in the database and how they are flagged.
//
// When the GeoLite2 Country and ASN databases are available it also breaks the
// list down by country and by originating ASN, reporting the flagged rate of
// each as a markdown table.
//
// Lines that do not parse as an IP address (comments, blank lines, user-agent
// strings, etc.) are silently skipped, as are addresses repeated later in the
// list: every count reported is per unique address. Usage:
//
//	ipassess [flags]
//	ipassess -mmdb ./var/abusive-ips.mmdb -list data/ham/gistfile1.txt
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"sort"
	"strings"

	"github.com/TecharoHQ/reputationdb"
	"github.com/facebookgo/flagenv"
	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

var (
	mmdbPath    = flag.String("mmdb", "./var/reputationdb.mmdb", "path to the vpnip mmdb database to assess against")
	listPath    = flag.String("list", "", "path to the newline-delimited list of IP addresses")
	countryPath = flag.String("geolite-country", "./var/GeoLite2-Country.mmdb", "path to the GeoLite2 Country database; empty disables the country breakdown")
	asnPath     = flag.String("geolite-asn", "./var/GeoLite2-ASN.mmdb", "path to the GeoLite2 ASN database; empty disables the ASN breakdown")
	top         = flag.Int("top", 25, "number of rows to show in the country and ASN breakdowns; zero or less shows every row")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	if err := run(*mmdbPath, *listPath); err != nil {
		log.Fatal(err)
	}
}

// stats accumulates the assessment of every IP address found in the list.
type stats struct {
	totalLines int // every line read from the list file
	skipped    int // lines that did not parse as an IP address
	duplicates int // addresses seen earlier in the list
	addresses  int // distinct addresses that parsed as an IP address
	found      int // addresses present in the database
	notFound   int // addresses absent from the database

	isVPN        int
	isDatacenter int
	isCrawler    int
	isProxy      int

	categories map[string]int
	providers  map[string]int

	countries breakdown
	asns      breakdown
}

// bucket counts the addresses attributed to a single country or ASN.
type bucket struct {
	label   string
	addrs   int
	flagged int
}

// breakdown accumulates address counts per country or ASN, keyed by a stable
// identifier (ISO code, AS number) so that entries sharing a display label stay
// distinct.
type breakdown map[string]*bucket

// add records one address under key, creating the bucket on first sight.
func (b breakdown) add(key, label string, flagged bool) {
	e, ok := b[key]
	if !ok {
		e = &bucket{label: label}
		b[key] = e
	}
	e.addrs++
	if flagged {
		e.flagged++
	}
}

func run(mmdbPath, listPath string) error {
	db, err := reputationdb.Open(mmdbPath)
	if err != nil {
		return fmt.Errorf("opening mmdb %s: %w", mmdbPath, err)
	}
	defer db.Close()

	g, err := openGeo(*countryPath, *asnPath)
	if err != nil {
		return err
	}
	defer g.Close()

	f, err := os.Open(listPath)
	if err != nil {
		return fmt.Errorf("opening list %s: %w", listPath, err)
	}
	defer f.Close()

	s := stats{
		categories: map[string]int{},
		providers:  map[string]int{},
		countries:  breakdown{},
		asns:       breakdown{},
	}

	// Every count below is per distinct address: an address listed twice is one
	// address, not two, and counting it twice would inflate both the totals and
	// the per-country and per-ASN rows. Unmap keeps an address from being
	// counted twice for being written once in IPv4 and once in IPv4-mapped
	// IPv6 form.
	seen := map[netip.Addr]struct{}{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s.totalLines++

		line := strings.TrimSpace(sc.Text())
		addr, err := netip.ParseAddr(line)
		if err != nil {
			// Not an IP address (comment, blank line, user-agent, ...); skip it.
			s.skipped++
			continue
		}

		if _, ok := seen[addr.Unmap()]; ok {
			s.duplicates++
			continue
		}
		seen[addr.Unmap()] = struct{}{}
		s.addresses++

		res, found, err := db.Lookup(addr)
		if err != nil {
			return fmt.Errorf("looking up %s: %w", addr, err)
		}

		// The geo breakdowns cover every address in the list, not just the
		// flagged ones, so that each row can report a flagged rate.
		if err := g.account(&s, addr, found); err != nil {
			return err
		}

		if !found {
			s.notFound++
			continue
		}
		s.found++

		if res.IsVPN {
			s.isVPN++
		}
		if res.IsDatacenter {
			s.isDatacenter++
		}
		if res.IsCrawler {
			s.isCrawler++
		}
		if res.IsProxy {
			s.isProxy++
		}
		for _, cat := range res.Categories {
			s.categories[cat]++
		}
		for _, prov := range res.Providers {
			s.providers[prov]++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading list %s: %w", listPath, err)
	}

	s.report(mmdbPath, listPath)
	return nil
}

// countryRecord is the subset of a GeoLite2 Country record this command reads.
type countryRecord struct {
	Country struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string            `maxminddb:"iso_code"`
		Names   map[string]string `maxminddb:"names"`
	} `maxminddb:"registered_country"`
}

// asnRecord is the subset of a GeoLite2 ASN record this command reads.
type asnRecord struct {
	Number uint   `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

// geo resolves country and ASN metadata for an address. Either reader may be
// nil, which disables the matching breakdown.
type geo struct {
	country *maxminddb.Reader
	asn     *maxminddb.Reader
}

// openGeo opens the GeoLite2 databases at the given paths. An empty path
// disables that database, as does a path that does not exist: the GeoLite2
// files are downloaded separately and are not required to assess a list.
func openGeo(countryPath, asnPath string) (*geo, error) {
	var g geo
	for _, db := range []struct {
		name string
		path string
		dest **maxminddb.Reader
	}{
		{"GeoLite2 Country", countryPath, &g.country},
		{"GeoLite2 ASN", asnPath, &g.asn},
	} {
		if db.path == "" {
			continue
		}
		reader, err := maxminddb.Open(db.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			log.Printf("%s database %s not found, skipping that breakdown", db.name, db.path)
			continue
		case err != nil:
			g.Close()
			return nil, fmt.Errorf("opening %s database %s: %w", db.name, db.path, err)
		}
		*db.dest = reader
	}
	return &g, nil
}

// Close releases both GeoLite2 readers.
func (g *geo) Close() {
	for _, reader := range []*maxminddb.Reader{g.country, g.asn} {
		if reader != nil {
			reader.Close()
		}
	}
}

// account attributes addr to its country and ASN buckets in s.
func (g *geo) account(s *stats, addr netip.Addr, flagged bool) error {
	if g.country != nil {
		res := g.country.Lookup(addr)
		if err := res.Err(); err != nil {
			return fmt.Errorf("looking up country for %s: %w", addr, err)
		}

		key, label := "", "(unknown)"
		if res.Found() {
			var rec countryRecord
			if err := res.Decode(&rec); err != nil {
				return fmt.Errorf("decoding country for %s: %w", addr, err)
			}
			// Addresses without a physical country (satellite, anycast) still
			// carry the country their block is registered to.
			c := rec.Country
			if c.ISOCode == "" {
				c = rec.RegisteredCountry
			}
			if c.ISOCode != "" {
				key = c.ISOCode
				label = fmt.Sprintf("%s (%s)", countryName(c.Names, c.ISOCode), c.ISOCode)
			}
		}
		s.countries.add(key, label, flagged)
	}

	if g.asn != nil {
		res := g.asn.Lookup(addr)
		if err := res.Err(); err != nil {
			return fmt.Errorf("looking up ASN for %s: %w", addr, err)
		}

		key, label := "", "(unknown)"
		if res.Found() {
			var rec asnRecord
			if err := res.Decode(&rec); err != nil {
				return fmt.Errorf("decoding ASN for %s: %w", addr, err)
			}
			if rec.Number != 0 {
				key = fmt.Sprintf("%d", rec.Number)
				label = strings.TrimSpace(fmt.Sprintf("AS%d %s", rec.Number, rec.Org))
			}
		}
		s.asns.add(key, label, flagged)
	}

	return nil
}

// countryName returns the English name for a country, falling back to the ISO
// code when GeoLite2 carries no localised name for it.
func countryName(names map[string]string, isoCode string) string {
	if name := names["en"]; name != "" {
		return name
	}
	return isoCode
}

// report prints the accumulated statistics to stdout.
func (s stats) report(mmdbPath, listPath string) {
	fmt.Printf("Assessment of %s against %s\n", listPath, mmdbPath)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("lines read:        %d\n", s.totalLines)
	fmt.Printf("skipped (non-IP):  %d\n", s.skipped)
	fmt.Printf("skipped (dupe):    %d\n", s.duplicates)
	fmt.Printf("unique IPs:        %d\n", s.addresses)
	fmt.Printf("  flagged (in db): %d (%s)\n", s.found, pct(s.found, s.addresses))
	fmt.Printf("  clean (not in):  %d (%s)\n", s.notFound, pct(s.notFound, s.addresses))

	// The flags are a fixed schema, so they keep a stable order rather than
	// sorting by count: it makes two runs easy to diff against each other.
	fmt.Print("\n### Flags (of flagged addresses)\n\n")
	printCountRows("Flag", []countRow{
		{"is_vpn", s.isVPN},
		{"is_datacenter", s.isDatacenter},
		{"is_crawler", s.isCrawler},
		{"is_proxy", s.isProxy},
	}, s.found)

	printCountTable("Categories", "Category", s.categories, s.found)
	printCountTable("Providers", "Provider", s.providers, s.found)

	s.countries.printTable("Countries", "Country", *top)
	s.asns.printTable("ASNs", "ASN", *top)
}

// printTable prints the breakdown as a markdown table sorted by descending
// unique address count, showing at most top rows. A top of zero or less shows
// all of them.
func (b breakdown) printTable(title, keyHeader string, top int) {
	if len(b) == 0 {
		return
	}

	rows := make([]*bucket, 0, len(b))
	for _, e := range b {
		rows = append(rows, e)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].addrs != rows[j].addrs {
			return rows[i].addrs > rows[j].addrs
		}
		return rows[i].label < rows[j].label
	})

	shown := rows
	if top > 0 && len(shown) > top {
		shown = shown[:top]
	}

	fmt.Printf("\n### %s (%d distinct, of all addresses)\n\n", title, len(rows))
	fmt.Printf("| %s | Unique IPs | Flagged | Rate |\n", keyHeader)
	fmt.Println("| --- | ---: | ---: | ---: |")
	for _, e := range shown {
		fmt.Printf("| %s | %d | %d | %s |\n", escapePipes(e.label), e.addrs, e.flagged, pct(e.flagged, e.addrs))
	}
	if len(shown) < len(rows) {
		fmt.Printf("\n... and %d more (raise -top to see them)\n", len(rows)-len(shown))
	}
}

// escapePipes escapes the pipes in a label so that they do not split the
// markdown table cell the label sits in.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// countRow is one row of a markdown table counting a label's occurrences
// against some total.
type countRow struct {
	label string
	count int
}

// printCountTable prints a count map as a markdown table sorted by descending
// count, then label.
func printCountTable(title, keyHeader string, counts map[string]int, total int) {
	if len(counts) == 0 {
		return
	}

	rows := make([]countRow, 0, len(counts))
	for label, count := range counts {
		rows = append(rows, countRow{label, count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].label < rows[j].label
	})

	fmt.Printf("\n### %s (%d distinct, of flagged addresses)\n\n", title, len(rows))
	printCountRows(keyHeader, rows, total)
}

// printCountRows prints rows as a markdown table of counts and their share of
// total, in the order given.
func printCountRows(keyHeader string, rows []countRow, total int) {
	fmt.Printf("| %s | Unique IPs | Share |\n", keyHeader)
	fmt.Println("| --- | ---: | ---: |")
	for _, r := range rows {
		fmt.Printf("| %s | %d | %s |\n", escapePipes(r.label), r.count, pct(r.count, total))
	}
}

// pct formats n/total as a percentage string, guarding against divide-by-zero.
func pct(n, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(total))
}
