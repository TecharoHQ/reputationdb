package reputationdbv1

import (
	"testing"

	"github.com/TecharoHQ/reputationdb"
)

func TestParseAddrs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		raw       []string
		wantAddrs []string
		wantErr   bool
	}{
		{
			name:      "one IPv4 address",
			raw:       []string{"1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "one IPv6 address",
			raw:       []string{"2001:db8::1"},
			wantAddrs: []string{"2001:db8::1"},
		},
		{
			name:      "a v4-in-v6 address is unmapped for the lookup",
			raw:       []string{"::ffff:1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "duplicates collapse",
			raw:       []string{"1.2.3.4", "1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{
			name:      "a v4 address and its v4-in-v6 form are the same address",
			raw:       []string{"1.2.3.4", "::ffff:1.2.3.4"},
			wantAddrs: []string{"1.2.3.4"},
		},
		{name: "not an address", raw: []string{"nonsense"}, wantErr: true},
		{name: "a CIDR prefix", raw: []string{"1.2.3.0/24"}, wantErr: true},
		{name: "a host and port", raw: []string{"1.2.3.4:80"}, wantErr: true},
		{name: "empty string", raw: []string{""}, wantErr: true},
		{name: "one bad address among good ones", raw: []string{"1.2.3.4", "nope"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addrs, inputs, err := parseAddrs(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseAddrs(%v) error = nil, want an error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAddrs(%v) error = %v", tt.raw, err)
			}

			if len(addrs) != len(tt.wantAddrs) {
				t.Fatalf("parseAddrs(%v) = %v, want %v", tt.raw, addrs, tt.wantAddrs)
			}
			for i, want := range tt.wantAddrs {
				if addrs[i].String() != want {
					t.Errorf("parseAddrs(%v)[%d] = %s, want %s", tt.raw, i, addrs[i], want)
				}
			}
			for _, addr := range addrs {
				if _, ok := inputs[addr]; !ok {
					t.Errorf("parseAddrs(%v) has no input string recorded for %s", tt.raw, addr)
				}
			}
		})
	}
}

// The response echoes what the client wrote, not the normalized form, so a
// client can match records to requests by string equality.
func TestParseAddrsEchoesTheRequestedForm(t *testing.T) {
	addrs, inputs, err := parseAddrs([]string{"::ffff:1.2.3.4"})
	if err != nil {
		t.Fatalf("parseAddrs error = %v", err)
	}
	if got := inputs[addrs[0]]; got != "::ffff:1.2.3.4" {
		t.Errorf("inputs[%s] = %q, want the string the client sent", addrs[0], got)
	}
}

func TestToRecord(t *testing.T) {
	res := reputationdb.Result{
		IsVPN:        true,
		IsDatacenter: true,
		IsCrawler:    false,
		IsProxy:      false,
		Categories:   []string{"datacenter", "vpn"},
		Providers:    []string{"datacentres", "nordvpn"},
		Sources: []reputationdb.ListMembership{
			{
				Repository: "github.com/az0/vpn_ip",
				List:       "data/input/ip/nordvpn.txt",
				Provider:   "nordvpn",
				Category:   "vpn",
			},
		},
	}

	got := toRecord("1.2.3.4", res)

	if got.GetIpAddress() != "1.2.3.4" {
		t.Errorf("IpAddress = %q, want %q", got.GetIpAddress(), "1.2.3.4")
	}
	if !got.GetIsVpn() || !got.GetIsDatacenter() {
		t.Errorf("IsVpn = %v, IsDatacenter = %v, want both true", got.GetIsVpn(), got.GetIsDatacenter())
	}
	if got.GetIsCrawler() || got.GetIsProxy() {
		t.Errorf("IsCrawler = %v, IsProxy = %v, want both false", got.GetIsCrawler(), got.GetIsProxy())
	}
	if len(got.GetCategories()) != 2 || got.GetCategories()[0] != "datacenter" {
		t.Errorf("Categories = %v, want [datacenter vpn]", got.GetCategories())
	}
	if len(got.GetProviders()) != 2 {
		t.Errorf("Providers = %v, want two entries", got.GetProviders())
	}

	sources := got.GetSources()
	if len(sources) != 1 {
		t.Fatalf("Sources = %v, want one entry", sources)
	}
	if sources[0].GetRepository() != "github.com/az0/vpn_ip" {
		t.Errorf("Sources[0].Repository = %q, want %q", sources[0].GetRepository(), "github.com/az0/vpn_ip")
	}
	if sources[0].GetList() != "data/input/ip/nordvpn.txt" {
		t.Errorf("Sources[0].List = %q, want %q", sources[0].GetList(), "data/input/ip/nordvpn.txt")
	}
	if sources[0].GetProvider() != "nordvpn" {
		t.Errorf("Sources[0].Provider = %q, want %q", sources[0].GetProvider(), "nordvpn")
	}
	if sources[0].GetCategory() != "vpn" {
		t.Errorf("Sources[0].Category = %q, want %q", sources[0].GetCategory(), "vpn")
	}
}

// An address with no list memberships still has to produce a well-formed
// record rather than a nil sources slice the JSON encoder renders as null.
func TestToRecordWithNoSources(t *testing.T) {
	got := toRecord("1.2.3.4", reputationdb.Result{})

	if got.GetSources() == nil {
		t.Error("Sources = nil, want an empty slice")
	}
	if len(got.GetSources()) != 0 {
		t.Errorf("Sources = %v, want it empty", got.GetSources())
	}
}
