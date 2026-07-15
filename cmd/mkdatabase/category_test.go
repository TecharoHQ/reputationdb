package main

import (
	"slices"
	"testing"

	vpnip "github.com/TecharoHQ/reputationdb"
)

func TestParseCategories(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string // expected selected(), in allCategories order
		wantErr bool
	}{
		{
			name: "no flags selects everything",
			in:   nil,
			want: []string{"abuse", "crawler", "datacenter", "proxy", "tor", "vpn"},
		},
		{
			name: "single category",
			in:   []string{vpnip.CategoryDatacenter},
			want: []string{"datacenter"},
		},
		{
			name: "several categories are sorted into allCategories order",
			in:   []string{vpnip.CategoryVPN, vpnip.CategoryAbuse},
			want: []string{"abuse", "vpn"},
		},
		{
			name: "duplicates collapse",
			in:   []string{vpnip.CategoryDatacenter, vpnip.CategoryDatacenter},
			want: []string{"datacenter"},
		},
		{
			name:    "unknown category is an error",
			in:      []string{"datacentre"},
			wantErr: true,
		},
		{
			name:    "one bad value among good ones is an error",
			in:      []string{vpnip.CategoryDatacenter, "nope"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCategories(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCategories(%v) = %v, want error", tt.in, got.selected())
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCategories(%v): %v", tt.in, err)
			}
			if !slices.Equal(got.selected(), tt.want) {
				t.Errorf("selected() = %v, want %v", got.selected(), tt.want)
			}
		})
	}
}

func TestCategorySetAll(t *testing.T) {
	full, err := parseCategories(nil)
	if err != nil {
		t.Fatalf("parseCategories(nil): %v", err)
	}
	if !full.all() {
		t.Error("unfiltered set reported all() = false")
	}

	partial, err := parseCategories([]string{vpnip.CategoryDatacenter})
	if err != nil {
		t.Fatalf("parseCategories(datacenter): %v", err)
	}
	if partial.all() {
		t.Error("datacenter-only set reported all() = true")
	}
	if !partial.has(vpnip.CategoryDatacenter) {
		t.Error("datacenter-only set reported has(datacenter) = false")
	}
	if partial.has(vpnip.CategoryVPN) {
		t.Error("datacenter-only set reported has(vpn) = true")
	}
}

// TestCategorySetAllFalseValue pins all() to the same notion of "selected" that
// has() and selected() use. A set can hold an explicit false — nothing in the
// map[string]bool type prevents it — so counting entries rather than selections
// would call this six-entry set complete while has(vpn) reports otherwise. all()
// decides which database an artifact is labelled as, so it must not disagree.
func TestCategorySetAllFalseValue(t *testing.T) {
	cs := categorySet{
		vpnip.CategoryAbuse:      true,
		vpnip.CategoryCrawler:    true,
		vpnip.CategoryDatacenter: true,
		vpnip.CategoryProxy:      true,
		vpnip.CategoryTor:        true,
		vpnip.CategoryVPN:        false,
	}
	if len(cs) != len(allCategories) {
		t.Fatalf("test setup: len(cs) = %d, want %d entries so all() cannot pass on count alone", len(cs), len(allCategories))
	}
	if cs.has(vpnip.CategoryVPN) {
		t.Error("set with vpn=false reported has(vpn) = true")
	}
	if cs.all() {
		t.Error("set with vpn=false reported all() = true, disagreeing with has(vpn) = false")
	}
}
