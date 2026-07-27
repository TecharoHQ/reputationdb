// Package reputationdbv1 implements
// techaro.lol.reputationdb.v1.ReputationService: the endpoint that answers
// reputation questions about a batch of IP addresses.
//
// Queries are served from a local memory-mapped copy of the newest published
// database, kept fresh by dbcache. Until that copy lands — which on a cold
// start is minutes after the process comes up — queries fail with Unavailable
// rather than reporting every address as clean.
//
// This endpoint is unauthenticated today, exactly like the fetch endpoints
// are. meta.proto declares an APIKey bearer scheme, but nothing validates it
// yet.
package reputationdbv1

import (
	"fmt"
	"net/netip"

	"github.com/TecharoHQ/reputationdb"
	reputationdbv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/v1"
)

// parseAddrs turns the requested strings into the addresses to look up,
// dropping duplicates.
//
// inputs maps each parsed address back to the string the client wrote it as, so
// the response can echo what was asked about rather than a normalized form the
// client would then have to match up itself. When two spellings of the same
// address are requested — 1.2.3.4 and ::ffff:1.2.3.4 — the first one wins and
// there is one record, not two.
func parseAddrs(raw []string) (addrs []netip.Addr, inputs map[netip.Addr]string, err error) {
	inputs = make(map[netip.Addr]string, len(raw))

	for _, s := range raw {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, nil, fmt.Errorf("%q is not an IP address", s)
		}
		// Unmap so a v4-in-v6 address looks up as the IPv4 address it is.
		addr = addr.Unmap()

		if _, seen := inputs[addr]; seen {
			continue
		}
		inputs[addr] = s
		addrs = append(addrs, addr)
	}

	return addrs, inputs, nil
}

// toRecord converts a decoded database record into its protobuf form.
//
// ipAddress is the string the client asked about, not addr.String(): see
// parseAddrs.
func toRecord(ipAddress string, res reputationdb.Result) *reputationdbv1.Record {
	sources := make([]*reputationdbv1.ListMembership, 0, len(res.Sources))
	for _, s := range res.Sources {
		sources = append(sources, &reputationdbv1.ListMembership{
			Repository: s.Repository,
			List:       s.List,
			Provider:   s.Provider,
			Category:   s.Category,
		})
	}

	return &reputationdbv1.Record{
		IpAddress:    ipAddress,
		IsVpn:        res.IsVPN,
		IsDatacenter: res.IsDatacenter,
		IsCrawler:    res.IsCrawler,
		IsProxy:      res.IsProxy,
		Categories:   res.Categories,
		Providers:    res.Providers,
		Sources:      sources,
	}
}
