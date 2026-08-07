package reputationdb

import "strings"

// CategoryByte groups the kind of list an IP address was found on. It is stored
// on every [ListMembership] and surfaced as top-level booleans on database results
// so consumers can branch on it cheaply.
//
// An IP address can match multiple categories, consumers should take care with
// short-circuit boolean logic.
type CategoryByte byte

// Has returns true if the category of an IP address matches a given category
// range.
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

var categoryBits = []struct {
	bit  CategoryByte
	name string
}{
	{CategoryByteVPN, CategoryVPN},
	{CategoryByteDatacenter, CategoryDatacenter},
	{CategoryByteCrawler, CategoryCrawler},
	{CategoryByteProxy, CategoryProxy},
	{CategoryByteAbuse, CategoryAbuse},
	{CategoryByteTor, CategoryTor},
}

// Strings returns information about which categories a given [CategoryByte]
// correlates to.
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
