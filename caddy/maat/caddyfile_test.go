package maat

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"

	// Registers the caddyfile config adapter and the HTTP app it adapts to.
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// TestCaddyfileAdapt is the smoke test for the matcher actually being usable
// by name from a Caddyfile: the module has to be registered as
// http.matchers.maat and its options have to survive the adapter.
func TestCaddyfileAdapt(t *testing.T) {
	const config = `:8080 {
	@datacenter {
		maat {
			server https://reputationdb.example.com
			categories datacenter
		}
	}
	respond @datacenter "no" 403

	respond "hi"
}
`

	adapter := caddyconfig.GetAdapter("caddyfile")
	if adapter == nil {
		t.Fatal("no caddyfile adapter registered")
	}

	result, warnings, err := adapter.Adapt([]byte(config), nil)
	if err != nil {
		t.Fatalf("adapting the Caddyfile: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("adapter warning: %s", w.Message)
	}

	json := string(result)
	for _, want := range []string{
		`"maat"`,
		`"server":"https://reputationdb.example.com"`,
		`"categories":["datacenter"]`,
	} {
		if !strings.Contains(json, want) {
			t.Errorf("adapted config is missing %s:\n%s", want, json)
		}
	}
}
