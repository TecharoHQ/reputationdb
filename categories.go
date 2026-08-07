package reputationdb

import "strings"

// CategoryByte is a bitmask of the kinds of list that an IP address is on. Every
// [ListMembership] has one. Each [Record] also has one, which is the union of
// the masks of its sources.
//
// This is the only form that the database stores. It holds no category strings
// at all. A full build has tens of millions of records, so a string for each
// category on each record costs much more than these two bytes.
//
// An IP address can be in more than one category. A test for one category does
// not exclude the others.
//
// The type is uint16 because mmdb has no 8-bit integer. Six bits are in use, so
// there is room for ten more categories before the width must change.
type CategoryByte uint16

// Has reports whether the mask holds any of the categories in kind.
//
// kind can name more than one category. Has then returns true if any one of
// them is set. That is what a filter needs: -category vpn -category tor selects
// a record on either list.
func (cb CategoryByte) Has(kind CategoryByte) bool {
	return cb&kind != 0
}

// Categories that reputationdb can store.
const (
	CategoryByteVPN CategoryByte = 1 << iota
	CategoryByteDatacenter
	CategoryByteCrawler
	CategoryByteProxy
	CategoryByteAbuse
	CategoryByteTor
)

// categoryBits pairs each bit with its name. The order is alphabetical by name,
// not by bit, because [CategoryByte.Strings] reads this list in order. Its
// callers get a sorted list of names at no cost, and one mask always gives the
// same output.
var categoryBits = []struct {
	bit  CategoryByte
	name string
}{
	{CategoryByteAbuse, CategoryAbuse},
	{CategoryByteCrawler, CategoryCrawler},
	{CategoryByteDatacenter, CategoryDatacenter},
	{CategoryByteProxy, CategoryProxy},
	{CategoryByteTor, CategoryTor},
	{CategoryByteVPN, CategoryVPN},
}

// Strings returns the name of every category in the mask, in alphabetical
// order. The result is empty when no bit is set.
//
// Each call allocates, so this belongs at the edges of the system. A command
// that prints a record is one such edge. A handler that puts a record on the
// wire is another. Code that compares a record against a set of categories must
// use [CategoryByte.Has] instead.
func (cb CategoryByte) Strings() []string {
	result := make([]string, 0, len(categoryBits))
	for _, c := range categoryBits {
		if cb.Has(c.bit) {
			result = append(result, c.name)
		}
	}
	return result
}

func (cb CategoryByte) String() string {
	return strings.Join(cb.Strings(), "|")
}

// FromCategories parses a set of categories into a [CategoryByte], ignoring
// any invalid categories.
func FromCategories(categories []string) CategoryByte {
	var result CategoryByte

	for _, cat := range categories {
		switch cat {
		case CategoryVPN:
			result |= CategoryByteVPN
		case CategoryDatacenter:
			result |= CategoryByteDatacenter
		case CategoryCrawler:
			result |= CategoryByteCrawler
		case CategoryProxy:
			result |= CategoryByteProxy
		case CategoryAbuse:
			result |= CategoryByteAbuse
		case CategoryTor:
			result |= CategoryByteTor
		}
	}

	return result
}
