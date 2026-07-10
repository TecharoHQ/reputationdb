package main

import (
	"errors"
	"io"
	"net"
	"net/netip"

	vpnip "github.com/TecharoHQ/reputationdb"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// DatabaseType is the value written into the mmdb metadata describing the
// structure of the records in this database.
const DatabaseType = "Techaro-Veil-VPN"

// Writer is a thin, raw wrapper around an mmdbwriter [mmdbwriter.Tree]. It deals
// in raw [mmdbtype.DataType] values via [Writer.InsertRaw] and in high-level
// [vpnip.Record] values via [Writer.Insert].
type Writer struct {
	tree *mmdbwriter.Tree
}

// NewWriter creates a Writer backed by a fresh IPv6 tree (which also serves
// IPv4 lookups via aliasing).
func NewWriter() (*Writer, error) {
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: DatabaseType,
		Description: map[string]string{
			"en": "VPN, datacenter, crawler, and proxy IP addresses aggregated from public lists",
		},
		// IPv6 tree, which transparently handles IPv4 lookups too.
		IPVersion:  6,
		RecordSize: 28,
		Languages:  []string{"en"},
	})
	if err != nil {
		return nil, err
	}

	return &Writer{tree: tree}, nil
}

// InsertRaw inserts a raw mmdb value at the given network prefix. This is the
// low-level escape hatch; most callers want [Writer.Insert].
func (w *Writer) InsertRaw(prefix netip.Prefix, value mmdbtype.DataType) error {
	return w.tree.Insert(prefixToIPNet(prefix), value)
}

// Insert converts a high-level [vpnip.Record] into its raw mmdb form and inserts
// it at the given prefix.
func (w *Writer) Insert(prefix netip.Prefix, rec vpnip.Record) error {
	return w.InsertRaw(prefix, rec.DataType())
}

// WriteTo serializes the database to out, returning the number of bytes written.
func (w *Writer) WriteTo(out io.Writer) (int64, error) {
	return w.tree.WriteTo(out)
}

// IsSkippableNetwork reports whether err is one of the structural insert errors
// that simply mean a prefix is not eligible for the tree (reserved ranges such
// as 10.0.0.0/8, or networks that collide with IPv4-in-IPv6 aliasing). These are
// expected for some upstream lists and should be skipped rather than treated as
// fatal.
func IsSkippableNetwork(err error) bool {
	var reserved *mmdbwriter.ReservedNetworkError
	var aliased *mmdbwriter.AliasedNetworkError
	return errors.As(err, &reserved) || errors.As(err, &aliased)
}

// prefixToIPNet converts a [netip.Prefix] into the [*net.IPNet] expected by the
// mmdbwriter API.
func prefixToIPNet(prefix netip.Prefix) *net.IPNet {
	addr := prefix.Addr()
	return &net.IPNet{
		IP:   addr.AsSlice(),
		Mask: net.CIDRMask(prefix.Bits(), addr.BitLen()),
	}
}
