package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// discardLogger returns a logger that writes nowhere, for tests that only care
// about return values.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// capturingLogger returns a logger that writes JSON log records to the
// returned buffer, for tests that need to assert on log output (e.g. that a
// warning was actually emitted).
func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// newTestRepo initializes a git repo in a temp dir with a single commit, and
// returns the repo path and the commit's hash.
func newTestRepo(t *testing.T, message string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit() error = %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := wt.Add("committed.txt"); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	return dir, hash.String()
}

func TestRepoMetadata(t *testing.T) {
	dir, hash := newTestRepo(t, "feat: a test commit")

	got, err := repoMetadata(discardLogger(), dir)
	if err != nil {
		t.Fatalf("repoMetadata() error = %v", err)
	}

	if got.Shasum != hash {
		t.Errorf("Shasum = %q, want %q", got.Shasum, hash)
	}
	if got.Message != "feat: a test commit" {
		t.Errorf("Message = %q, want %q", got.Message, "feat: a test commit")
	}
}

func TestRepoMetadataFromSubdirectory(t *testing.T) {
	dir, hash := newTestRepo(t, "feat: nested")

	sub := filepath.Join(dir, "cmd", "thing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	got, err := repoMetadata(discardLogger(), sub)
	if err != nil {
		t.Fatalf("repoMetadata() error = %v", err)
	}
	if got.Shasum != hash {
		t.Errorf("Shasum = %q, want %q", got.Shasum, hash)
	}
}

func TestRepoMetadataDirtyWorktreeWarnsButSucceeds(t *testing.T) {
	dir, hash := newTestRepo(t, "feat: clean commit")

	// Dirty the worktree after committing.
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	lg, logs := capturingLogger()
	got, err := repoMetadata(lg, dir)
	if err != nil {
		t.Fatalf("repoMetadata() error = %v, want success with a warning", err)
	}
	if got.Shasum != hash {
		t.Errorf("Shasum = %q, want %q", got.Shasum, hash)
	}

	if !bytes.Contains(logs.Bytes(), []byte(`"level":"WARN"`)) {
		t.Errorf("log output = %s, want a WARN-level record about the dirty worktree", logs)
	}
	if !bytes.Contains(logs.Bytes(), []byte("dirty worktree")) {
		t.Errorf("log output = %s, want a message mentioning the dirty worktree", logs)
	}
}

func TestRepoMetadataNotARepo(t *testing.T) {
	// t.TempDir() is not inside a git repo on any supported CI image.
	if _, err := repoMetadata(discardLogger(), t.TempDir()); err == nil {
		t.Error("repoMetadata() error = nil, want an error outside a git repo")
	}
}

func TestRepoMetadataUnbornHead(t *testing.T) {
	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit() error = %v", err)
	}

	if _, err := repoMetadata(discardLogger(), dir); err == nil {
		t.Error("repoMetadata() error = nil, want an error for a repo with an unborn HEAD")
	}
}
