package reputationdb

import "testing"

func TestCategoryByteToFromRoundtrip(t *testing.T) {
	for _, want := range []CategoryByte{
		CategoryByteVPN,
		CategoryByteDatacenter,
		CategoryByteCrawler,
		CategoryByteProxy,
		CategoryByteAbuse,
		CategoryByteTor,
		CategoryByteAbuse | CategoryByteCrawler,
		CategoryByteAbuse | CategoryByteCrawler | CategoryByteDatacenter | CategoryByteProxy | CategoryByteTor | CategoryByteVPN,
	} {
		t.Run(want.String(), func(t *testing.T) {
			st := want.Strings()
			got := FromCategories(st)

			if want != got {
				t.Logf("want: %x (%v)", byte(want), st)
				t.Logf(" got: %x (%v)", byte(got), got.Strings())
				t.Error("did not get expected information in the roundtrip")
			}
		})
	}
}
