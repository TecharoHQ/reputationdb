// Command vpn-ip-maker builds a MaxMind .mmdb database of VPN, datacenter, and
// crawler IP addresses aggregated from several public git repositories.
//
// The upstream repositories are cloned into memory with rsc.io/gitfs (no working
// tree is written to disk), their IP/CIDR lists are parsed and merged, and the
// result is written to the mmdb file named as the sole positional argument:
//
//	vpn-ip-maker vpn-ip.mmdb
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	vpnip "github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/web/useragent"
	"github.com/facebookgo/flagenv"
	"github.com/gaissmai/bart"

	_ "github.com/joho/godotenv/autoload"
)

var (
	gcPercent            = flag.Int("gc-percent", 20, "GOGC value; lower trades CPU for a smaller peak heap (see runtime/debug.SetGCPercent)")
	ref                  = flag.String("ref", "HEAD", "git ref to clone from each source repository")
	userAgentContactLink = flag.String("user-agent-contact-link", "", "contact link for the user agent string")
	userAgentOrgName     = flag.String("user-agent-org-name", "", "organization name for the user agent string")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	if useragent.Version == "(devel)" {
		log.Fatal("Please build this binary and try again (don't use `go run`)")
	}

	if *userAgentContactLink == "" {
		log.Fatal("Please set --user-agent-contact-link")
	}

	if *userAgentOrgName == "" {
		log.Fatal("Please set --user-agent-org-name")
	}

	lg := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("program", "mkdatabase", "version", useragent.Version)
	slog.SetDefault(lg)

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <output.mmdb>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(2)
	}
	outPath := args[0]

	if err := run(lg, outPath); err != nil {
		log.Fatal(err)
	}
}

// cacheRoot returns the root directory under which list sources are cached:
// <os.UserCacheDir>/techaro/reputationdb. It returns "" (caching disabled) if
// the user cache directory cannot be determined.
func cacheRoot(lg *slog.Logger) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		lg.Warn("no user cache dir; sources will not be cached", "err", err)
		return ""
	}
	return filepath.Join(dir, "techaro", "reputationdb")
}

func run(lg *slog.Logger, outPath string) error {
	// The aggregated set of prefixes dwarfs everything else this program holds.
	// A lower GOGC keeps the peak heap closer to the live set (at the cost of
	// more frequent GC) instead of letting it balloon to ~2x before collecting.
	debug.SetGCPercent(*gcPercent)

	// store is the single in-memory home for every prefix and its memberships.
	// Using the radix trie as the primary store (rather than a map plus a
	// separate trie for mergeContained) avoids holding two full copies, and lets
	// the build loop below delete entries as it consumes them.
	store := &bart.Table[*vpnip.Record]{}

	cacheDir := cacheRoot(lg)
	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: useragent.Transport(
			*userAgentOrgName,
			"reputationdb/mkdatabase",
			*userAgentContactLink,
			http.DefaultTransport,
		),
	}

	for _, src := range sources {
		fsys, err := repoFS(lg, src, *ref, cacheDir)
		if err != nil {
			return err
		}

		n, err := collect(src, fsys, store)
		if err != nil {
			return fmt.Errorf("collecting from %s: %w", src.url, err)
		}
		lg.Info("collected", "repo", src.name, "entries", n)
	}

	for _, src := range httpSources {
		n, err := collectHTTP(context.Background(), httpClient, src, cacheDir, store)
		if err != nil {
			return fmt.Errorf("collecting from %s: %w", src.url, err)
		}
		lg.Info("collected", "source", src.name, "entries", n)
	}

	for _, src := range asnSources {
		n, err := collectAS(context.Background(), httpClient, src, cacheDir, store)
		if err != nil {
			return fmt.Errorf("collecting from AS%d: %w", src.asn, err)
		}
		lg.Info("collected", "source", fmt.Sprintf("AS%d", src.asn), "provider", src.provider, "entries", n)
	}

	for _, src := range fileSources {
		n, err := collectFile(src, store)
		if err != nil {
			return fmt.Errorf("collecting from %s: %w", src.path, err)
		}
		lg.Info("collected", "source", src.name, "entries", n)
	}

	mergeContained(store)
	debug.FreeOSMemory()

	lg.Info("building database", "unique_prefixes", store.Size())

	w, err := NewWriter(legacyDatabaseType, legacyDescription, time.Now())
	if err != nil {
		return fmt.Errorf("creating writer: %w", err)
	}

	// Snapshot the prefixes so we can delete each record from the store right
	// after inserting it into the tree. This keeps only one full representation
	// of the data live at a time: the store shrinks as the tree grows, instead
	// of both being held at full size simultaneously.
	prefixes := make([]netip.Prefix, 0, store.Size())
	for prefix := range store.All() {
		prefixes = append(prefixes, prefix)
	}

	var inserted, skipped int
	for _, prefix := range prefixes {
		rec, ok := store.Get(prefix)
		if !ok {
			continue
		}
		if err := w.Insert(prefix, *rec); err != nil {
			if IsSkippableNetwork(err) {
				skipped++
				store.Delete(prefix)
				continue
			}
			return fmt.Errorf("inserting %s: %w", prefix, err)
		}
		inserted++
		store.Delete(prefix)
	}
	if skipped > 0 {
		lg.Info("skipped ineligible prefixes", "count", skipped)
	}

	// The store is fully drained; release it (and its backing memory) before the
	// serialization pass, which allocates buffers of its own.
	store = nil
	prefixes = nil
	debug.FreeOSMemory()

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer f.Close()

	written, err := w.WriteTo(f)
	if err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", outPath, err)
	}

	lg.Info("wrote database", "path", outPath, "bytes", written, "prefixes", inserted)
	return nil
}
