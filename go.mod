module github.com/TecharoHQ/reputationdb

go 1.26.4

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.11-20260709200747-435963d16310.1
	connectrpc.com/connect v1.20.0
	github.com/facebookgo/flagenv v0.0.0-20160425205200-fcd59fca7456
	github.com/gaissmai/bart v0.28.0
	github.com/joho/godotenv v1.5.1
	github.com/maxmind/mmdbwriter v1.2.0
	github.com/oschwald/maxminddb-golang/v2 v2.4.1
	google.golang.org/protobuf v1.36.11
	rsc.io/gitfs v1.0.0
)

require (
	charm.land/lipgloss/v2 v2.0.0-beta.3.0.20251120230642-dcccabe2cd63 // indirect
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/Masterminds/semver/v3 v3.4.0 // indirect
	github.com/caarlos0/log v0.5.3 // indirect
	github.com/caarlos0/pinata v0.3.4 // indirect
	github.com/charmbracelet/colorprofile v0.3.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20251120225753-26363bddd922 // indirect
	github.com/charmbracelet/x/ansi v0.11.1 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.5.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.3.0 // indirect
	github.com/facebookgo/ensure v0.0.0-20200202191622-63f1cf65ac4c // indirect
	github.com/facebookgo/stack v0.0.0-20160209184415-751773369052 // indirect
	github.com/facebookgo/subset v0.0.0-20200203212716-c811ad88dec4 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go4.org/netipx v0.0.0-20231129151722-fdeea329fbba // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/tools v0.48.0 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	github.com/caarlos0/pinata
	golang.org/x/tools/cmd/goimports
	golang.org/x/tools/cmd/stringer
	honnef.co/go/tools/cmd/staticcheck
)
