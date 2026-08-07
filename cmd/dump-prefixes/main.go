// Command dump-prefixes writes every network in a reputation database out as a
// plain IP list, one CIDR prefix per line.
//
// The database is overwhelmingly made of single addresses, so most of the
// output is /32s and /128s rather than aggregated ranges. Prefixes from the
// IPv4 half of the database are written in their IPv4 spelling.
//
//	dump-prefixes -mmdb ./var/reputationdb.mmdb -out all-prefixes.txt
//	dump-prefixes -category vpn -out vpn-prefixes.txt
//	dump-prefixes -category datacenter -provider mullvad
//
// Both -category and -provider may be repeated. Repeating one flag widens the
// selection (a record on any of the named lists is written out); using both
// narrows it (a record has to match each flag to be written out). With no
// filter at all every prefix is written, and no record has to be decoded, which
// is several times faster than any filtered dump.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/TecharoHQ/reputationdb"
	"github.com/facebookgo/flagenv"
)

var (
	mmdbPath   = flag.String("mmdb", "./var/reputationdb.mmdb", "path to the reputation database to dump")
	outPath    = flag.String("out", "-", "file to write prefixes to; - writes to stdout")
	slogLevel  = flag.String("slog-level", "INFO", "logging level (see log/slog)")
	categories repeatedFlag
	providers  repeatedFlag
)

func init() {
	flag.Var(&categories, "category", "only write prefixes in this category; repeat to select several; omit to select all (one of: "+strings.Join(knownCategories, ", ")+")")
	flag.Var(&providers, "provider", "only write prefixes belonging to this provider; repeat to select several; omit to select all")
}

func main() {
	flagenv.Parse()
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*slogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -slog-level %q: %v\n", *slogLevel, err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("program", "dump-prefixes"))

	if err := run(*mmdbPath, *outPath, categories, providers); err != nil {
		slog.Error("dump failed", "err", err)
		os.Exit(1)
	}
}

// knownCategories is every category a record may carry. A -category value is
// checked against it so that a typo fails at startup instead of writing an
// empty file that looks like an honest answer.
var knownCategories = []string{
	reputationdb.CategoryAbuse,
	reputationdb.CategoryCrawler,
	reputationdb.CategoryDatacenter,
	reputationdb.CategoryProxy,
	reputationdb.CategoryTor,
	reputationdb.CategoryVPN,
}

// filter decides which of the database's records get written out. A family of
// values is a union within itself and an intersection against the other
// families: -category vpn -category tor writes records on either list, while
// -category vpn -provider mullvad writes only records on both.
type filter struct {
	// categories is the -category values in the bitmask form that the database
	// stores. A record then costs one AND instead of a walk over both slices.
	categories reputationdb.CategoryByte
	providers  []string
}

// parseFilter validates the repeated -category and -provider values and builds
// the filter they describe.
//
// Providers are taken as given. Unlike categories they are an open set derived
// from upstream list filenames, so there is nothing to check a value against; a
// provider that matches nothing is reported after the walk instead.
func parseFilter(categories, providers []string) (filter, error) {
	for _, c := range categories {
		if !slices.Contains(knownCategories, c) {
			return filter{}, fmt.Errorf("unknown category %q: valid categories are %s", c, strings.Join(knownCategories, ", "))
		}
	}
	return filter{categories: reputationdb.FromCategories(categories), providers: providers}, nil
}

// empty reports whether the filter selects everything, in which case no record
// needs to be decoded to decide.
func (f filter) empty() bool { return f.categories == 0 && len(f.providers) == 0 }

// match reports whether res satisfies every family of values in the filter.
func (f filter) match(res *reputationdb.Result) bool {
	if f.categories != 0 && !res.Categories.Has(f.categories) {
		return false
	}
	if len(f.providers) != 0 && !slices.ContainsFunc(f.providers, res.HasProvider) {
		return false
	}
	return true
}

// stats counts what a dump saw. decoded is worth separating from prefixes
// because decoding is what makes a filtered dump slow: an unfiltered dump
// visits just as many prefixes and decodes none of them.
type stats struct {
	prefixes int // networks visited in the database
	decoded  int // records decoded to test against the filter
	written  int // prefixes written out
}

// LogValue renders the counts as one group so a run summary stays a single
// structured field.
func (s stats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("prefixes", s.prefixes),
		slog.Int("decoded", s.decoded),
		slog.Int("written", s.written),
	)
}

func run(mmdbPath, outPath string, categories, providers []string) error {
	f, err := parseFilter(categories, providers)
	if err != nil {
		return err
	}

	db, err := reputationdb.Open(mmdbPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", mmdbPath, err)
	}
	defer db.Close()

	out, closeOut, err := openOut(outPath)
	if err != nil {
		return err
	}
	// Covers the error paths below; the success path closes explicitly so that
	// it can report a close error. closeOut is safe to call twice.
	defer func() { _ = closeOut() }()

	// A full database is tens of millions of prefixes, so the output is
	// buffered: one write syscall per prefix would dominate the run.
	buf := bufio.NewWriter(out)
	s, err := dump(buf, db, f)
	if err != nil {
		return err
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	// Closing is what surfaces a deferred write error on some filesystems, so
	// it happens here rather than in a defer whose error nobody reads.
	if err := closeOut(); err != nil {
		return fmt.Errorf("closing %s: %w", outPath, err)
	}

	slog.Info("dump complete", "stats", s, "mmdb", mmdbPath,
		"database-type", db.Metadata().DatabaseType,
		"category", categories, "provider", providers)
	if !f.empty() && s.written == 0 {
		// Every filter value is spelled correctly (categories are checked, and
		// providers cannot be) yet nothing matched. Say so, because an empty
		// file otherwise reads as a database that genuinely holds none of this.
		slog.Warn("no prefix matched the filter", "category", categories, "provider", providers)
	}
	return nil
}

// dump writes the prefixes of every network matching f to w, one per line.
func dump(w io.Writer, db *reputationdb.DB, f filter) (stats, error) {
	var s stats

	skipDecode := f.empty()
	for network, err := range db.Networks() {
		if err != nil {
			return s, fmt.Errorf("walking the database: %w", err)
		}
		s.prefixes++

		if !skipDecode {
			res, err := network.Result()
			if err != nil {
				return s, fmt.Errorf("decoding the record at %s: %w", network.Prefix, err)
			}
			s.decoded++
			if !f.match(&res) {
				continue
			}
		}

		if _, err := fmt.Fprintln(w, network.Prefix); err != nil {
			return s, fmt.Errorf("writing %s: %w", network.Prefix, err)
		}
		s.written++
	}

	return s, nil
}

// openOut opens the destination for the prefix list.
//
// The close function is a no-op for stdout, since closing os.Stdout would break
// any later writer of it and gains nothing. It is idempotent, so calling it
// explicitly and from a defer closes the file once.
func openOut(path string) (io.Writer, func() error, error) {
	if path == "-" || path == "" {
		return os.Stdout, func() error { return nil }, nil
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("creating output file %s: %w", path, err)
	}
	return f, sync.OnceValue(f.Close), nil
}

// repeatedFlag collects the values of a flag given more than once. flag.Value's
// Set is called once per occurrence.
type repeatedFlag []string

// String renders the collected values for flag's usage output.
func (r *repeatedFlag) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

// Set appends one occurrence's value.
func (r *repeatedFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}
