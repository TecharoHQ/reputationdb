package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			name: "kubectl prefixed ipv4",
			line: `[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] 85.208.96.201 - - [30/Jul/2026:18:45:02 +0000] "GET /x HTTP/1.1" 200 4899 "-" "-" 399 1.525`,
			want: "85.208.96.201",
			ok:   true,
		},
		{
			name: "kubectl prefixed ipv6",
			line: `[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] 2a05:d01c:497:716:3aa0:d5d3:1069:e167 - - [30/Jul/2026:18:45:02 +0000] "POST /x HTTP/2.0" 200 119`,
			want: "2a05:d01c:497:716:3aa0:d5d3:1069:e167",
			ok:   true,
		},
		{
			name: "bare access log without kubectl prefix",
			line: `136.66.41.41 - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 3984`,
			want: "136.66.41.41",
			ok:   true,
		},
		{
			name: "leading whitespace",
			line: `   136.66.41.41 - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 3984`,
			want: "136.66.41.41",
			ok:   true,
		},
		{
			// The tail of a multi-container `kubectl logs` stream can interleave
			// prefixes; anything whose first field is not an address is not a
			// request we can attribute to a client.
			name: "first field is not an address",
			line: `[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] I0730 18:45:02.123456 1 event.go:389] Event occurred`,
			ok:   false,
		},
		{
			name: "unix socket dash address",
			line: `- - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 3984`,
			ok:   false,
		},
		{
			name: "empty line",
			line: ``,
			ok:   false,
		},
		{
			name: "only a kubectl prefix",
			line: `[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller]`,
			ok:   false,
		},
		{
			// An unterminated bracket must not eat the rest of the line looking
			// for a closing bracket that never comes.
			name: "unterminated bracket",
			line: `[pod/ingress-nginx 85.208.96.201 - -`,
			ok:   false,
		},
		{
			// IPv4-mapped IPv6 is written back out in its plain IPv4 form so
			// that the two spellings collapse under sort|uniq.
			name: "ipv4-mapped ipv6 is unmapped",
			line: `::ffff:85.208.96.201 - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 1`,
			want: "85.208.96.201",
			ok:   true,
		},
		{
			// nginx writes a zone-less address; a zone would make the output
			// unusable as an IP list, so reject it rather than emit it.
			name: "zoned address rejected",
			line: `fe80::1%eth0 - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 1`,
			ok:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := clientIP(tt.line)
			if ok != tt.ok {
				t.Logf("want ok: %v", tt.ok)
				t.Logf("got ok:  %v", ok)
				t.Fatal("got wrong ok value")
			}
			if ok && got != tt.want {
				t.Logf("want: %q", tt.want)
				t.Logf("got:  %q", got)
				t.Error("got wrong address")
			}
		})
	}
}

func TestRegexpValue(t *testing.T) {
	t.Parallel()

	t.Run("default is printed unquoted", func(t *testing.T) {
		t.Parallel()

		rv := newRegexpValue(defaultHoneypotRe)
		if got := rv.String(); got != defaultHoneypotRe {
			t.Logf("want: %q", defaultHoneypotRe)
			t.Logf("got:  %q", got)
			t.Error("String() does not round-trip the pattern it was built with")
		}
	})

	t.Run("zero value has no pattern", func(t *testing.T) {
		t.Parallel()

		// flag prints the zero value of a Value to decide whether a default is
		// worth mentioning, so it must not panic on a nil regexp.
		var rv regexpValue
		if got := rv.String(); got != "" {
			t.Logf("got: %q", got)
			t.Error("zero regexpValue should stringify as empty")
		}
	})

	t.Run("set replaces the pattern", func(t *testing.T) {
		t.Parallel()

		rv := newRegexpValue(defaultHoneypotRe)
		if err := rv.Set(`/trap/`); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if got := rv.String(); got != `/trap/` {
			t.Logf("got: %q", got)
			t.Error("Set did not replace the pattern")
		}
		if !rv.Regexp().MatchString(`GET /trap/ HTTP/1.1`) {
			t.Error("compiled regexp does not match what it should")
		}
	})

	t.Run("set rejects a bad pattern and keeps the old one", func(t *testing.T) {
		t.Parallel()

		rv := newRegexpValue(defaultHoneypotRe)
		if err := rv.Set(`(`); err == nil {
			t.Fatal("Set accepted an uncompilable pattern")
		}
		if got := rv.String(); got != defaultHoneypotRe {
			t.Logf("got: %q", got)
			t.Error("a failed Set clobbered the previous pattern")
		}
	})
}

// The sample lines from a real ingress-nginx stream: one honeypot hit, two
// ordinary requests.
const sampleLog = `[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] 2a05:d01c:497:716:3aa0:d5d3:1069:e167 - - [30/Jul/2026:18:45:02 +0000] "POST /techaro.thoth.iptoasn.v1.IpToASNService/Lookup HTTP/2.0" 200 119 "-" "Techaro/anubis:1.25.0 grpc-go/1.77.0" 406 0.068 [techaro-thoth-http] [] 10.244.111.13:3000 161 0.068 200 078c608384c2ade6ef58868123ce7382
[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] 136.66.41.41 - - [30/Jul/2026:18:45:02 +0000] "GET / HTTP/1.1" 200 3984 "-" "Mozilla/5.0 (Linux; Android 12; Pixel 6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Mobile Safari/537.36" 673 0.007 [default-techarosite-http] [] 10.244.111.7:3000 3984 0.007 200 27644c0f5e4c4ad4aecccfdb0d2fec09
[pod/ingress-nginx-controller-5cd856b6bb-lgfhj/controller] 85.208.96.201 - - [30/Jul/2026:18:45:02 +0000] "GET /.within.website/x/cmd/anubis/api/honeypot/7ec7591b-97d0-49f1-a3d6-d5d53ec4aa58/515cfc5b-300e-46ab-af34-8dd36bebbbc3 HTTP/1.1" 200 4899 "-" "Mozilla/5.0 (compatible; SemrushBot/7~bl; +http://www.semrush.com/bot.html)" 399 1.525 [default-xesite-anubis] [] 10.244.111.51:8081 4900 1.525 200 dd894048924b3f3759b5877ab9a85a4e
`

func TestScan(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		pattern string
		in      string
		want    []string
		stats   stats
	}{
		{
			name:    "sample log yields only the honeypot hit",
			pattern: defaultHoneypotRe,
			in:      sampleLog,
			want:    []string{"85.208.96.201"},
			stats:   stats{lines: 3, matched: 1, written: 1},
		},
		{
			name:    "honeypot init stage matches too",
			pattern: defaultHoneypotRe,
			in:      `1.2.3.4 - - [30/Jul/2026:18:45:02 +0000] "GET /.within.website/x/cmd/anubis/api/honeypot/7ec7591b-97d0-49f1-a3d6-d5d53ec4aa58/init HTTP/1.1" 200 4899` + "\n",
			want:    []string{"1.2.3.4"},
			stats:   stats{lines: 1, matched: 1, written: 1},
		},
		{
			// Anubis' BasePrefix is configurable, so the honeypot route can be
			// mounted under an arbitrary path.
			name:    "base prefix does not stop a match",
			pattern: defaultHoneypotRe,
			in:      `1.2.3.4 - - [30/Jul/2026:18:45:02 +0000] "GET /myprefix/.within.website/x/cmd/anubis/api/honeypot/abc/init HTTP/1.1" 200 1` + "\n",
			want:    []string{"1.2.3.4"},
			stats:   stats{lines: 1, matched: 1, written: 1},
		},
		{
			// Duplicates are deliberately kept so `sort | uniq -c` can rank the
			// noisiest offenders downstream.
			name:    "duplicates are kept",
			pattern: `honeypot`,
			in:      "1.2.3.4 - - \"GET /honeypot/x\"\n1.2.3.4 - - \"GET /honeypot/y\"\n5.6.7.8 - - \"GET /honeypot/z\"\n",
			want:    []string{"1.2.3.4", "1.2.3.4", "5.6.7.8"},
			stats:   stats{lines: 3, matched: 3, written: 3},
		},
		{
			// A matching line we cannot attribute to a client is worth counting
			// separately: silently dropping it would hide a log format change.
			name:    "matching line without a parseable address is counted",
			pattern: `honeypot`,
			in:      "I0730 18:45:02.123456 1 event.go:389] honeypot something\n1.2.3.4 - - \"GET /honeypot/x\"\n",
			want:    []string{"1.2.3.4"},
			stats:   stats{lines: 2, matched: 2, written: 1, unparseable: 1},
		},
		{
			name:    "blank lines are skipped without counting as unparseable",
			pattern: `honeypot`,
			in:      "\n\n1.2.3.4 - - \"GET /honeypot/x\"\n\n",
			want:    []string{"1.2.3.4"},
			stats:   stats{lines: 1, matched: 1, written: 1},
		},
		{
			name:    "no matches yields no output",
			pattern: defaultHoneypotRe,
			in:      "136.66.41.41 - - \"GET / HTTP/1.1\" 200 3984\n",
			want:    nil,
			stats:   stats{lines: 1},
		},
		{
			name:    "empty input",
			pattern: defaultHoneypotRe,
			in:      "",
			want:    nil,
			stats:   stats{},
		},
		{
			name:    "final line without a trailing newline still counts",
			pattern: `honeypot`,
			in:      `1.2.3.4 - - "GET /honeypot/x"`,
			want:    []string{"1.2.3.4"},
			stats:   stats{lines: 1, matched: 1, written: 1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			re, err := regexp.Compile(tt.pattern)
			if err != nil {
				t.Fatalf("compiling %q: %v", tt.pattern, err)
			}

			var out strings.Builder
			got, err := scan(&out, strings.NewReader(tt.in), re)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}

			if got != tt.stats {
				t.Logf("want: %+v", tt.stats)
				t.Logf("got:  %+v", got)
				t.Error("got wrong stats")
			}

			var want string
			for _, addr := range tt.want {
				want += addr + "\n"
			}
			if out.String() != want {
				t.Logf("want: %q", want)
				t.Logf("got:  %q", out.String())
				t.Error("got wrong output")
			}
		})
	}
}

// stepReader hands out one line per Read call, invoking before with the index
// of the line it is about to yield. It lets a test observe what has been
// written at each point partway through a scan.
type stepReader struct {
	lines  []string
	i      int
	before func(i int)
}

func (r *stepReader) Read(p []byte) (int, error) {
	if r.i >= len(r.lines) {
		return 0, io.EOF
	}
	r.before(r.i)
	n := copy(p, r.lines[r.i])
	r.i++
	return n, nil
}

// TestScanWritesEachLineAsItGoes proves a match reaches the writer before the
// input is exhausted. A `kubectl logs -f | nginxscan` pipeline is useless if
// the addresses only show up when the stream ends, which is exactly what
// happens if the output is block-buffered.
func TestScanWritesEachLineAsItGoes(t *testing.T) {
	t.Parallel()

	lines := []string{
		"1.2.3.4 - - \"GET /honeypot/a\"\n",
		"5.6.7.8 - - \"GET /honeypot/b\"\n",
		"9.10.11.12 - - \"GET /honeypot/c\"\n",
	}
	// Before line i is read, every address from the lines before it must
	// already have been written.
	want := []string{"", "1.2.3.4\n", "1.2.3.4\n5.6.7.8\n"}

	var out strings.Builder
	r := &stepReader{lines: lines, before: func(i int) {
		if got := out.String(); got != want[i] {
			t.Errorf("before reading line %d:\nwant: %q\ngot:  %q", i, want[i], got)
		}
	}}

	got, err := scan(&out, r, regexp.MustCompile(`honeypot`))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if want := (stats{lines: 3, matched: 3, written: 3}); got != want {
		t.Logf("want: %+v", want)
		t.Logf("got:  %+v", got)
		t.Error("got wrong stats")
	}
	if want := "1.2.3.4\n5.6.7.8\n9.10.11.12\n"; out.String() != want {
		t.Logf("want: %q", want)
		t.Logf("got:  %q", out.String())
		t.Error("got wrong output")
	}
}

// TestOpenOutWritesThrough is the guard against the output being buffered
// again. It writes one address to the writer openOut hands back and reads the
// file from disk without closing first: if anything block-buffers in between,
// the file is still empty and this fails.
func TestOpenOutWritesThrough(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ips.txt")
	out, closeOut, err := openOut(path)
	if err != nil {
		t.Fatalf("openOut: %v", err)
	}
	defer func() { _ = closeOut() }()

	if _, err := fmt.Fprintln(out, "1.2.3.4"); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	if want := "1.2.3.4\n"; string(got) != want {
		t.Logf("want: %q", want)
		t.Logf("got:  %q", string(got))
		t.Error("output did not reach the file before close; is it buffered?")
	}

	// run closes explicitly and again from a defer, so a second close must not
	// report an error.
	if err := closeOut(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closeOut(); err != nil {
		t.Errorf("second close should be a no-op, got: %v", err)
	}
}

// TestOpenOutStdout checks that the stdout path neither closes os.Stdout nor
// reports an error for trying.
func TestOpenOutStdout(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"-", ""} {
		t.Run("path "+path, func(t *testing.T) {
			t.Parallel()

			out, closeOut, err := openOut(path)
			if err != nil {
				t.Fatalf("openOut: %v", err)
			}
			if out != io.Writer(os.Stdout) {
				t.Error("did not get os.Stdout")
			}
			if err := closeOut(); err != nil {
				t.Errorf("close: %v", err)
			}
			// If closeOut had really closed stdout this would fail.
			if _, err := os.Stdout.Stat(); err != nil {
				t.Errorf("os.Stdout was closed: %v", err)
			}
		})
	}
}

// TestScanLongLine guards the scanner's buffer limit: nginx logs carry
// attacker-controlled user agents and request URIs, so a line far longer than
// bufio.Scanner's 64KiB default is a thing a real log can contain.
func TestScanLongLine(t *testing.T) {
	t.Parallel()

	long := `1.2.3.4 - - [30/Jul/2026:18:45:02 +0000] "GET /.within.website/x/cmd/anubis/api/honeypot/abc/init HTTP/1.1" 200 1 "-" "` + strings.Repeat("A", 128*1024) + `"` + "\n"

	var out strings.Builder
	got, err := scan(&out, strings.NewReader(long), regexp.MustCompile(defaultHoneypotRe))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if want := (stats{lines: 1, matched: 1, written: 1}); got != want {
		t.Logf("want: %+v", want)
		t.Logf("got:  %+v", got)
		t.Error("got wrong stats")
	}
	if out.String() != "1.2.3.4\n" {
		t.Logf("got: %q", out.String())
		t.Error("long line was not scanned")
	}
}
