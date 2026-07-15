package main

import (
	"runtime/debug"
	"testing"
	"time"
)

func TestBuildEpoch(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	commit := time.Date(2026, 7, 14, 22, 42, 42, 0, time.UTC)

	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     time.Time
	}{
		{
			name: "clean tree uses the commit timestamp, making the build reproducible",
			settings: []debug.BuildSetting{
				{Key: "vcs.time", Value: "2026-07-14T22:42:42Z"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: commit,
		},
		{
			name: "dirty tree falls back to now: the inputs are not pinned",
			settings: []debug.BuildSetting{
				{Key: "vcs.time", Value: "2026-07-14T22:42:42Z"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: now,
		},
		{
			name: "missing vcs.time falls back to now",
			settings: []debug.BuildSetting{
				{Key: "vcs.modified", Value: "false"},
			},
			want: now,
		},
		{
			name: "unparseable vcs.time falls back to now",
			settings: []debug.BuildSetting{
				{Key: "vcs.time", Value: "not a timestamp"},
				{Key: "vcs.modified", Value: "false"},
			},
			want: now,
		},
		{
			name:     "a binary built outside a checkout has no stamps at all",
			settings: nil,
			want:     now,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildEpoch(tt.settings, now); !got.Equal(tt.want) {
				t.Errorf("buildEpoch() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestShortRevision(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name: "a full sha is shortened",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "2e65f968c6ab6ec9a9957582b267a13ea8e17ab7"},
			},
			want: "2e65f968",
		},
		{
			name: "a short value is passed through",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "2e65"},
			},
			want: "2e65",
		},
		{
			name:     "no stamp yields no revision",
			settings: nil,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortRevision(tt.settings); got != tt.want {
				t.Errorf("shortRevision() = %q, want %q", got, tt.want)
			}
		})
	}
}
