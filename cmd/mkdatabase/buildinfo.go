package main

import (
	"runtime/debug"
	"time"
)

// buildSetting returns the value of the named debug.BuildSetting, or "" when it
// is absent.
func buildSetting(settings []debug.BuildSetting, key string) string {
	for _, s := range settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// buildEpoch returns the timestamp stamped into the database's metadata: the
// commit timestamp when the binary was built from a clean checkout, and now
// otherwise.
//
// Pinning to the commit makes a build a pure function of its inputs, so a clean
// tree plus a warm source cache rebuilds a byte-identical database and therefore
// the same version ID under publish-database's content-hash keying. A dirty tree
// has no revision worth naming, so it reports the wall clock instead.
//
// The tradeoff: CI always checks out clean, so build_epoch tracks the builder's
// revision rather than the freshness of the upstream data, which is refetched
// daily. For the free datacentre database, freshness is conveyed by the GitHub
// release asset's own upload timestamp.
//
// These stamps are recorded when the *binary* is built, not when the database
// is. That is consistent with how this repo runs: npm run build rebuilds the
// binary immediately before running it, so edits to the tree (including the
// lists under data/) reliably surface as vcs.modified=true.
func buildEpoch(settings []debug.BuildSetting, now time.Time) time.Time {
	if buildSetting(settings, "vcs.modified") != "false" {
		return now
	}
	t, err := time.Parse(time.RFC3339, buildSetting(settings, "vcs.time"))
	if err != nil {
		return now
	}
	return t
}

// shortRevision returns an abbreviated vcs.revision for the description string,
// or "" when the binary carries no VCS stamps.
func shortRevision(settings []debug.BuildSetting) string {
	rev := buildSetting(settings, "vcs.revision")
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// vcsSettings returns the running binary's build settings, or nil when the
// build info is unavailable. It is wired up from main.go in a later task; see
// buildinfo_test.go for why it has no direct test of its own.
//
//lint:ignore U1000 wired up by main.go in a later task
func vcsSettings() []debug.BuildSetting {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return bi.Settings
}
