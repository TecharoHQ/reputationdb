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
