// Command ipassess loads a vpnip .mmdb database and checks a newline-delimited
// list of IP addresses against it, reporting how many of the listed addresses
// are present in the database and how they are flagged.
//
// Lines that do not parse as an IP address (comments, blank lines, user-agent
// strings, etc.) are silently skipped. Usage:
//
//	ipassess [flags]
//	ipassess -mmdb ./var/abusive-ips.mmdb -list data/ham/gistfile1.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"sort"
	"strings"

	"github.com/TecharoHQ/reputationdb"
	"github.com/facebookgo/flagenv"
)

var (
	mmdbPath = flag.String("mmdb", "./var/abusive-ips.mmdb", "path to the vpnip mmdb database to assess against")
	listPath = flag.String("list", "", "path to the newline-delimited list of IP addresses")
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
	addresses  int // lines that parsed as an IP address
	found      int // addresses present in the database
	notFound   int // addresses absent from the database

	isVPN        int
	isDatacenter int
	isCrawler    int
	isProxy      int

	categories map[string]int
	providers  map[string]int
}

func run(mmdbPath, listPath string) error {
	db, err := reputationdb.Open(mmdbPath)
	if err != nil {
		return fmt.Errorf("opening mmdb %s: %w", mmdbPath, err)
	}
	defer db.Close()

	f, err := os.Open(listPath)
	if err != nil {
		return fmt.Errorf("opening list %s: %w", listPath, err)
	}
	defer f.Close()

	s := stats{
		categories: map[string]int{},
		providers:  map[string]int{},
	}

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
		s.addresses++

		res, found, err := db.Lookup(addr)
		if err != nil {
			return fmt.Errorf("looking up %s: %w", addr, err)
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

// report prints the accumulated statistics to stdout.
func (s stats) report(mmdbPath, listPath string) {
	fmt.Printf("Assessment of %s against %s\n", listPath, mmdbPath)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("lines read:        %d\n", s.totalLines)
	fmt.Printf("skipped (non-IP):  %d\n", s.skipped)
	fmt.Printf("IP addresses:      %d\n", s.addresses)
	fmt.Printf("  flagged (in db): %d (%s)\n", s.found, pct(s.found, s.addresses))
	fmt.Printf("  clean (not in):  %d (%s)\n", s.notFound, pct(s.notFound, s.addresses))

	fmt.Println("\nFlags (of flagged addresses):")
	fmt.Printf("  is_vpn:        %d (%s)\n", s.isVPN, pct(s.isVPN, s.found))
	fmt.Printf("  is_datacenter: %d (%s)\n", s.isDatacenter, pct(s.isDatacenter, s.found))
	fmt.Printf("  is_crawler:    %d (%s)\n", s.isCrawler, pct(s.isCrawler, s.found))
	fmt.Printf("  is_proxy:      %d (%s)\n", s.isProxy, pct(s.isProxy, s.found))

	printCounts("Categories", s.categories, s.found)
	printCounts("Providers", s.providers, s.found)
}

// printCounts prints a count map sorted by descending count, then name.
func printCounts(title string, counts map[string]int, total int) {
	if len(counts) == 0 {
		return
	}

	type kv struct {
		name  string
		count int
	}
	pairs := make([]kv, 0, len(counts))
	for name, count := range counts {
		pairs = append(pairs, kv{name, count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].name < pairs[j].name
	})

	fmt.Printf("\n%s (of flagged addresses):\n", title)
	for _, p := range pairs {
		fmt.Printf("  %-24s %d (%s)\n", p.name, p.count, pct(p.count, total))
	}
}

// pct formats n/total as a percentage string, guarding against divide-by-zero.
func pct(n, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(total))
}
