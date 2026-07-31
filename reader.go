package reputationdb

import (
	"iter"
	"net/netip"
	"slices"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// Result is the decoded record stored for an IP address in a vpnip database. It
// mirrors the on-disk schema produced by [Record.DataType].
type Result struct {
	// IsVPN reports whether the address appears on at least one VPN provider
	// list.
	IsVPN bool `maxminddb:"is_vpn"`
	// IsDatacenter reports whether the address falls within a known datacenter
	// range.
	IsDatacenter bool `maxminddb:"is_datacenter"`
	// IsCrawler reports whether the address belongs to a known crawler/bot.
	IsCrawler bool `maxminddb:"is_crawler"`
	// IsProxy reports whether the address appears on an open/free proxy list.
	IsProxy bool `maxminddb:"is_proxy"`
	// Categories is the distinct, sorted set of categories the address belongs
	// to (see the Category* constants).
	Categories []string `maxminddb:"categories"`
	// Providers is the distinct, sorted set of providers the address belongs to.
	Providers []string `maxminddb:"providers"`
	// Sources lists every upstream list/file the address was found on.
	Sources []ListMembership `maxminddb:"sources"`
}

// HasProvider reports whether the result includes the named provider.
func (r *Result) HasProvider(name string) bool {
	return slices.Contains(r.Providers, name)
}

// HasCategory reports whether the result includes the named category.
func (r *Result) HasCategory(name string) bool {
	return slices.Contains(r.Categories, name)
}

// DB is a read-only handle to a vpnip mmdb database.
type DB struct {
	reader *maxminddb.Reader
}

// Open opens the vpnip database at path for lookups. Call [DB.Close] when done.
func Open(path string) (*DB, error) {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{reader: reader}, nil
}

// OpenBytes opens a vpnip database from an in-memory buffer. The buffer must
// remain valid for the lifetime of the returned DB.
func OpenBytes(buffer []byte) (*DB, error) {
	reader, err := maxminddb.OpenBytes(buffer)
	if err != nil {
		return nil, err
	}
	return &DB{reader: reader}, nil
}

// Close releases the resources held by the database.
func (db *DB) Close() error {
	return db.reader.Close()
}

// Metadata returns the mmdb metadata (database type, build epoch, etc.).
func (db *DB) Metadata() maxminddb.Metadata {
	return db.reader.Metadata
}

// Network is one prefix stored in the database, paired with the record at it.
//
// The record is not decoded until [Network.Result] asks for it. Decoding is by
// far the expensive half of a walk — on a full database it costs several times
// what visiting every prefix does — so a caller that only wants the prefixes
// should not pay for it.
type Network struct {
	// Prefix is the network the record covers. Prefixes from the IPv4 half of
	// the tree are reported in their IPv4 form.
	Prefix netip.Prefix

	res maxminddb.Result
}

// Result decodes and returns the record stored at this network.
func (n Network) Result() (Result, error) {
	var out Result
	if err := n.res.Decode(&out); err != nil {
		return Result{}, err
	}
	return out, nil
}

// Networks returns an iterator over every network in the database that carries
// a record, together with any error that ended the walk early. Stop consuming
// the iterator as soon as a non-nil error arrives: the network paired with it
// is not meaningful.
//
// Networks without a record are skipped. So are the aliases of the IPv4 subtree
// that an IPv6 database carries, so each IPv4 prefix is visited exactly once.
func (db *DB) Networks() iter.Seq2[Network, error] {
	return func(yield func(Network, error) bool) {
		for res := range db.reader.Networks() {
			if err := res.Err(); err != nil {
				yield(Network{}, err)
				return
			}
			if !yield(Network{Prefix: res.Prefix(), res: res}, nil) {
				return
			}
		}
	}
}

// Lookup returns the record for addr. It reports found=false (with a nil error)
// when the address is not present in the database.
func (db *DB) Lookup(addr netip.Addr) (result Result, found bool, err error) {
	res := db.reader.Lookup(addr)
	if err := res.Err(); err != nil {
		return Result{}, false, err
	}
	if !res.Found() {
		return Result{}, false, nil
	}
	if err := res.Decode(&result); err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}
