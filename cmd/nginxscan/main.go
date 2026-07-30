// Command nginxscan reads ingress-nginx access logs and writes out the client
// IP address of every request that hit an Anubis honeypot route.
//
// Input is read from stdin, or from the files named as positional arguments.
// Both the bare nginx combined log format and the `[pod/name/container] `
// prefix that `kubectl logs --prefix` adds are understood. Output is one
// address per line, in the order the requests were logged, duplicates
// included: a downstream script is expected to sort it, and keeping the
// duplicates lets `sort | uniq -c` rank the noisiest offenders.
//
//	kubectl logs -n ingress-nginx -l app.kubernetes.io/name=ingress-nginx --prefix --tail=-1 \
//	  | nginxscan -out honeypot-ips.txt
//	sort -u honeypot-ips.txt > data/manually-submitted/techaro/$(date -I).txt
//
// The route to look for is the -honeypot-re flag, a regexp matched against the
// whole log line. The default matches the honeypot maze Anubis mounts at
// {BasePrefix}/.within.website/x/cmd/anubis/api/honeypot/{id}/{stage}.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/facebookgo/flagenv"
)

// defaultHoneypotRe matches a request for the honeypot maze Anubis registers at
// {BasePrefix}{APIPrefix}honeypot/{id}/{stage}. BasePrefix is configurable and
// APIPrefix is not, so the match starts at the fixed part of the route. The
// trailing slash is required: it keeps the pattern from matching a bare
// .../honeypot mention somewhere else in the line.
const defaultHoneypotRe = `/\.within\.website/x/cmd/anubis/api/honeypot/`

var (
	honeypotRe = newRegexpValue(defaultHoneypotRe)
	outPath    = flag.String("out", "-", "file to write matched IP addresses to; - writes to stdout")
	slogLevel  = flag.String("slog-level", "INFO", "logging level (see log/slog)")
)

func init() {
	flag.Var(honeypotRe, "honeypot-re", "regexp matched against each log line; lines that match have their client IP written out")
}

func main() {
	flagenv.Parse()
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*slogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -slog-level %q: %v\n", *slogLevel, err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("program", "nginxscan"))

	if err := run(*outPath, flag.Args(), honeypotRe.Regexp()); err != nil {
		slog.Error("scan failed", "err", err)
		os.Exit(1)
	}
}

// stats counts what a scan saw. Only lines that matched the honeypot regexp are
// accounted for beyond the line count; the overwhelming majority of an nginx
// log is ordinary traffic that this command has no opinion about.
type stats struct {
	lines       int // non-blank lines read
	matched     int // lines matching the honeypot regexp
	written     int // addresses written out
	unparseable int // matching lines whose client IP could not be read
}

// LogValue renders the counts as one group so a run summary stays a single
// structured field.
func (s stats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("lines", s.lines),
		slog.Int("matched", s.matched),
		slog.Int("written", s.written),
		slog.Int("unparseable", s.unparseable),
	)
}

// add folds another scan's counts into s.
func (s *stats) add(other stats) {
	s.lines += other.lines
	s.matched += other.matched
	s.written += other.written
	s.unparseable += other.unparseable
}

func run(outPath string, args []string, re *regexp.Regexp) error {
	out, closeOut, err := openOut(outPath)
	if err != nil {
		return err
	}
	// Covers the error paths below; the success path closes explicitly so that
	// it can report a close error. closeOut is safe to call twice.
	defer func() { _ = closeOut() }()

	// The output is deliberately not buffered: each address is written straight
	// through to the fd as soon as its line is matched, so that a
	// `kubectl logs -f | nginxscan` pipeline reports hits as they happen rather
	// than in 4KiB bursts. That costs one write syscall per honeypot hit, which
	// is a rounding error next to reading the log lines they were found in.
	var total stats
	if len(args) == 0 {
		s, err := scan(out, os.Stdin, re)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		total.add(s)
	}
	for _, path := range args {
		s, err := scanFile(out, path, re)
		if err != nil {
			return err
		}
		total.add(s)
	}

	// Closing is what surfaces a deferred write error on some filesystems, so
	// it happens here rather than in a defer whose error nobody reads.
	if err := closeOut(); err != nil {
		return fmt.Errorf("closing %s: %w", outPath, err)
	}

	slog.Info("scan complete", "stats", total, "honeypot-re", re.String())
	if total.unparseable != 0 {
		// A matching line whose first field is not an address means either an
		// interleaved non-access-log line or a log format this command does not
		// understand. Either way it is worth saying out loud, because the
		// alternative is silently under-reporting abusive addresses.
		slog.Warn("some matching lines had no readable client IP", "count", total.unparseable)
	}
	return nil
}

// openOut opens the destination for matched addresses. The writer it returns
// is the file itself, unwrapped: interposing a bufio.Writer here would hold
// matched addresses back until a block filled, which is the opposite of what a
// `tail -f`-style pipeline wants.
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

// scanFile scans one named log file.
func scanFile(w io.Writer, path string, re *regexp.Regexp) (stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return stats{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	s, err := scan(w, f, re)
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", path, err)
	}
	return s, nil
}

// maxLineLen bounds a single log line. nginx logs carry attacker-controlled
// request URIs and user agents, so lines well past bufio.Scanner's 64KiB
// default do occur; a megabyte is generous without letting one line pull an
// unbounded amount of a log into memory.
const maxLineLen = 1024 * 1024

// scan reads newline-delimited log lines from r and writes the client IP of
// every line matching re to w, one per line.
func scan(w io.Writer, r io.Reader, re *regexp.Regexp) (stats, error) {
	var s stats

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineLen)

	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		s.lines++

		if !re.MatchString(line) {
			continue
		}
		s.matched++

		addr, ok := clientIP(line)
		if !ok {
			s.unparseable++
			slog.Debug("matching line has no readable client IP", "line", line)
			continue
		}

		if _, err := fmt.Fprintln(w, addr); err != nil {
			return s, fmt.Errorf("writing %s: %w", addr, err)
		}
		s.written++
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return s, fmt.Errorf("log line longer than %d bytes: %w", maxLineLen, err)
		}
		return s, err
	}

	return s, nil
}

// clientIP returns the client address a log line is attributed to: the first
// field of the nginx combined log format, after stripping the
// `[pod/name/container] ` prefix that `kubectl logs --prefix` prepends.
//
// It reports false for a line whose first field is not an address, which is how
// an interleaved controller log line or a different log format gets skipped
// rather than emitted as garbage.
func clientIP(line string) (string, bool) {
	line = strings.TrimSpace(line)

	// Strip a kubectl prefix. Only a bracket at the very start counts: the
	// combined log format brackets the timestamp and ingress-nginx brackets the
	// upstream name, and neither should be mistaken for a stream prefix. An
	// unterminated bracket means the line is not one this command understands.
	if strings.HasPrefix(line, "[") {
		end := strings.IndexByte(line, ']')
		if end < 0 {
			return "", false
		}
		line = strings.TrimSpace(line[end+1:])
	}

	field, _, _ := strings.Cut(line, " ")
	addr, err := netip.ParseAddr(field)
	if err != nil {
		return "", false
	}

	// A zone makes an address meaningless outside the host that logged it, so
	// it has no business in a list of abusive addresses.
	if addr.Zone() != "" {
		return "", false
	}

	// Unmap so that an address logged as ::ffff:1.2.3.4 collapses together with
	// the same address logged as 1.2.3.4 when the output is sorted.
	return addr.Unmap().String(), true
}

// regexpValue is a flag.Getter holding a compiled regexp, so that a bad pattern
// is reported as a flag parse error at startup rather than panicking partway
// through a scan. It keeps the source pattern around because a *regexp.Regexp
// built from a pattern does not always print back the same string.
type regexpValue struct {
	pattern string
	re      *regexp.Regexp
}

// newRegexpValue builds a regexpValue from a pattern that must compile; it is
// for flag defaults, which are compiled at init time and are a programming
// error if they are wrong.
func newRegexpValue(pattern string) *regexpValue {
	return &regexpValue{pattern: pattern, re: regexp.MustCompile(pattern)}
}

// String returns the pattern as written. The flag package calls this on a zero
// value to work out whether a default is worth printing, so it must tolerate a
// nil receiver and an uncompiled value.
func (rv *regexpValue) String() string {
	if rv == nil {
		return ""
	}
	return rv.pattern
}

// Set compiles pattern and adopts it. A pattern that does not compile leaves
// the previous value in place.
func (rv *regexpValue) Set(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("compiling %q: %w", pattern, err)
	}
	rv.pattern, rv.re = pattern, re
	return nil
}

// Get implements flag.Getter.
func (rv *regexpValue) Get() any { return rv.re }

// Regexp returns the compiled regexp.
func (rv *regexpValue) Regexp() *regexp.Regexp { return rv.re }
