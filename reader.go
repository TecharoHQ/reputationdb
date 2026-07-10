package reputationdb

import (
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
