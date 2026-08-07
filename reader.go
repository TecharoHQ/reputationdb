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
	// Categories is the union of the categories of every source for this address.
	//
	// [CategoryByte.Has] is the correct way to examine this field. A comparison
	// for equality is wrong. An address on both a VPN list and a datacenter list
	// has both bits set, so it equals neither one alone.
	//
	// A caller that starts from category names must convert them to a mask once
	// with [FromCategories]. One call to [CategoryByte.Strings] for each result
	// is much slower, because each call allocates.
	Categories CategoryByte `maxminddb:"categories"`
	// Sources lists every upstream list/file the address was found on. The
	// provider names live here and nowhere else, so [Result.HasProvider] and
	// [Result.Providers] read them from this slice.
	Sources []ListMembership `maxminddb:"sources"`
}

// HasProvider reports whether the result includes the named provider.
//
// This walks the sources. An address is on a handful of lists at most, so the
// walk is cheaper than the array of names that the database would otherwise
// carry on every record.
func (r *Result) HasProvider(name string) bool {
	return slices.ContainsFunc(r.Sources, func(s ListMembership) bool {
		return s.Provider == name
	})
}

// Providers returns the distinct, sorted set of providers for this address.
//
// Each call allocates, so this belongs at the edges of the system, next to
// [CategoryByte.Strings]. Code that looks for one provider must use
// [Result.HasProvider] instead.
func (r *Result) Providers() []string {
	out := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		if !slices.Contains(out, s.Provider) {
			out = append(out, s.Provider)
		}
	}
	slices.Sort(out)
	return out
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
