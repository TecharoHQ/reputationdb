package useragent

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
)

var (
	hostname = "<unknown>"

	// Version is the version of the main module, filled in from the build
	// info at startup, so outgoing requests advertise which build made them.
	Version = "devel"
)

func init() {
	name, _ := os.Hostname()
	if name != "" {
		hostname = name
	}

	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		Version = bi.Main.Version
	}
}

// GenUserAgent creates a unique User-Agent string for outgoing HTTP requests.
func GenUserAgent(org, prefix, infoURL string) string {
	return fmt.Sprintf(
		"%s/%s/%s (%s/%s/%s; %s; +%s) Hostname/%s",
		org, prefix, Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, infoURL,
		os.Args[0], hostname,
	)
}

// Transport wraps a http transport with user agent information.
func Transport(org, prefix, infoURL string, rt http.RoundTripper) http.RoundTripper {
	return userAgentTransport{org: org, prefix: prefix, infoURL: infoURL, rt: rt}
}

type userAgentTransport struct {
	org, prefix, infoURL string
	rt                   http.RoundTripper
}

func (uat userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("User-Agent", GenUserAgent(uat.org, uat.prefix, uat.infoURL))
	return uat.rt.RoundTrip(r)
}
