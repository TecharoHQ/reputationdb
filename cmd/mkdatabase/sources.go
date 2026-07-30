package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	vpnip "github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/web/ripeasn"
	"github.com/gaissmai/bart"
	"rsc.io/gitfs"
)

// cacheMaxAge is how long a cached list copy is considered fresh; older copies
// are refetched over HTTP.
const cacheMaxAge = 23 * time.Hour

// listSpec describes one glob of list files within a repository and the
// category every address found in those files should be tagged with.
type listSpec struct {
	// glob is an fs.Glob pattern (slash-separated) matched against the cloned
	// repository's file tree.
	glob string
	// category is the Category* constant applied to every address read from the
	// matched files.
	category string
	// provider optionally overrides the provider name; when empty it is derived
	// from the file path with deriveProvider.
	provider string
	// parse optionally overrides the line parser; when nil parseIPLines is used.
	parse func([]byte) []netip.Prefix
}

// repoSource describes a single upstream git repository and which of its files
// hold IP/CIDR lists.
type repoSource struct {
	// name is the canonical repository name stored on each record, e.g.
	// "github.com/az0/vpn_ip".
	name string
	// url is the clone URL passed to gitfs.
	url string
	// lists enumerates the list files of interest within the repo.
	lists []listSpec
}

// sources is the set of upstream repositories imported into the database.
var sources = []repoSource{
	{
		name: "github.com/az0/vpn_ip",
		url:  "https://github.com/az0/vpn_ip",
		lists: []listSpec{
			{glob: "data/input/ip/*.txt", category: vpnip.CategoryVPN},
		},
	},
	{
		name: "github.com/coocoobau/vpn-ip-lists",
		url:  "https://github.com/coocoobau/vpn-ip-lists",
		lists: []listSpec{
			{glob: "*-ips.txt", category: vpnip.CategoryVPN},
		},
	},
	{
		name: "github.com/tn3w/TunnelBear-IPs",
		url:  "https://github.com/tn3w/TunnelBear-IPs",
		lists: []listSpec{
			{glob: "tunnelbear_ips.txt", category: vpnip.CategoryVPN},
		},
	},
	{
		name: "github.com/hexydec/ip-ranges",
		url:  "https://github.com/hexydec/ip-ranges",
		lists: []listSpec{
			{glob: "output/datacentres.txt", category: vpnip.CategoryDatacenter},
			{glob: "output/crawlers.txt", category: vpnip.CategoryCrawler},
		},
	},
	{
		name: "github.com/proxifly/free-proxy-list",
		url:  "https://github.com/proxifly/free-proxy-list",
		lists: []listSpec{
			{
				glob:     "proxies/all/data.txt",
				category: vpnip.CategoryProxy,
				provider: "proxifly",
				parse:    parseProxyURLs,
			},
		},
	},
	{
		name: "github.com/lorenzozuliani-sudo/tiktok-ips-list",
		url:  "https://github.com/lorenzozuliani-sudo/tiktok-ips-list",
		lists: []listSpec{
			{
				glob:     "bytedance_ips.json",
				category: vpnip.CategoryDatacenter,
				provider: "bytedance",
				parse:    parseGoogleStyleJSON,
			},
		},
	},
	{
		name: "github.com/SoliSpirit/proxy-list",
		url:  "https://github.com/SoliSpirit/proxy-list",
		lists: []listSpec{
			{glob: "**/*.txt", category: vpnip.CategoryProxy, provider: "solispirit", parse: parseColonHost},
		},
	},
	{
		name: "github.com/zloi-user/hideip.me",
		url:  "https://github.com/zloi-user/hideip.me",
		lists: []listSpec{
			{glob: "*.txt", category: vpnip.CategoryProxy, provider: "hideip", parse: parseColonHost},
		},
	},
	{
		name: "github.com/VPSLabCloud/VPSLab-Free-Proxy-List",
		url:  "https://github.com/VPSLabCloud/VPSLab-Free-Proxy-List",
		lists: []listSpec{
			{glob: "*.txt", category: vpnip.CategoryProxy, provider: "vpslab", parse: parseColonHost},
		},
	},
	{
		name: "github.com/komutan234/Proxy-List-Free",
		url:  "https://github.com/komutan234/Proxy-List-Free",
		lists: []listSpec{
			{glob: "proxies/*.txt", category: vpnip.CategoryProxy, provider: "komutan", parse: parseColonHost},
		},
	},
	{
		name: "github.com/TheSpeedX/PROXY-List",
		url:  "https://github.com/TheSpeedX/PROXY-List",
		lists: []listSpec{
			{glob: "proxy-list/*.txt", category: vpnip.CategoryProxy, provider: "speedx", parse: parseColonHost},
		},
	},
	{
		name: "github.com/stormsia/proxy-list",
		url:  "https://github.com/stormsia/proxy-list",
		lists: []listSpec{
			{glob: "working_proxies.txt", category: vpnip.CategoryProxy, provider: "stormsia", parse: parseColonHost},
		},
	},
	{
		name: "github.com/ebrasha/abdal-proxy-hub",
		url:  "https://github.com/ebrasha/abdal-proxy-hub",
		lists: []listSpec{
			{glob: "*.txt", category: vpnip.CategoryProxy, provider: "ebrasha", parse: parseColonHost},
		},
	},
	{
		name: "github.com/handeveloper1/DailyProxy---Auto-Update-List",
		url:  "https://github.com/handeveloper1/DailyProxy---Auto-Update-List",
		lists: []listSpec{
			{glob: "**/*.txt", category: vpnip.CategoryProxy, provider: "dailyproxy", parse: parseColonHost},
		},
	},
	{
		name: "github.com/hendrikbgr/Free-Proxy-Repo",
		url:  "https://github.com/hendrikbgr/Free-Proxy-Repo",
		lists: []listSpec{
			{glob: "proxy_list.txt", category: vpnip.CategoryProxy, provider: "hendrikbgr", parse: parseColonHost},
		},
	},
}

// httpSource describes a single upstream IP/CIDR list served as a plain text
// file over HTTP rather than living in a git repository.
type httpSource struct {
	// name is the canonical source name stored on each record, e.g.
	// "threathive.net".
	name string
	// url is the HTTP(S) URL the list is downloaded from.
	url string
	// provider is the provider name stored on each record.
	provider string
	// category is the Category* constant applied to every address in the list.
	category string
	// parse optionally overrides the line parser; when nil parseIPLines is used.
	parse func([]byte) []netip.Prefix
	// parseGrouped optionally parses the list into prefixes keyed by a
	// per-group provider name (e.g. one bucket per cloud region). When set it
	// takes precedence over parse and provider: each group is folded under its
	// own provider while sharing the source's repository, list, and category.
	parseGrouped func([]byte) map[string][]netip.Prefix
}

// httpSources is the set of plain-text HTTP list files imported into the
// database.
var httpSources = []httpSource{
	{
		name:     "threathive.net",
		url:      "https://threathive.net/hiveblocklist.txt",
		provider: "threathive",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "ipinsights.io",
		url:      "https://www.ipinsights.io/downloads/blocklist-cidr.txt",
		provider: "ipinsights",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/bitwire-it/ipblocklist",
		url:      "https://raw.githubusercontent.com/bitwire-it/ipblocklist/refs/heads/main/inbound.txt",
		provider: "bitwire",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/juergen2025sys/NETSHIELD",
		url:      "https://github.com/juergen2025sys/NETSHIELD/raw/refs/heads/main/bot_detector_blacklist_ipv4.txt",
		provider: "netshield",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/juergen2025sys/NETSHIELD",
		url:      "https://github.com/juergen2025sys/NETSHIELD/raw/refs/heads/main/blacklist_confidence40_ipv4.txt",
		provider: "netshield",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/cbuijs/badip",
		url:      "https://github.com/cbuijs/badip/raw/refs/heads/main/badip.list",
		provider: "cbuijs",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/NETMOUNTAINS/Curated-IP-Blocklist",
		url:      "https://raw.githubusercontent.com/NETMOUNTAINS/Curated-IP-Blocklist/refs/heads/main/ip-blacklist.list",
		provider: "netmountains",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/MagicTeaMC/bad-ips",
		url:      "https://github.com/MagicTeaMC/bad-ips/raw/refs/heads/main/bad-ips.txt",
		provider: "magicteamc",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/MagicTeaMC/bad-ips",
		url:      "https://github.com/MagicTeaMC/bad-ips/raw/refs/heads/main/bad-ips-v6.txt",
		provider: "magicteamc",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "iplists.firehol.org",
		url:      "https://iplists.firehol.org/files/firehol_level1.netset",
		provider: "firehol-level1",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/kraloveckey/ipsets-blocklist",
		url:      "https://raw.githubusercontent.com/kraloveckey/ipsets-blocklist/refs/heads/main/botscout_30d.ipset",
		provider: "botscout",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/kawaiipantsu/ip-blacklist-collection",
		url:      "https://raw.githubusercontent.com/kawaiipantsu/ip-blacklist-collection/refs/heads/main/services/tor-exitnodes.csv",
		provider: "tor",
		category: vpnip.CategoryTor,
		parse:    parseCSVFirstField,
	},
	{
		// Ported from JasonLovesDoggo/caddy-defender's ranges/fetchers/tor.go.
		name:     "github.com/alireza-rezaee/tor-nodes",
		url:      "https://cdn.jsdelivr.net/gh/alireza-rezaee/tor-nodes@main/latest.exits.csv",
		provider: "tor",
		category: vpnip.CategoryTor,
		parse:    parseCSVSecondField,
	},
	{
		name:     "github.com/elliotwutingfeng/ThreatFox-IOC-IPs",
		url:      "https://raw.githubusercontent.com/elliotwutingfeng/ThreatFox-IOC-IPs/refs/heads/main/ips.txt",
		provider: "threatfox",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "github.com/hproxy-com/free-proxy-list",
		url:      "https://raw.githubusercontent.com/hproxy-com/free-proxy-list/refs/heads/main/all.txt",
		provider: "hproxy",
		category: vpnip.CategoryProxy,
		parse:    parseColonHost,
	},
	{
		name:     "github.com/fyvri/fresh-proxy-list",
		url:      "https://raw.githubusercontent.com/fyvri/fresh-proxy-list/archive/storage/classic/all.txt",
		provider: "fyvri",
		category: vpnip.CategoryProxy,
		parse:    parseColonHost,
	},
	{
		name:     "api.proxyscrape.com",
		url:      "https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=ipport&format=text",
		provider: "proxyscrape",
		category: vpnip.CategoryProxy,
		parse:    parseColonHost,
	},
	{
		name:     "github.com/TheSpeedX/SOCKS-List",
		url:      "https://raw.githubusercontent.com/TheSpeedX/SOCKS-List/master/socks5.txt",
		provider: "speedx",
		category: vpnip.CategoryProxy,
		parse:    parseColonHost,
	},
	{
		name:     "geofeed.constant.com",
		url:      "https://geofeed.constant.com/?text",
		provider: "vultr",
		category: vpnip.CategoryDatacenter,
	},
	{
		name:     "digitalocean.com",
		url:      "https://digitalocean.com/geo/google.csv",
		provider: "digitalocean",
		category: vpnip.CategoryDatacenter,
		parse:    parseCSVFirstField,
	},
	{
		name:     "github.com/CTI-Buddy/BLACKWALL",
		url:      "https://raw.githubusercontent.com/CTI-Buddy/BLACKWALL/refs/heads/main/THEBLACKWALL.txt",
		provider: "blackwall",
		category: vpnip.CategoryAbuse,
	},
	{
		name:     "ip-ranges.amazonaws.com",
		url:      "https://ip-ranges.amazonaws.com/ip-ranges.json",
		provider: "aws",
		category: vpnip.CategoryDatacenter,
		parse:    parseAWSJSON,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/github/github_ips.json",
		provider: "github_actions",
		category: vpnip.CategoryDatacenter,
		parse:    parseGitHubActions,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/hetzner/hetzner_ips.txt",
		provider: "hetzner",
		category: vpnip.CategoryDatacenter,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://raw.githubusercontent.com/rezmoss/cloud-provider-ip-addresses/refs/heads/main/scaleway/scaleway_ips.txt",
		provider: "scaleway",
		category: vpnip.CategoryDatacenter,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://raw.githubusercontent.com/rezmoss/cloud-provider-ip-addresses/refs/heads/main/tor/tor_ips.txt",
		provider: "tor",
		category: vpnip.CategoryTor,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/ibmcloud/ibmcloud_ips.json",
		provider: "ibmcloud",
		category: vpnip.CategoryDatacenter,
		parse:    parseIPAddressJSON,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/ovhcloud/ovhcloud_ips.json",
		provider: "ovhcloud",
		category: vpnip.CategoryDatacenter,
		parse:    parseIPAddressJSON,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/fastly/fastly_ips.json",
		provider: "fastly",
		category: vpnip.CategoryDatacenter,
		parse:    parseIPAddressJSON,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/googlebot/googlebot_ips.txt",
		provider: "googlebot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/bingbot/bingbot_ips.txt",
		provider: "bingbot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/duckduckbot/duckduckbot_ips.txt",
		provider: "duckduckbot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/applebot/applebot_ips.txt",
		provider: "applebot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/amazonbot/amazonbot_ips.txt",
		provider: "amazonbot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/gptbot/gptbot_ips.txt",
		provider: "gptbot",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/perplexitybot/perplexitybot_ips.txt",
		provider: "perplexitybot",
		category: vpnip.CategoryCrawler,
	},
	// AI crawler/service ranges, ported from JasonLovesDoggo/caddy-defender's
	// ranges/fetchers package (openai.go, mistral.go, github.go).
	{
		name:     "openai.com",
		url:      "https://openai.com/searchbot.json",
		provider: "openai",
		category: vpnip.CategoryCrawler,
		parse:    parseGoogleStyleJSON,
	},
	{
		name:     "openai.com",
		url:      "https://openai.com/chatgpt-user.json",
		provider: "openai",
		category: vpnip.CategoryCrawler,
		parse:    parseGoogleStyleJSON,
	},
	{
		name:     "openai.com",
		url:      "https://openai.com/gptbot.json",
		provider: "openai",
		category: vpnip.CategoryCrawler,
		parse:    parseGoogleStyleJSON,
	},
	{
		name:     "mistral.ai",
		url:      "https://mistral.ai/mistralai-user-ips.json",
		provider: "mistral",
		category: vpnip.CategoryCrawler,
		parse:    parseGoogleStyleJSON,
	},
	{
		name:     "api.github.com",
		url:      "https://api.github.com/meta",
		provider: "github_copilot",
		category: vpnip.CategoryCrawler,
		parse:    parseGitHubMetaKey("copilot"),
	},
	{
		name:     "github.com/rezmoss/cloud-provider-ip-addresses",
		url:      "https://github.com/rezmoss/cloud-provider-ip-addresses/raw/refs/heads/main/mullvad/mullvad_ips.txt",
		provider: "mullvad",
		category: vpnip.CategoryVPN,
	},
	{
		// Ported from JasonLovesDoggo/caddy-defender's ranges/fetchers/vpn.go.
		name:     "github.com/X4BNet/lists_vpn",
		url:      "https://cdn.jsdelivr.net/gh/X4BNet/lists_vpn@main/output/vpn/ipv4.txt",
		provider: "x4bnet",
		category: vpnip.CategoryVPN,
	},
	{
		// Hurricane Electric's free IPv6 tunnel broker, exported as an RFC 8805
		// geofeed CSV (prefix in the first field). Tunnel endpoints egress traffic
		// through HE's network, so they behave like a VPN tunnel.
		name:     "tunnelbroker.net",
		url:      "https://tunnelbroker.net/export/google",
		provider: "tunnelbroker",
		category: vpnip.CategoryVPN,
		parse:    parseCSVFirstField,
	},
	// First-party / canonical upstreams for cloud providers. These authoritative
	// feeds supersede the third-party rezmoss republish (which is preferred only
	// for providers that publish no direct feed), per the source priority:
	// official provider source > AS parsing > third-party IP list.
	{
		name:     "gstatic.com",
		url:      "https://www.gstatic.com/ipranges/cloud.json",
		provider: "googlecloud",
		category: vpnip.CategoryDatacenter,
		parse:    parseGoogleStyleJSON,
	},
	{
		name:     "docs.oracle.com",
		url:      "https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json",
		provider: "oracle",
		category: vpnip.CategoryDatacenter,
		parse:    parseOracleJSON,
	},
	{
		name:     "github.com/femueller/cloud-ip-ranges",
		url:      "https://raw.githubusercontent.com/femueller/cloud-ip-ranges/refs/heads/master/microsoft-azure-ip-ranges.json",
		provider: "azure",
		category: vpnip.CategoryDatacenter,
		parse:    parseAzureJSON,
	},
	{
		name:     "geoip.linode.com",
		url:      "https://geoip.linode.com/",
		provider: "linode",
		category: vpnip.CategoryDatacenter,
		parse:    parseCSVFirstField,
	},
	{
		name:     "cloudflare.com",
		url:      "https://www.cloudflare.com/ips-v4",
		provider: "cloudflare",
		category: vpnip.CategoryDatacenter,
	},
	{
		name:     "cloudflare.com",
		url:      "https://www.cloudflare.com/ips-v6",
		provider: "cloudflare",
		category: vpnip.CategoryDatacenter,
	},
	{
		name:     "mask-api.icloud.com",
		url:      "https://mask-api.icloud.com/egress-ip-ranges.csv",
		provider: "apple_private_relay",
		category: vpnip.CategoryProxy,
		parse:    parseCSVFirstField,
	},
}

// fileSource describes a single plain-text list file read directly from the local
// filesystem, as opposed to a git repository (repoSource), an HTTP URL
// (httpSource), or an ASN (asnSource). Paths are relative to the directory the
// command is run from (normally the repository root).
type fileSource struct {
	// name is the canonical source name stored on each record's Repository field,
	// e.g. "fdo".
	name string
	// path is the filesystem path to the list file, relative to the working
	// directory.
	path string
	// provider is the provider name stored on each record.
	provider string
	// category is the Category* constant applied to every address in the list.
	category string
	// parse optionally overrides the line parser; when nil parseIPLines is used.
	parse func([]byte) []netip.Prefix
}

// fileSources is the set of local filesystem list files imported into the
// database.
var fileSources = []fileSource{
	{
		name:     "fdo",
		path:     "data/manually-submitted/fdo/ips.txt",
		provider: "fdo",
		category: vpnip.CategoryAbuse,
	},
	{
		// Ported from JasonLovesDoggo/caddy-defender's ranges/fetchers/deepseek.go,
		// which hardcodes these addresses since DeepSeek publishes no feed for them.
		name:     "deepseek",
		path:     "data/manually-submitted/deepseek/ips.txt",
		provider: "deepseek",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "sourceware_honeypot",
		path:     "data/manually-submitted/sourceware/ips.txt",
		provider: "sourceware",
		category: vpnip.CategoryCrawler,
	},
	{
		name:     "sourceware_honeypot",
		path:     "data/manually-submitted/sourceware/202607141625.txt",
		provider: "sourceware",
		category: vpnip.CategoryCrawler,
	},
	{
		// Huawei Cloud (Mexico) ranges caught abusively scraping cgit on
		// sourceware.org.
		name:     "sourceware_honeypot",
		path:     "data/manually-submitted/sourceware/20260723-huawei-cloud-mx.txt",
		provider: "sourceware",
		category: vpnip.CategoryAbuse,
	},
	{
		// Range impersonating Googlebot while abusively scraping cgit on
		// sourceware.org.
		name:     "sourceware_honeypot",
		path:     "data/manually-submitted/sourceware/20260723-rogue-googlebot.txt",
		provider: "sourceware",
		category: vpnip.CategoryAbuse,
	},
	{
		// Addresses caught by Xe's Limsa Lominsa honeypot on 2026-07-30.
		name:     "20260730-limsa-lominsa-honeypot",
		path:     "data/manually-submitted/xe/20260730-limsa-lominsa-honeypot.txt",
		provider: "techaro",
		category: vpnip.CategoryAbuse,
	},

	// Per-network blackhole lists submitted by nochan on 2026-07-14, one file per
	// network. Every list names the network it blackholes, so each entry keeps
	// that name and is categorised by what the network is: hosting/CDN operators
	// as datacenter, scanning and intelligence outfits as crawler, VPN and proxy
	// services as such, and everything else -- ISPs, country rollups, and one-off
	// problem networks -- as abuse, which is why they were blackholed.
	{name: "1337", path: "data/manually-submitted/nochan/20260714_network_blackholes/_1337.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "1gservers", path: "data/manually-submitted/nochan/20260714_network_blackholes/_1gservers.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "3xk_tech", path: "data/manually-submitted/nochan/20260714_network_blackholes/_3xk_tech.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "active_fiber_za", path: "data/manually-submitted/nochan/20260714_network_blackholes/_active_fiber_za.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "adamo_tele", path: "data/manually-submitted/nochan/20260714_network_blackholes/_adamo_tele.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "advin_services", path: "data/manually-submitted/nochan/20260714_network_blackholes/_advin_services.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aeza", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aeza.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aeza_group", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aeza_group.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "africa_on_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_africa_on_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "alentus", path: "data/manually-submitted/nochan/20260714_network_blackholes/_alentus.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "alibaba", path: "data/manually-submitted/nochan/20260714_network_blackholes/_alibaba.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "all_brazil", path: "data/manually-submitted/nochan/20260714_network_blackholes/_all_brazil.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "alpinedc", path: "data/manually-submitted/nochan/20260714_network_blackholes/_alpinedc.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "altushost", path: "data/manually-submitted/nochan/20260714_network_blackholes/_altushost.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "alviva_holding", path: "data/manually-submitted/nochan/20260714_network_blackholes/_alviva_holding.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aplikanusa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aplikanusa.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "arelion", path: "data/manually-submitted/nochan/20260714_network_blackholes/_arelion.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "arosscloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_arosscloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aruba", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aruba.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aurologic", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aurologic.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "automattic", path: "data/manually-submitted/nochan/20260714_network_blackholes/_automattic.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "aws", path: "data/manually-submitted/nochan/20260714_network_blackholes/_aws.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "bangladesh_working_group", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bangladesh_working_group.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "beijing_tiantexin", path: "data/manually-submitted/nochan/20260714_network_blackholes/_beijing_tiantexin.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "beijing_volcano", path: "data/manually-submitted/nochan/20260714_network_blackholes/_beijing_volcano.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "bharti_airtel", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bharti_airtel.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "bigglobe", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bigglobe.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "bitsight", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bitsight.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "black_mesa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_black_mesa.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "blix", path: "data/manually-submitted/nochan/20260714_network_blackholes/_blix.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "bluevps", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bluevps.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "brisanet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_brisanet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "buddy_software", path: "data/manually-submitted/nochan/20260714_network_blackholes/_buddy_software.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "bulgaria", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bulgaria.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "bulgaria_ead", path: "data/manually-submitted/nochan/20260714_network_blackholes/_bulgaria_ead.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "carinet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_carinet.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "cdn77", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cdn77.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "cdnext", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cdnext.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "cds_global_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cds_global_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "censys", path: "data/manually-submitted/nochan/20260714_network_blackholes/_censys.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "cherry_servers", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cherry_servers.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "china_mobile", path: "data/manually-submitted/nochan/20260714_network_blackholes/_china_mobile.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "china_telecom_beijing_hebei", path: "data/manually-submitted/nochan/20260714_network_blackholes/_china_telecom_beijing_hebei.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "china_telecom_global", path: "data/manually-submitted/nochan/20260714_network_blackholes/_china_telecom_global.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "chinanet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_chinanet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "chinanet_fujian", path: "data/manually-submitted/nochan/20260714_network_blackholes/_chinanet_fujian.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "chinaunicom", path: "data/manually-submitted/nochan/20260714_network_blackholes/_chinaunicom.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "chungwa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_chungwa.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "cloudbackbone", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cloudbackbone.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "cloudflare", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cloudflare.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "clouvider", path: "data/manually-submitted/nochan/20260714_network_blackholes/_clouvider.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "code_m", path: "data/manually-submitted/nochan/20260714_network_blackholes/_code_m.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "cogentco", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cogentco.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "colocatel", path: "data/manually-submitted/nochan/20260714_network_blackholes/_colocatel.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "colocrossing", path: "data/manually-submitted/nochan/20260714_network_blackholes/_colocrossing.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "community_antenna_service__fishy", path: "data/manually-submitted/nochan/20260714_network_blackholes/_community_antenna_service__fishy.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "completeweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_completeweb.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "constant", path: "data/manually-submitted/nochan/20260714_network_blackholes/_constant.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "constantcompany", path: "data/manually-submitted/nochan/20260714_network_blackholes/_constantcompany.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "constantine_cybersecurity", path: "data/manually-submitted/nochan/20260714_network_blackholes/_constantine_cybersecurity.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "contabo", path: "data/manually-submitted/nochan/20260714_network_blackholes/_contabo.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "country_china", path: "data/manually-submitted/nochan/20260714_network_blackholes/_country_china.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "cyber_pakistan", path: "data/manually-submitted/nochan/20260714_network_blackholes/_cyber_pakistan.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "data_campus", path: "data/manually-submitted/nochan/20260714_network_blackholes/_data_campus.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "datacamp_ltd", path: "data/manually-submitted/nochan/20260714_network_blackholes/_datacamp_ltd.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "dedik_services", path: "data/manually-submitted/nochan/20260714_network_blackholes/_dedik_services.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "default_blackholes", path: "data/manually-submitted/nochan/20260714_network_blackholes/_default_blackholes.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "delis", path: "data/manually-submitted/nochan/20260714_network_blackholes/_delis.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "digitalocean", path: "data/manually-submitted/nochan/20260714_network_blackholes/_digitalocean.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "dinet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_dinet.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "domaintools", path: "data/manually-submitted/nochan/20260714_network_blackholes/_domaintools.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "dreamhost", path: "data/manually-submitted/nochan/20260714_network_blackholes/_dreamhost.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "driftnet_intelligence", path: "data/manually-submitted/nochan/20260714_network_blackholes/_driftnet_intelligence.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "eagle_redes_br", path: "data/manually-submitted/nochan/20260714_network_blackholes/_eagle_redes_br.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ebox", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ebox.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ecotel", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ecotel.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "eisenia_ab", path: "data/manually-submitted/nochan/20260714_network_blackholes/_eisenia_ab.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ekabi", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ekabi.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ekaterinburg_2000", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ekaterinburg_2000.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "elitework", path: "data/manually-submitted/nochan/20260714_network_blackholes/_elitework.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "eonix", path: "data/manually-submitted/nochan/20260714_network_blackholes/_eonix.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "equinix", path: "data/manually-submitted/nochan/20260714_network_blackholes/_equinix.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "esds_ai", path: "data/manually-submitted/nochan/20260714_network_blackholes/_esds_ai.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "etop", path: "data/manually-submitted/nochan/20260714_network_blackholes/_etop.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "eugamehost", path: "data/manually-submitted/nochan/20260714_network_blackholes/_eugamehost.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "explorenet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_explorenet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "fastly", path: "data/manually-submitted/nochan/20260714_network_blackholes/_fastly.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "fdcservers", path: "data/manually-submitted/nochan/20260714_network_blackholes/_fdcservers.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "feo_srl", path: "data/manually-submitted/nochan/20260714_network_blackholes/_feo_srl.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "first_american_payment", path: "data/manually-submitted/nochan/20260714_network_blackholes/_first_american_payment.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "firstserver", path: "data/manually-submitted/nochan/20260714_network_blackholes/_firstserver.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "florian_kolb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_florian_kolb.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "forcepoint_llc", path: "data/manually-submitted/nochan/20260714_network_blackholes/_forcepoint_llc.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "forte_telecom_br", path: "data/manually-submitted/nochan/20260714_network_blackholes/_forte_telecom_br.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "foundation_priv", path: "data/manually-submitted/nochan/20260714_network_blackholes/_foundation_priv.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "fourplex", path: "data/manually-submitted/nochan/20260714_network_blackholes/_fourplex.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "frantech", path: "data/manually-submitted/nochan/20260714_network_blackholes/_frantech.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "freerange_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_freerange_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "g_core_labs", path: "data/manually-submitted/nochan/20260714_network_blackholes/_g_core_labs.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "german_shit", path: "data/manually-submitted/nochan/20260714_network_blackholes/_german_shit.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ghosty_networks", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ghosty_networks.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "gleam", path: "data/manually-submitted/nochan/20260714_network_blackholes/_gleam.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "glesys", path: "data/manually-submitted/nochan/20260714_network_blackholes/_glesys.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "global_connect", path: "data/manually-submitted/nochan/20260714_network_blackholes/_global_connect.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "globalconnect_ab", path: "data/manually-submitted/nochan/20260714_network_blackholes/_globalconnect_ab.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "godaddy", path: "data/manually-submitted/nochan/20260714_network_blackholes/_godaddy.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "golden_myanmar", path: "data/manually-submitted/nochan/20260714_network_blackholes/_golden_myanmar.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "google", path: "data/manually-submitted/nochan/20260714_network_blackholes/_google.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "google_dns_crap", path: "data/manually-submitted/nochan/20260714_network_blackholes/_google_dns_crap.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "googlecloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_googlecloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hangzhou", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hangzhou.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "hernlabs", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hernlabs.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "hetzner", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hetzner.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "highvelocity", path: "data/manually-submitted/nochan/20260714_network_blackholes/_highvelocity.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hill_county_telephone_coop", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hill_county_telephone_coop.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "hivelocity", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hivelocity.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "horizon_iq", path: "data/manually-submitted/nochan/20260714_network_blackholes/_horizon_iq.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "host_africa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_host_africa.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostbaltic", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostbaltic.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostdime", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostdime.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostinger", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostinger.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostpapa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostpapa.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostroyal", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostroyal.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hostwinds", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hostwinds.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "hotrush", path: "data/manually-submitted/nochan/20260714_network_blackholes/_hotrush.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "huawei", path: "data/manually-submitted/nochan/20260714_network_blackholes/_huawei.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "huawei_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_huawei_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ifog", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ifog.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "incognet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_incognet.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "internet_archive", path: "data/manually-submitted/nochan/20260714_network_blackholes/_internet_archive.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "internet_solutions", path: "data/manually-submitted/nochan/20260714_network_blackholes/_internet_solutions.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "internet_vikings", path: "data/manually-submitted/nochan/20260714_network_blackholes/_internet_vikings.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "interserver", path: "data/manually-submitted/nochan/20260714_network_blackholes/_interserver.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "intraline", path: "data/manually-submitted/nochan/20260714_network_blackholes/_intraline.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ionos", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ionos.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ip_volume", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ip_volume.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ipip_country_iq", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ipip_country_iq.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "iq_stress", path: "data/manually-submitted/nochan/20260714_network_blackholes/_iq_stress.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "jsc_silknet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_jsc_silknet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "julian_achter", path: "data/manually-submitted/nochan/20260714_network_blackholes/_julian_achter.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "kan_gurgen", path: "data/manually-submitted/nochan/20260714_network_blackholes/_kan_gurgen.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "kinamo", path: "data/manually-submitted/nochan/20260714_network_blackholes/_kinamo.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "koddos", path: "data/manually-submitted/nochan/20260714_network_blackholes/_koddos.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "koom", path: "data/manually-submitted/nochan/20260714_network_blackholes/_koom.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "korea_telcom", path: "data/manually-submitted/nochan/20260714_network_blackholes/_korea_telcom.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "krypt", path: "data/manually-submitted/nochan/20260714_network_blackholes/_krypt.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "kyivstar", path: "data/manually-submitted/nochan/20260714_network_blackholes/_kyivstar.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "lantointernet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_lantointernet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "latitude_sh_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_latitude_sh_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "leaseweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_leaseweb.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "lightedge", path: "data/manually-submitted/nochan/20260714_network_blackholes/_lightedge.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "lighthedge", path: "data/manually-submitted/nochan/20260714_network_blackholes/_lighthedge.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "limenet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_limenet.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "limestone_networks", path: "data/manually-submitted/nochan/20260714_network_blackholes/_limestone_networks.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "linkedin", path: "data/manually-submitted/nochan/20260714_network_blackholes/_linkedin.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "linkweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_linkweb.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "linode", path: "data/manually-submitted/nochan/20260714_network_blackholes/_linode.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "liquidtelecom", path: "data/manually-submitted/nochan/20260714_network_blackholes/_liquidtelecom.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "logicweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_logicweb.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "long_van_soft", path: "data/manually-submitted/nochan/20260714_network_blackholes/_long_van_soft.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "low_visibility_routes", path: "data/manually-submitted/nochan/20260714_network_blackholes/_low_visibility_routes.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "lumen", path: "data/manually-submitted/nochan/20260714_network_blackholes/_lumen.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "maxiplace", path: "data/manually-submitted/nochan/20260714_network_blackholes/_maxiplace.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "mevspace", path: "data/manually-submitted/nochan/20260714_network_blackholes/_mevspace.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "microsoft", path: "data/manually-submitted/nochan/20260714_network_blackholes/_microsoft.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "misc_scanners", path: "data/manually-submitted/nochan/20260714_network_blackholes/_misc_scanners.ipset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "misc_scanners", path: "data/manually-submitted/nochan/20260714_network_blackholes/_misc_scanners.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "modat", path: "data/manually-submitted/nochan/20260714_network_blackholes/_modat.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "monkeybrains", path: "data/manually-submitted/nochan/20260714_network_blackholes/_monkeybrains.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "mullvad", path: "data/manually-submitted/nochan/20260714_network_blackholes/_mullvad.netset", provider: "nochan", category: vpnip.CategoryVPN},
	{name: "namecheap", path: "data/manually-submitted/nochan/20260714_network_blackholes/_namecheap.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "national_institute_of_health_nih", path: "data/manually-submitted/nochan/20260714_network_blackholes/_national_institute_of_health_nih.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "netartgroup", path: "data/manually-submitted/nochan/20260714_network_blackholes/_netartgroup.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "netcup_de", path: "data/manually-submitted/nochan/20260714_network_blackholes/_netcup_de.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "netgenwebs", path: "data/manually-submitted/nochan/20260714_network_blackholes/_netgenwebs.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "netresearch", path: "data/manually-submitted/nochan/20260714_network_blackholes/_netresearch.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "nubacloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_nubacloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "nubes", path: "data/manually-submitted/nochan/20260714_network_blackholes/_nubes.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "omegatech", path: "data/manually-submitted/nochan/20260714_network_blackholes/_omegatech.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "openai", path: "data/manually-submitted/nochan/20260714_network_blackholes/_openai.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "opendns", path: "data/manually-submitted/nochan/20260714_network_blackholes/_opendns.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "optibounce", path: "data/manually-submitted/nochan/20260714_network_blackholes/_optibounce.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "oracle_cloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_oracle_cloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ovh", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ovh.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "oxylabs", path: "data/manually-submitted/nochan/20260714_network_blackholes/_oxylabs.netset", provider: "nochan", category: vpnip.CategoryProxy},
	{name: "oy_crea", path: "data/manually-submitted/nochan/20260714_network_blackholes/_oy_crea.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "packethub", path: "data/manually-submitted/nochan/20260714_network_blackholes/_packethub.netset", provider: "nochan", category: vpnip.CategoryProxy},
	{name: "paloalto_networks", path: "data/manually-submitted/nochan/20260714_network_blackholes/_paloalto_networks.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "paradise_networks", path: "data/manually-submitted/nochan/20260714_network_blackholes/_paradise_networks.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "pfcloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_pfcloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "play_p4", path: "data/manually-submitted/nochan/20260714_network_blackholes/_play_p4.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "powerhouse_mgmt", path: "data/manually-submitted/nochan/20260714_network_blackholes/_powerhouse_mgmt.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "pptech", path: "data/manually-submitted/nochan/20260714_network_blackholes/_pptech.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "private_layer", path: "data/manually-submitted/nochan/20260714_network_blackholes/_private_layer.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "proton", path: "data/manually-submitted/nochan/20260714_network_blackholes/_proton.netset", provider: "nochan", category: vpnip.CategoryVPN},
	{name: "proximus", path: "data/manually-submitted/nochan/20260714_network_blackholes/_proximus.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "psychz", path: "data/manually-submitted/nochan/20260714_network_blackholes/_psychz.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "pt_aurora", path: "data/manually-submitted/nochan/20260714_network_blackholes/_pt_aurora.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "qualys", path: "data/manually-submitted/nochan/20260714_network_blackholes/_qualys.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "quickpacket", path: "data/manually-submitted/nochan/20260714_network_blackholes/_quickpacket.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "rachamim", path: "data/manually-submitted/nochan/20260714_network_blackholes/_rachamim.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "radiant_communication", path: "data/manually-submitted/nochan/20260714_network_blackholes/_radiant_communication.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "railway", path: "data/manually-submitted/nochan/20260714_network_blackholes/_railway.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "reliablesite", path: "data/manually-submitted/nochan/20260714_network_blackholes/_reliablesite.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "resec_ai_prosperous", path: "data/manually-submitted/nochan/20260714_network_blackholes/_resec_ai_prosperous.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "rices_private", path: "data/manually-submitted/nochan/20260714_network_blackholes/_rices_private.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "rocket_science_group", path: "data/manually-submitted/nochan/20260714_network_blackholes/_rocket_science_group.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "router_hosting", path: "data/manually-submitted/nochan/20260714_network_blackholes/_router_hosting.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ru", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ru.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ru_js", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ru_js.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "rwth", path: "data/manually-submitted/nochan/20260714_network_blackholes/_rwth.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "sakura", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sakura.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "scaleway", path: "data/manually-submitted/nochan/20260714_network_blackholes/_scaleway.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "scan.bufferover.run", path: "data/manually-submitted/nochan/20260714_network_blackholes/_scan.bufferover.run.ipset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "secfirewalas", path: "data/manually-submitted/nochan/20260714_network_blackholes/_secfirewalas.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "securitytrails", path: "data/manually-submitted/nochan/20260714_network_blackholes/_securitytrails.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "sefroyek_pardaz_engineering", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sefroyek_pardaz_engineering.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "selaras_citra", path: "data/manually-submitted/nochan/20260714_network_blackholes/_selaras_citra.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "selectel", path: "data/manually-submitted/nochan/20260714_network_blackholes/_selectel.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "semrush", path: "data/manually-submitted/nochan/20260714_network_blackholes/_semrush.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "servetheworld", path: "data/manually-submitted/nochan/20260714_network_blackholes/_servetheworld.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "shadowservers", path: "data/manually-submitted/nochan/20260714_network_blackholes/_shadowservers.netset", provider: "nochan", category: vpnip.CategoryCrawler},
	{name: "sharktek", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sharktek.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "sia_nano", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sia_nano.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "silverstar", path: "data/manually-submitted/nochan/20260714_network_blackholes/_silverstar.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "sino", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sino.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "skoali", path: "data/manually-submitted/nochan/20260714_network_blackholes/_skoali.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "spacenet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_spacenet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "spamhaus_drop_lasso", path: "data/manually-submitted/nochan/20260714_network_blackholes/_spamhaus_drop_lasso.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "sprius", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sprius.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "stormwall", path: "data/manually-submitted/nochan/20260714_network_blackholes/_stormwall.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "strong_tech_vpn", path: "data/manually-submitted/nochan/20260714_network_blackholes/_strong_tech_vpn.netset", provider: "nochan", category: vpnip.CategoryVPN},
	{name: "sun_network_africa", path: "data/manually-submitted/nochan/20260714_network_blackholes/_sun_network_africa.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "surfnet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_surfnet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "syn_ltd", path: "data/manually-submitted/nochan/20260714_network_blackholes/_syn_ltd.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "techoff_srv_ltd", path: "data/manually-submitted/nochan/20260714_network_blackholes/_techoff_srv_ltd.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "techtel_lmds", path: "data/manually-submitted/nochan/20260714_network_blackholes/_techtel_lmds.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "techties", path: "data/manually-submitted/nochan/20260714_network_blackholes/_techties.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "telecable_economico", path: "data/manually-submitted/nochan/20260714_network_blackholes/_telecable_economico.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "telefonica_espana", path: "data/manually-submitted/nochan/20260714_network_blackholes/_telefonica_espana.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "telegram_messenger_eu", path: "data/manually-submitted/nochan/20260714_network_blackholes/_telegram_messenger_eu.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "telenet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_telenet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "tencent", path: "data/manually-submitted/nochan/20260714_network_blackholes/_tencent.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "terrahost", path: "data/manually-submitted/nochan/20260714_network_blackholes/_terrahost.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "timeweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_timeweb.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "topnet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_topnet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "totalplay_mx", path: "data/manually-submitted/nochan/20260714_network_blackholes/_totalplay_mx.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "trackforce", path: "data/manually-submitted/nochan/20260714_network_blackholes/_trackforce.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "trafficforce", path: "data/manually-submitted/nochan/20260714_network_blackholes/_trafficforce.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "ttnet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ttnet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "tube_hosting", path: "data/manually-submitted/nochan/20260714_network_blackholes/_tube_hosting.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "tzulo_inc", path: "data/manually-submitted/nochan/20260714_network_blackholes/_tzulo_inc.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "tzulo_inc_ipv6", path: "data/manually-submitted/nochan/20260714_network_blackholes/_tzulo_inc_ipv6.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "uab_code200", path: "data/manually-submitted/nochan/20260714_network_blackholes/_uab_code200.netset", provider: "nochan", category: vpnip.CategoryProxy},
	{name: "ucloud", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ucloud.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "ultahost", path: "data/manually-submitted/nochan/20260714_network_blackholes/_ultahost.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "unified_layer", path: "data/manually-submitted/nochan/20260714_network_blackholes/_unified_layer.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "utma", path: "data/manually-submitted/nochan/20260714_network_blackholes/_utma.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "valence_tech", path: "data/manually-submitted/nochan/20260714_network_blackholes/_valence_tech.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "veganet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_veganet.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "velia", path: "data/manually-submitted/nochan/20260714_network_blackholes/_velia.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "verizon_business", path: "data/manually-submitted/nochan/20260714_network_blackholes/_verizon_business.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "versatel", path: "data/manually-submitted/nochan/20260714_network_blackholes/_versatel.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "versaweb", path: "data/manually-submitted/nochan/20260714_network_blackholes/_versaweb.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "vietserver", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vietserver.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "viettel_group", path: "data/manually-submitted/nochan/20260714_network_blackholes/_viettel_group.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "virtual_systems", path: "data/manually-submitted/nochan/20260714_network_blackholes/_virtual_systems.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "visual_trading", path: "data/manually-submitted/nochan/20260714_network_blackholes/_visual_trading.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "vivid_hosting", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vivid_hosting.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "vnpt_corp", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vnpt_corp.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "vpn", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vpn.netset", provider: "nochan", category: vpnip.CategoryVPN},
	{name: "vpsvault", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vpsvault.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "vultr", path: "data/manually-submitted/nochan/20260714_network_blackholes/_vultr.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "web2objects", path: "data/manually-submitted/nochan/20260714_network_blackholes/_web2objects.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "web_lacerda", path: "data/manually-submitted/nochan/20260714_network_blackholes/_web_lacerda.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "webhero", path: "data/manually-submitted/nochan/20260714_network_blackholes/_webhero.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "webnx_inc", path: "data/manually-submitted/nochan/20260714_network_blackholes/_webnx_inc.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "webwerks", path: "data/manually-submitted/nochan/20260714_network_blackholes/_webwerks.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "wholesale_internet", path: "data/manually-submitted/nochan/20260714_network_blackholes/_wholesale_internet.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "world4you", path: "data/manually-submitted/nochan/20260714_network_blackholes/_world4you.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "xneelo", path: "data/manually-submitted/nochan/20260714_network_blackholes/_xneelo.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "xtra_telecom", path: "data/manually-submitted/nochan/20260714_network_blackholes/_xtra_telecom.netset", provider: "nochan", category: vpnip.CategoryAbuse},
	{name: "zenlayer", path: "data/manually-submitted/nochan/20260714_network_blackholes/_zenlayer.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
	{name: "zscaler", path: "data/manually-submitted/nochan/20260714_network_blackholes/_zscaler.netset", provider: "nochan", category: vpnip.CategoryDatacenter},
}

// asnSource describes a single autonomous system whose announced prefixes are
// imported from RIPEstat (see [ripeasn.Fetch]).
type asnSource struct {
	// asn is the AS number (without the "AS" prefix) queried from RIPEstat,
	// e.g. 136907 for AS136907.
	asn uint
	// provider is the provider name stored on each record, e.g. "huawei-cloud".
	provider string
	// categories are the Category* constants applied to every prefix the AS
	// announces. Each is folded as its own membership, so an AS tagged both
	// datacenter and abuse surfaces is_datacenter and is_abuse on its records.
	categories []string
}

// asnSources is the set of autonomous systems imported into the database via
// RIPEstat's announced-prefixes API.
var asnSources = []asnSource{
	{asn: 136907, provider: "huawei-cloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 45102, provider: "alibaba-cloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 21859, provider: "zenlayer", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 214738, provider: "wehost", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 22616, provider: "zscaler", categories: []string{vpnip.CategoryDatacenter}},

	// Abuse-prone VPS/hosting networks: heavy sources of VPN exit nodes,
	// residential-proxy backbones, and scraping/abuse traffic.
	{asn: 9009, provider: "m247", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 60068, provider: "datacamp", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 51167, provider: "contabo", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 60781, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 19148, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 36352, provider: "colocrossing", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 54290, provider: "hostwinds", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 53667, provider: "frantech", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 40676, provider: "psychz", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 49505, provider: "selectel", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Mainstream cloud/CDN and VPS providers: tagged datacenter only.
	{asn: 199524, provider: "gcore", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 54825, provider: "equinix-metal", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 8560, provider: "ionos", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 31034, provider: "aruba", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 197540, provider: "netcup", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 202053, provider: "upcloud", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 20940, provider: "akamai", categories: []string{vpnip.CategoryDatacenter}},

	// Bulletproof / abuse-tolerant hosting and proxy/VPN egress networks.
	{asn: 198953, provider: "proton66", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 210644, provider: "aeza", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 216071, provider: "vdsina", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 51396, provider: "pfcloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 200651, provider: "flokinet", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 49453, provider: "global-layer", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 206264, provider: "amarutu", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 39572, provider: "advancedhosters", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 209588, provider: "flyservers", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 207137, provider: "packethub", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 51852, provider: "private-layer", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 8100, provider: "quadranet", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 25820, provider: "it7", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 53850, provider: "gorillaservers", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 207990, provider: "hostroyale", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 215730, provider: "h2nexus", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 401116, provider: "nybula", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 132839, provider: "powerline", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 213035, provider: "serverion", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 59711, provider: "hz-hosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Mainstream / regional VPS and hosting providers: datacenter only.
	{asn: 55990, provider: "huawei-cloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 9370, provider: "sakura", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 7506, provider: "gmo", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 62240, provider: "clouvider", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 49981, provider: "worldstream", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 50673, provider: "serverius", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 60404, provider: "liteserver", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 29802, provider: "hivelocity", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 46562, provider: "performive", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 35916, provider: "multacom", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 22612, provider: "namecheap", categories: []string{vpnip.CategoryDatacenter}},

	// More bulletproof / abuse-tolerant hosting (many CIS/EU-registered).
	{asn: 9123, provider: "timeweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 200019, provider: "alexhost", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 41722, provider: "miran", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 60117, provider: "hostsailor", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 200373, provider: "3xk-tech", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 213251, provider: "metaliance", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 401120, provider: "cheapy-host", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 215311, provider: "regxa", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 64236, provider: "unrealservers", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 206092, provider: "secfirewall", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 204957, provider: "greenfloid", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 215925, provider: "vpsvault", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 209605, provider: "hostbaltic", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Additional Leaseweb US locations (WDC, LAX, SFO) beyond AS60781/AS19148.
	{asn: 30633, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 395954, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 7203, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// More mainstream / regional VPS and hosting providers: datacenter only.
	{asn: 47583, provider: "hostinger", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 56630, provider: "melbicom", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 25369, provider: "hydra", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 24961, provider: "myloc", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 51765, provider: "creanova", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 211252, provider: "akari", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 200350, provider: "yandex-cloud", categories: []string{vpnip.CategoryDatacenter}},

	// Additional bulletproof / abuse-tolerant networks (curated, known operators).
	{asn: 206237, provider: "ipv4fun", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211483, provider: "netverse", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 206419, provider: "hostco", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211572, provider: "ukhost4u", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 59725, provider: "silvercloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 201632, provider: "upcloud-se", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 207844, provider: "hostcc", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211652, provider: "hosting4all", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 208055, provider: "vdserver", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 215357, provider: "empower", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 206704, provider: "liramail", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 45090, provider: "tencent-apac", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 132203, provider: "tencent-cn", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 37963, provider: "alibaba-alt", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 206268, provider: "iphosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212053, provider: "hosting-services", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211739, provider: "cloudserv", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 205269, provider: "webix", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212360, provider: "premium-server", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 209642, provider: "onlineservers", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212365, provider: "fast-hosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211997, provider: "serverhub", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 210931, provider: "nextsrv", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212011, provider: "cloudnet-eu", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212106, provider: "hosthub", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212148, provider: "netblock", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Brazilian hosting (abuse-prone).
	{asn: 64762, provider: "teserv-br", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 133069, provider: "ttnet-br", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 28261, provider: "nova-br", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Additional mainstream / regional VPS providers: datacenter only.
	{asn: 205395, provider: "ipxo-cloud", categories: []string{vpnip.CategoryDatacenter}},

	// Hosting/VPS networks surfaced by Cloudflare Radar's top bot-traffic ASNs
	// (docs/cf-bot-asn.md). Residential ISPs, mobile carriers, telco backbones,
	// transit, and ad-tech from that list are deliberately excluded; providers
	// already covered by an official feed or existing entry are skipped per the
	// official > AS > IP-list priority.
	{asn: 7979, provider: "servers-com", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 48090, provider: "techoff", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 212238, provider: "datacamp", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 46636, provider: "natcoweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 211590, provider: "bucklog", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 203020, provider: "hostroyale", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 398465, provider: "rackdog", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 55286, provider: "b2net", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 55081, provider: "24shells", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 398781, provider: "oculus", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 137409, provider: "gsl", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 202448, provider: "mvps", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 396362, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 59253, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 28753, provider: "leaseweb", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 63023, provider: "gthost", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 136787, provider: "packethub", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 147049, provider: "packethub", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 29066, provider: "velia", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 40021, provider: "contabo", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 141995, provider: "contabo", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 23033, provider: "wowrack", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 50245, provider: "serverel", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 62874, provider: "web2objects", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 56740, provider: "datahata", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 30058, provider: "fdcservers", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 142430, provider: "digivps", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 210855, provider: "meetscale", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 208677, provider: "cloud-ru", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 203919, provider: "lumadock", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 23470, provider: "reliablesite", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 150303, provider: "solordp", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 205016, provider: "hernlabs", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 57043, provider: "hostkey", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 207728, provider: "eurohoster", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 62563, provider: "globaltelehost", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 47447, provider: "23m", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 140947, provider: "snthostings", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 62651, provider: "strongtech", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 6681, provider: "givemecloud", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 38136, provider: "akari-networks", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Established colos / cloud / bare-metal from the same list: datacenter only.
	{asn: 27357, provider: "rackspace", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 60558, provider: "phoenixnap", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 44066, provider: "firstcolo", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 396356, provider: "latitude", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 262287, provider: "latitude", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 19318, provider: "interserver", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 400940, provider: "railway", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 20326, provider: "teraswitch", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 63199, provider: "cds-global", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 150436, provider: "byteplus", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 135377, provider: "ucloud", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 19437, provider: "securedservers", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 20454, provider: "securedservers", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 214996, provider: "netcup", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 46475, provider: "limestone", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 27257, provider: "webair", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 14537, provider: "continent8", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 18779, provider: "egihosting", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 53334, provider: "totaluptime", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 11798, provider: "acedata", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 906, provider: "dmit", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 11878, provider: "tzulo", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 62610, provider: "zenlayer", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 4229, provider: "zenlayer", categories: []string{vpnip.CategoryDatacenter}},

	// Commercial proxy provider infrastructure.
	{asn: 396319, provider: "oxylabs", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryProxy}},

	// Stark Industries / PQ Hosting / Neculiti bulletproof cluster (sanctioned).
	// Several originals (stark-pq, pq-hosting, weiss) currently announce no
	// routes post-sanction; kept so coverage resumes automatically if they do.
	{asn: 44477, provider: "stark-pq", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 209847, provider: "worktitans", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 213999, provider: "worktitans", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 33993, provider: "ufo-hosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 43624, provider: "pq-hosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 44774, provider: "weiss", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 48031, provider: "xserver-ua", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Other notorious bulletproof / abuse-tolerant operators and enablers.
	{asn: 206728, provider: "medialand", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 30823, provider: "aurologic", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 202425, provider: "ip-volume", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 52000, provider: "mirhosting", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 214943, provider: "railnet", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 213702, provider: "qwins", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},
	{asn: 51381, provider: "1337team", categories: []string{vpnip.CategoryDatacenter, vpnip.CategoryAbuse}},

	// Mainstream datacenter / hosting (non-abuse egress): datacenter only.
	{asn: 54641, provider: "inmotion", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 3842, provider: "ramnode", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 32244, provider: "liquidweb", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 26347, provider: "dreamhost", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 26496, provider: "godaddy", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 46606, provider: "newfold", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 63473, provider: "hosthatch", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 62282, provider: "delska", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 36007, provider: "kamatera", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 64022, provider: "kamatera", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 40401, provider: "backblaze", categories: []string{vpnip.CategoryDatacenter}},
	{asn: 396865, provider: "backblaze", categories: []string{vpnip.CategoryDatacenter}},
}

// deriveProvider turns a list file path into a provider name by stripping the
// .txt extension and known suffixes used across the upstream repos:
//
//	data/input/ip/nordvpn_api.txt -> nordvpn
//	nordvpn-ips.txt               -> nordvpn
//	tunnelbear_ips.txt            -> tunnelbear
//	output/datacentres.txt        -> datacentres
func deriveProvider(p string) string {
	name := strings.TrimSuffix(path.Base(p), ".txt")
	for _, suffix := range []string{"_api", "-ips", "_ips"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return name
}

// parseIPLines parses the contents of a list file into prefixes. It accepts both
// bare IP addresses (treated as /32 or /128) and CIDR notation. Lines may carry
// trailing "# comments" and whole-line comments; blank and unparseable lines are
// skipped.
func parseIPLines(data []byte) []netip.Prefix {
	var out []netip.Prefix
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if p, ok := parsePrefixField(line); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseCSVFirstField parses CSV blocklists whose first column is a bare IP
// address or CIDR range, ignoring any remaining columns, e.g.:
//
//	171.25.193.25/32,"TOR Exit node"
//
// Blank, "#"-commented, and unparseable lines are skipped.
func parseCSVFirstField(data []byte) []netip.Prefix {
	var out []netip.Prefix
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		field, _, _ := strings.Cut(line, ",")
		if p, ok := parsePrefixField(strings.TrimSpace(field)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseCSVSecondField parses CSV lists whose second column (index 1) is a bare
// IP address or CIDR range, ignoring every other column, e.g. the
// alireza-rezaee/tor-nodes CSV format:
//
//	nickname,171.25.193.25,or_port,dir_port
//
// The header row and any blank or unparseable lines are skipped (the header's
// second field never parses as a valid prefix).
func parseCSVSecondField(data []byte) []netip.Prefix {
	var out []netip.Prefix
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, rest, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		field, _, _ := strings.Cut(rest, ",")
		if p, ok := parsePrefixField(strings.TrimSpace(field)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseColonHost parses proxy lists whose lines begin with an IP address
// followed by a colon-separated port and optional trailing fields, e.g.:
//
//	190.97.238.85:999
//	200.34.227.28:8080:Brazil
//
// Only the leading IP is kept, as a host prefix. Blank, "#"-commented, and
// unparseable lines are skipped. IPv4 only; bracketed IPv6 hosts are not parsed.
func parseColonHost(data []byte) []netip.Prefix {
	var out []netip.Prefix
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, _, _ := strings.Cut(line, ":")
		if p, ok := parsePrefixField(strings.TrimSpace(host)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parsePrefixField turns a single bare-IP or CIDR token into a masked prefix.
// Bare addresses become host prefixes (/32 or /128). It reports false for empty
// or unparseable tokens.
func parsePrefixField(field string) (netip.Prefix, bool) {
	if field == "" {
		return netip.Prefix{}, false
	}

	if strings.ContainsRune(field, '/') {
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return netip.Prefix{}, false
		}
		return prefix.Masked(), true
	}

	addr, err := netip.ParseAddr(field)
	if err != nil {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), true
}

// parseProxyURLs parses lists whose lines are proxy URLs of the form
// "scheme://host:port" (e.g. "socks5://208.102.51.6:58208"), extracting the host
// as a prefix. Lines may carry trailing "# comments" and whole-line comments;
// blank and unparseable lines are skipped.
func parseProxyURLs(data []byte) []netip.Prefix {
	var out []netip.Prefix
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out
}

// parseGoogleStyleJSON parses IP range files in the Google/AWS-style schema used
// by several cloud providers (and the tiktok-ips-list repo):
//
//	{"prefixes": [{"ipv4Prefix": "1.2.3.0/24"}, {"ipv6Prefix": "2001:db8::/32"}]}
//
// Both ipv4Prefix and ipv6Prefix keys are accepted; blank and unparseable
// entries are skipped. CIDR ranges are masked to their network address.
func parseGoogleStyleJSON(data []byte) []netip.Prefix {
	var doc struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
			IPv6Prefix string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []netip.Prefix
	for _, p := range doc.Prefixes {
		raw := p.IPv4Prefix
		if raw == "" {
			raw = p.IPv6Prefix
		}
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		out = append(out, prefix.Masked())
	}
	return out
}

// parseAWSJSON parses the AWS IP range schema served at
// https://ip-ranges.amazonaws.com/ip-ranges.json, which splits IPv4 and IPv6
// ranges into two arrays keyed differently from the Google-style schema:
//
//	{
//	  "prefixes":      [{"ip_prefix":   "3.5.140.0/22", "service": "AMAZON"}],
//	  "ipv6_prefixes": [{"ipv6_prefix": "2600:1f01::/47", "service": "AMAZON"}]
//	}
//
// Both arrays are read; blank and unparseable entries are skipped and CIDR
// ranges are masked to their network address.
func parseAWSJSON(data []byte) []netip.Prefix {
	var doc struct {
		Prefixes []struct {
			IPPrefix string `json:"ip_prefix"`
		} `json:"prefixes"`
		IPv6Prefixes []struct {
			IPv6Prefix string `json:"ipv6_prefix"`
		} `json:"ipv6_prefixes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	out := make([]netip.Prefix, 0, len(doc.Prefixes)+len(doc.IPv6Prefixes))
	for _, p := range doc.Prefixes {
		if prefix, err := netip.ParsePrefix(p.IPPrefix); err == nil {
			out = append(out, prefix.Masked())
		}
	}
	for _, p := range doc.IPv6Prefixes {
		if prefix, err := netip.ParsePrefix(p.IPv6Prefix); err == nil {
			out = append(out, prefix.Masked())
		}
	}
	return out
}

// parseOracleJSON parses Oracle Cloud's public IP range export served at
// https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json, which nests
// CIDRs under a per-region cidr list:
//
//	{"regions": [{"region": "us-ashburn-1", "cidrs": [{"cidr": "1.2.3.0/24"}]}]}
//
// Every region's CIDRs are read; unparseable entries are skipped and ranges are
// masked to their network address.
func parseOracleJSON(data []byte) []netip.Prefix {
	var doc struct {
		Regions []struct {
			CIDRs []struct {
				CIDR string `json:"cidr"`
			} `json:"cidrs"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []netip.Prefix
	for _, r := range doc.Regions {
		for _, c := range r.CIDRs {
			if p, ok := parsePrefixField(c.CIDR); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// parseAzureJSON parses the Microsoft Azure IP range export (as republished by
// femueller/cloud-ip-ranges), which groups address prefixes under each service
// tag's properties:
//
//	{"values": [{"properties": {"addressPrefixes": ["1.2.3.0/24", "2001:db8::/32"]}}]}
//
// Every service tag's prefixes are read; unparseable entries are skipped and
// ranges are masked to their network address.
func parseAzureJSON(data []byte) []netip.Prefix {
	var doc struct {
		Values []struct {
			Properties struct {
				AddressPrefixes []string `json:"addressPrefixes"`
			} `json:"properties"`
		} `json:"values"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []netip.Prefix
	for _, v := range doc.Values {
		for _, raw := range v.Properties.AddressPrefixes {
			if p, ok := parsePrefixField(raw); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// parseGitHubActions parses GitHub's IP range export as republished by the
// rezmoss/cloud-provider-ip-addresses repo, a flat array tagging each prefix
// with the GitHub service it belongs to:
//
//	[{"ip_address": "4.148.0.0/16", "ip_type": "IPv4", "service": "actions"}]
//
// Only entries whose service is "actions" are kept (the GitHub Actions runner
// ranges); other services and unparseable entries are skipped.
func parseGitHubActions(data []byte) []netip.Prefix {
	var doc []struct {
		IPAddress string `json:"ip_address"`
		Service   string `json:"service"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []netip.Prefix
	for _, e := range doc {
		if e.Service != "actions" {
			continue
		}
		if p, ok := parsePrefixField(e.IPAddress); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseGitHubMetaKey returns a parser for GitHub's /meta API response, which
// flatly maps each GitHub product to its own array of CIDR strings:
//
//	{"copilot": ["20.100.224.0/20", "2a01:111:f403::/48"], "actions": [...]}
//
// The returned parser reads only the named key; unparseable entries are
// skipped.
func parseGitHubMetaKey(key string) func([]byte) []netip.Prefix {
	return func(data []byte) []netip.Prefix {
		var doc map[string][]string
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil
		}

		var out []netip.Prefix
		for _, raw := range doc[key] {
			if p, ok := parsePrefixField(raw); ok {
				out = append(out, p)
			}
		}
		return out
	}
}

// parseIPAddressJSON parses the rezmoss/cloud-provider-ip-addresses flat-array
// JSON schema, keeping only the ip_address of each entry and ignoring every
// other field:
//
//	[{"ip_address": "23.235.32.0/20", "ip_type": "IPv4"}]
//
// Unparseable entries are skipped. Use this for providers whose region/service
// tags carry no useful distinction (e.g. a single "global" region).
func parseIPAddressJSON(data []byte) []netip.Prefix {
	var doc []struct {
		IPAddress string `json:"ip_address"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []netip.Prefix
	for _, e := range doc {
		if p, ok := parsePrefixField(e.IPAddress); ok {
			out = append(out, p)
		}
	}
	return out
}

// regionGroupedParser returns a parser for the rezmoss/cloud-provider-ip-addresses
// flat-array JSON schema that buckets each prefix under a "<provider>-<region>"
// provider so the resulting records record which region a range belongs to:
//
//	[{"ip_address": "8.34.208.0/23", "ip_type": "IPv4", "region": "europe-west1"}]
//
// Entries with no region fall under "<provider>-global"; unparseable entries
// are skipped.
func regionGroupedParser(provider string) func([]byte) map[string][]netip.Prefix {
	return func(data []byte) map[string][]netip.Prefix {
		var doc []struct {
			IPAddress string `json:"ip_address"`
			Region    string `json:"region"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil
		}

		out := map[string][]netip.Prefix{}
		for _, e := range doc {
			p, ok := parsePrefixField(e.IPAddress)
			if !ok {
				continue
			}
			region := e.Region
			if region == "" {
				region = "global"
			}
			key := provider + "-" + region
			out[key] = append(out[key], p)
		}
		return out
	}
}

// matchFiles returns the files in fsys matching pattern. Plain patterns are
// handled by [fs.Glob]; patterns containing "**" match the trailing filename
// glob against files at any depth below the directory prefix that precedes the
// "**", e.g. "**/*.txt" matches every .txt file in the tree and
// "Countries/**/*.txt" only those under Countries/.
func matchFiles(fsys fs.FS, pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		return fs.Glob(fsys, pattern)
	}

	root := "."
	if i := strings.Index(pattern, "**"); i > 0 {
		if root = strings.TrimSuffix(pattern[:i], "/"); root == "" {
			root = "."
		}
	}
	base := path.Base(pattern)

	var files []string
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ok, err := path.Match(base, path.Base(p))
		if err != nil {
			return err
		}
		if ok {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// fetchedMarker is the empty sentinel file written at the root of a repo's
// cache directory once its list files have been cached. Its modification time
// records when the repo was last fetched, and its name (a dotfile without a
// list extension) is never matched by any source glob.
const fetchedMarker = ".fetched"

// repoFS returns a filesystem holding src's list files. When cacheDir is
// non-empty and a copy cached within [cacheMaxAge] exists, it is served from
// disk without touching the network; otherwise the repo is cloned with gitfs,
// its matched list files are cached for next time, and the clone is used. An
// empty cacheDir disables caching entirely.
func repoFS(lg *slog.Logger, src repoSource, ref, cacheDir string) (fs.FS, error) {
	var repoDir string
	if cacheDir != "" {
		repoDir = filepath.Join(cacheDir, "sources", filepath.FromSlash(src.name))
		if cachedRepoFresh(repoDir) {
			lg.Info("using cached repo", "repo", src.name, "path", repoDir)
			return os.DirFS(repoDir), nil
		}
	}

	lg.Info("cloning", "repo", src.url, "ref", ref)

	repo, err := gitfs.NewRepo(src.url)
	if err != nil {
		return nil, fmt.Errorf("opening repo %s: %w", src.url, err)
	}

	_, fsys, err := repo.Clone(ref)
	if err != nil {
		return nil, fmt.Errorf("cloning repo %s: %w", src.url, err)
	}

	if repoDir != "" {
		if err := cacheRepo(src, fsys, repoDir); err != nil {
			lg.Warn("caching repo failed", "repo", src.name, "path", repoDir, "err", err)
		} else {
			// Serve from the freshly written cache so the cached and clone code
			// paths read through the same on-disk layout.
			return os.DirFS(repoDir), nil
		}
	}

	return fsys, nil
}

// cachedRepoFresh reports whether repoDir holds a [fetchedMarker] younger than
// [cacheMaxAge], i.e. a cached repo copy that may be served without refetching.
func cachedRepoFresh(repoDir string) bool {
	info, err := os.Stat(filepath.Join(repoDir, fetchedMarker))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < cacheMaxAge
}

// cacheRepo writes the list files of src matched in fsys to repoDir, preserving
// their repository-relative paths so the cached tree globs identically to the
// clone. The directory is rebuilt from scratch each time so files removed
// upstream (or left over from a different ref) do not linger, and a
// [fetchedMarker] is written last to stamp the fetch time.
//
// It deliberately caches every list in src.lists, never the categories the
// current build selected: the cache outlives any one build and is shared by all
// of them, so whichever build populates it first must leave it complete for the
// rest. Narrowing this to the selected lists — the way [collect] is narrowed —
// would let a datacenter-only build on a cold cache write a datacenter-only
// tree; a later full build would find that tree fresh, skip the clone, and
// silently ship a database missing every VPN, abuse, crawler, proxy, and tor
// list this repository carries. Filter at fold time, not at cache time.
func cacheRepo(src repoSource, fsys fs.FS, repoDir string) error {
	if err := os.RemoveAll(repoDir); err != nil {
		return err
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}

	for _, ls := range src.lists {
		files, err := matchFiles(fsys, ls.glob)
		if err != nil {
			return err
		}

		for _, file := range files {
			data, err := fs.ReadFile(fsys, file)
			if err != nil {
				return err
			}

			dst := filepath.Join(repoDir, filepath.FromSlash(file))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return err
			}
		}
	}

	return os.WriteFile(filepath.Join(repoDir, fetchedMarker), nil, 0o644)
}

// collect reads every file matching lists from the repository's file tree and
// folds the parsed prefixes into the store. lists is passed separately from src
// rather than read off src.lists so callers can narrow it to the selected
// categories; see selectLists.
func collect(src repoSource, lists []listSpec, fsys fs.FS, store *bart.Table[*vpnip.Record]) (int, error) {
	count := 0
	for _, ls := range lists {
		files, err := matchFiles(fsys, ls.glob)
		if err != nil {
			return count, err
		}

		for _, file := range files {
			data, err := fs.ReadFile(fsys, file)
			if err != nil {
				return count, err
			}

			provider := ls.provider
			if provider == "" {
				provider = deriveProvider(file)
			}

			parse := ls.parse
			if parse == nil {
				parse = parseIPLines
			}

			count += fold(store, parse(data), vpnip.ListMembership{
				Repository: src.name,
				List:       file,
				Provider:   provider,
				Category:   ls.category,
			})
		}
	}
	return count, nil
}

// collectHTTP downloads src over HTTP (or reads a fresh on-disk copy from
// cacheDir) and folds every parsed prefix into the store, keyed by prefix.
// Existing records gain additional [vpnip.ListMembership] entries.
func collectHTTP(ctx context.Context, client *http.Client, src httpSource, cacheDir string, store *bart.Table[*vpnip.Record]) (int, error) {
	data, err := fetchHTTP(ctx, client, src, cacheDir)
	if err != nil {
		return 0, err
	}

	list := path.Base(src.url)

	// A grouped parser assigns each prefix its own provider (e.g. a cloud
	// region), so fold one membership per group rather than a single one for
	// the whole source.
	if src.parseGrouped != nil {
		count := 0
		for provider, prefixes := range src.parseGrouped(data) {
			count += fold(store, prefixes, vpnip.ListMembership{
				Repository: src.name,
				List:       list,
				Provider:   provider,
				Category:   src.category,
			})
		}
		return count, nil
	}

	parse := src.parse
	if parse == nil {
		parse = parseIPLines
	}

	return fold(store, parse(data), vpnip.ListMembership{
		Repository: src.name,
		List:       list,
		Provider:   src.provider,
		Category:   src.category,
	}), nil
}

// lfsPointerPrefix begins every git-lfs pointer file, per the LFS spec.
var lfsPointerPrefix = []byte("version https://git-lfs.github.com/spec/v1")

// isLFSPointer reports whether data is a git-lfs pointer rather than the file it
// stands for.
//
// The lists under data/manually-submitted are stored in LFS, and a clone made
// without git-lfs installed leaves the pointers unsmudged. Nothing in a pointer
// parses as an IP address, so without this check the affected source folds in
// zero prefixes and reports no error at all -- silently building a database
// missing every manually-submitted list.
func isLFSPointer(data []byte) bool {
	return bytes.HasPrefix(data, lfsPointerPrefix)
}

// collectFile reads the local list file for src and folds each parsed prefix into
// the store under src's repository, provider, and category. It is the
// filesystem analogue of [collectHTTP]: same parsing and folding, but the bytes
// come from disk instead of the network.
func collectFile(src fileSource, store *bart.Table[*vpnip.Record]) (int, error) {
	data, err := os.ReadFile(src.path)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", src.path, err)
	}

	if isLFSPointer(data) {
		return 0, fmt.Errorf("%s is an unsmudged git-lfs pointer, not a list: run `git lfs pull`", src.path)
	}

	parse := src.parse
	if parse == nil {
		parse = parseIPLines
	}

	return fold(store, parse(data), vpnip.ListMembership{
		Repository: src.name,
		List:       path.Base(src.path),
		Provider:   src.provider,
		Category:   src.category,
	}), nil
}

// collectAS fetches the prefixes announced by src's autonomous system from
// RIPEstat (or a fresh on-disk copy from cacheDir) and folds each into the store
// under every category in categories. Existing records gain additional
// [vpnip.ListMembership] entries.
func collectAS(ctx context.Context, client *http.Client, src asnSource, categories []string, cacheDir string, store *bart.Table[*vpnip.Record]) (int, error) {
	prefixes, err := fetchAS(ctx, client, src, cacheDir)
	if err != nil {
		return 0, err
	}
	return foldAS(store, src, categories, prefixes), nil
}

// fetchAS returns the prefixes announced by src's AS. When cacheDir is non-empty
// it caches the prefix list at cacheDir/sources/stat.ripe.net/AS<n>.json, serving
// a copy younger than [cacheMaxAge] without touching the network and otherwise
// fetching from RIPEstat with [ripeasn.Fetch] and refreshing the cache. An empty
// cacheDir disables caching entirely. The cache holds the parsed prefix list (not
// RIPEstat's raw response), keeping the cache format independent of the ripeasn
// package.
func fetchAS(ctx context.Context, client *http.Client, src asnSource, cacheDir string) ([]netip.Prefix, error) {
	var cachePath string
	if cacheDir != "" {
		cachePath = filepath.Join(cacheDir, "sources", "stat.ripe.net", fmt.Sprintf("AS%d.json", src.asn))
		if data, ok := readFreshCache(cachePath); ok {
			if prefixes, err := decodePrefixes(data); err == nil {
				slog.Info("using cached AS prefixes", "asn", src.asn, "path", cachePath)
				return prefixes, nil
			} else {
				slog.Warn("ignoring corrupt AS cache", "asn", src.asn, "path", cachePath, "err", err)
			}
		}
	}

	slog.Info("fetching announced prefixes", "asn", src.asn)

	resp, err := ripeasn.Fetch(ctx, client, src.asn)
	if err != nil {
		return nil, err
	}
	prefixes := asnPrefixes(resp)

	if cachePath != "" {
		if data, err := json.Marshal(prefixes); err != nil {
			slog.Warn("encoding AS prefixes failed", "asn", src.asn, "err", err)
		} else if err := writeCache(cachePath, data); err != nil {
			slog.Warn("caching AS prefixes failed", "asn", src.asn, "path", cachePath, "err", err)
		}
	}

	return prefixes, nil
}

// decodePrefixes parses the JSON prefix-list cache written by [fetchAS] back into
// a slice of prefixes.
func decodePrefixes(data []byte) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	if err := json.Unmarshal(data, &prefixes); err != nil {
		return nil, err
	}
	return prefixes, nil
}

// asnPrefixes extracts the announced prefixes from a RIPEstat response, masking
// each to its network address and dropping any invalid entries.
func asnPrefixes(resp *ripeasn.Response) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(resp.Data.Prefixes))
	for _, p := range resp.Data.Prefixes {
		if p.Prefix.IsValid() {
			out = append(out, p.Prefix.Masked())
		}
	}
	return out
}

// foldAS folds every prefix the AS announces into the store, once per category.
// categories is passed separately from src rather than read off src.categories
// so callers can narrow it to the selected ones: an AS tagged both datacenter
// and abuse contributes only its datacenter membership to a datacentre-only
// build. See categorySet.intersect.
func foldAS(store *bart.Table[*vpnip.Record], src asnSource, categories []string, prefixes []netip.Prefix) int {
	list := fmt.Sprintf("AS%d", src.asn)
	count := 0
	for _, category := range categories {
		count += fold(store, prefixes, vpnip.ListMembership{
			Repository: "stat.ripe.net",
			List:       list,
			Provider:   src.provider,
			Category:   category,
		})
	}
	return count
}

// fetchHTTP returns the body of src. When cacheDir is non-empty it caches the
// list at cacheDir/sources/<name>/<file>, serving a cached copy younger than
// [cacheMaxAge] without touching the network and otherwise downloading the list
// and refreshing the cache. An empty cacheDir disables caching entirely.
func fetchHTTP(ctx context.Context, client *http.Client, src httpSource, cacheDir string) ([]byte, error) {
	var cachePath string
	if cacheDir != "" {
		cachePath = filepath.Join(cacheDir, "sources", filepath.FromSlash(src.name), httpListName(src.url))
		if data, ok := readFreshCache(cachePath); ok {
			slog.Info("using cached list", "source", src.name, "path", cachePath)
			return data, nil
		}
	}

	slog.Info("downloading", "url", src.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if cachePath != "" {
		if err := writeCache(cachePath, data); err != nil {
			slog.Warn("caching list failed", "source", src.name, "path", cachePath, "err", err)
		}
	}

	return data, nil
}

// httpListName returns the cache filename for an HTTP source URL: its last path
// segment, falling back to the host when the path has no usable segment (e.g. a
// bare "?query" URL like the constant.com geofeed). The result is always a
// single path component with no separators.
func httpListName(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return base
		}
		if u.Host != "" {
			return u.Host
		}
	}
	return path.Base(rawURL)
}

// readFreshCache returns the contents of path when it exists and was modified
// within [cacheMaxAge]. It reports false for a missing, stale, or unreadable
// cache file so the caller falls back to fetching over HTTP.
func readFreshCache(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) >= cacheMaxAge {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// writeCache writes data to path atomically, creating parent directories as
// needed. The write goes to a temporary file in the same directory and is then
// renamed into place so a crash mid-write never leaves a truncated cache entry.
func writeCache(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// mergeContained augments every record with the list memberships of any broader
// prefix in the set that contains it, so a narrow entry (e.g. a /32 on one
// blocklist) inherits the categories and providers of a wider CIDR (e.g. a /24
// on another) that covers it. Inheritance flows from supernet to subnet only: a
// containing prefix does not gain the data of its narrower members.
//
// The store is already a [bart.Table] radix trie, so each record's covering
// supernets are found with a single trie descent rather than one map lookup per
// possible prefix length. Each record enumerates all of its covering prefixes
// directly, so the result is independent of iteration order; [vpnip.Record.Add]
// dedups any overlap. Only the stored *Record values are mutated, never the trie
// structure, so iterating the table while reading supernets from it is safe.
func mergeContained(store *bart.Table[*vpnip.Record]) {
	for prefix, rec := range store.All() {
		for super, parent := range store.Supernets(prefix) {
			if super == prefix {
				continue // Supernets includes pfx itself; inherit from broader only.
			}
			for _, m := range parent.Sources {
				rec.Add(m)
			}
		}
	}
}

// fold inserts each prefix into the store, attaching membership to new and
// existing records alike, and returns the number of prefixes processed.
func fold(store *bart.Table[*vpnip.Record], prefixes []netip.Prefix, m vpnip.ListMembership) int {
	for _, prefix := range prefixes {
		rec, ok := store.Get(prefix)
		if !ok {
			rec = &vpnip.Record{}
			store.Insert(prefix, rec)
		}
		rec.Add(m)
	}
	return len(prefixes)
}
