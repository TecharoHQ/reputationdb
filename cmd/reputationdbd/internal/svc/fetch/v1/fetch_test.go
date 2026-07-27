package fetchv1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/TecharoHQ/reputationdb/internal/dbstore/dbstoretest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// seedIndex writes an encoded version index into the fake store.
func seedIndex(t *testing.T, store *dbstoretest.Fake, versions ...*fetchv1.DatabaseVersion) {
	t.Helper()

	encoded, err := dbstore.EncodeIndex(&fetchv1.ListResponse{Versions: versions})
	if err != nil {
		t.Fatalf("EncodeIndex() error = %v", err)
	}
	store.Objects[dbstore.IndexKey] = encoded
}

func TestIndexIsCachedForTheTTL(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: "abc"})

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := newServer(store, discardLogger())
	s.now = func() time.Time { return now }

	for range 3 {
		if _, err := s.index(context.Background()); err != nil {
			t.Fatalf("index() error = %v", err)
		}
	}
	if len(store.Gets) != 1 {
		t.Errorf("index() read the bucket %d times within the TTL, want 1", len(store.Gets))
	}

	now = now.Add(indexCacheTTL + time.Second)
	if _, err := s.index(context.Background()); err != nil {
		t.Fatalf("index() error = %v", err)
	}
	if len(store.Gets) != 2 {
		t.Errorf("index() read the bucket %d times after the TTL lapsed, want 2", len(store.Gets))
	}
}

func TestIndexLoadErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	s := newServer(store, discardLogger())
	if _, err := s.index(context.Background()); err == nil {
		t.Fatal("index() error = nil, want the store error to propagate")
	}
}

// A refetch that fails must not leave a stale index installed as if it were
// fresh: the next call has to try the bucket again.
func TestIndexRefetchFailureDoesNotServeStale(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: "abc"})

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := newServer(store, discardLogger())
	s.now = func() time.Time { return now }

	if _, err := s.index(context.Background()); err != nil {
		t.Fatalf("index() error = %v", err)
	}

	now = now.Add(indexCacheTTL + time.Second)
	store.ListErr = errors.New("network is on fire")

	if _, err := s.index(context.Background()); err == nil {
		t.Fatal("index() error = nil after the TTL lapsed and the bucket failed, want an error")
	}

	store.ListErr = nil
	got, err := s.index(context.Background())
	if err != nil {
		t.Fatalf("index() error = %v once the bucket recovered", err)
	}
	if len(got.GetVersions()) != 1 {
		t.Errorf("index() = %d versions after recovery, want 1", len(got.GetVersions()))
	}
}

func TestListReturnsIndexVersions(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store,
		&fetchv1.DatabaseVersion{VersionId: "newest", RepoShasum: "aaa"},
		&fetchv1.DatabaseVersion{VersionId: "older", RepoShasum: "bbb"},
	)

	s := newServer(store, discardLogger())
	resp, err := s.List(context.Background(), connect.NewRequest(&fetchv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	got := resp.Msg.GetVersions()
	if len(got) != 2 {
		t.Fatalf("List() returned %d versions, want 2", len(got))
	}
	// Newest first, in the order the publisher wrote them.
	if got[0].GetVersionId() != "newest" || got[1].GetVersionId() != "older" {
		t.Errorf("List() order = [%q %q], want [newest older]", got[0].GetVersionId(), got[1].GetVersionId())
	}
	if got[0].GetRepoShasum() != "aaa" {
		t.Errorf("List() dropped metadata: repo_shasum = %q, want %q", got[0].GetRepoShasum(), "aaa")
	}
}

func TestListEmptyIndex(t *testing.T) {
	s := newServer(dbstoretest.New(), discardLogger())

	resp, err := s.List(context.Background(), connect.NewRequest(&fetchv1.ListRequest{}))
	if err != nil {
		t.Fatalf("List() error = %v, want an empty list rather than an error", err)
	}
	if len(resp.Msg.GetVersions()) != 0 {
		t.Errorf("List() returned %d versions, want 0", len(resp.Msg.GetVersions()))
	}
}

func TestListStoreFailureIsUnavailable(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	s := newServer(store, discardLogger())
	_, err := s.List(context.Background(), connect.NewRequest(&fetchv1.ListRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("List() code = %v, want %v", got, connect.CodeUnavailable)
	}
}

func TestValidVersionID(t *testing.T) {
	real := dbstore.VersionID([]byte("pretend this is an mmdb"))

	for _, tt := range []struct {
		name string
		id   string
		want bool
	}{
		{"a real version ID", real, true},
		{"empty", "", false},
		{"too short", real[:85], false},
		{"too long", real + "A", false},
		{"standard base64 padding", strings.Repeat("A", 84) + "==", false},
		{"standard base64 alphabet", strings.Repeat("A", 85) + "+", false},
		{"path separator", strings.Repeat("A", 85) + "/", false},
		{"traversal attempt", "../../etc/passwd", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := validVersionID(tt.id); got != tt.want {
				t.Errorf("validVersionID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestFindVersion(t *testing.T) {
	idx := &fetchv1.ListResponse{Versions: []*fetchv1.DatabaseVersion{
		{VersionId: "a", RepoShasum: "sha-a"},
		{VersionId: "b", RepoShasum: "sha-b"},
	}}

	if got := findVersion(idx, "b"); got == nil || got.GetRepoShasum() != "sha-b" {
		t.Errorf("findVersion(idx, \"b\") = %v, want the entry with repo_shasum sha-b", got)
	}
	if got := findVersion(idx, "c"); got != nil {
		t.Errorf("findVersion(idx, \"c\") = %v, want nil", got)
	}
	if got := findVersion(&fetchv1.ListResponse{}, "a"); got != nil {
		t.Errorf("findVersion(empty, \"a\") = %v, want nil", got)
	}
}
