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
	fetchv1connect "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1/fetchv1connect"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/TecharoHQ/reputationdb/internal/dbstore/dbstoretest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Compile-time proof that Server can be handed to NewFetchServiceHandler.
var _ fetchv1connect.FetchServiceHandler = (*Server)(nil)

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

func TestInfoReturnsVersionMetadata(t *testing.T) {
	id := dbstore.VersionID([]byte("a database"))
	published := time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC)

	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{
		VersionId:         id,
		RepoShasum:        "0123456789abcdef",
		RepoCommitMessage: "feat: add a source",
		CreatedAt:         timestamppb.New(published),
	})

	s := newServer(store, discardLogger())
	resp, err := s.Info(context.Background(), connect.NewRequest(&fetchv1.InfoRequest{VersionId: id}))
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	got := resp.Msg.GetVersion()
	if got.GetVersionId() != id {
		t.Errorf("Info() version_id = %q, want %q", got.GetVersionId(), id)
	}
	if got.GetRepoShasum() != "0123456789abcdef" {
		t.Errorf("Info() repo_shasum = %q, want %q", got.GetRepoShasum(), "0123456789abcdef")
	}
	if got.GetRepoCommitMessage() != "feat: add a source" {
		t.Errorf("Info() repo_commit_message = %q, want %q", got.GetRepoCommitMessage(), "feat: add a source")
	}
	if !got.GetCreatedAt().AsTime().Equal(published) {
		t.Errorf("Info() created_at = %v, want %v", got.GetCreatedAt().AsTime(), published)
	}
}

func TestInfoMalformedVersionIDIsInvalidArgument(t *testing.T) {
	s := newServer(dbstoretest.New(), discardLogger())

	for _, id := range []string{"", "nonsense", "../../etc/passwd"} {
		_, err := s.Info(context.Background(), connect.NewRequest(&fetchv1.InfoRequest{VersionId: id}))
		if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
			t.Errorf("Info(%q) code = %v, want %v", id, got, connect.CodeInvalidArgument)
		}
	}
}

// Info reports metadata, and a version that has aged out of the index has none
// left to report, even though its object is still in the bucket. Fetch is the
// endpoint that still serves those; see TestFetchServesAVersionThatAgedOut.
func TestInfoUnknownVersionIsNotFound(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: dbstore.VersionID([]byte("indexed"))})

	s := newServer(store, discardLogger())
	missing := dbstore.VersionID([]byte("never published"))

	_, err := s.Info(context.Background(), connect.NewRequest(&fetchv1.InfoRequest{VersionId: missing}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Errorf("Info() code = %v, want %v", got, connect.CodeNotFound)
	}
}

func TestInfoStoreFailureIsUnavailable(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	s := newServer(store, discardLogger())
	id := dbstore.VersionID([]byte("whatever"))

	_, err := s.Info(context.Background(), connect.NewRequest(&fetchv1.InfoRequest{VersionId: id}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("Info() code = %v, want %v", got, connect.CodeUnavailable)
	}
}

func TestFetchReturnsAPresignedURLAndMetadata(t *testing.T) {
	id := dbstore.VersionID([]byte("a database"))

	store := dbstoretest.New()
	store.Objects[dbstore.ObjectKey(id)] = []byte("compressed database bytes")
	seedIndex(t, store, &fetchv1.DatabaseVersion{
		VersionId:  id,
		RepoShasum: "0123456789abcdef",
	})

	s := newServer(store, discardLogger())
	resp, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: id}))
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	if got := resp.Msg.GetVersion().GetVersionId(); got != id {
		t.Errorf("Fetch() version_id = %q, want %q", got, id)
	}
	if got := resp.Msg.GetVersion().GetRepoShasum(); got != "0123456789abcdef" {
		t.Errorf("Fetch() repo_shasum = %q, want the index entry's", got)
	}
	if got := resp.Msg.GetLifetime().AsDuration(); got != clientLifetime {
		t.Errorf("Fetch() lifetime = %v, want %v", got, clientLifetime)
	}
	if !strings.Contains(resp.Msg.GetPresignedUrl(), dbstore.ObjectKey(id)) {
		t.Errorf("Fetch() presigned_url = %q, want it to address %q", resp.Msg.GetPresignedUrl(), dbstore.ObjectKey(id))
	}
	if len(store.Presigns) != 1 || store.Presigns[0] != dbstore.ObjectKey(id) {
		t.Errorf("Fetch() presigned keys = %v, want [%q]", store.Presigns, dbstore.ObjectKey(id))
	}
}

// README.md promises that a version which has aged out of the ten-entry index
// is still downloadable by a client that knows its ID. Its provenance is gone,
// so the response carries the ID and nothing else.
func TestFetchServesAVersionThatAgedOut(t *testing.T) {
	agedOut := dbstore.VersionID([]byte("an old database"))

	store := dbstoretest.New()
	store.Objects[dbstore.ObjectKey(agedOut)] = []byte("still here")
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: dbstore.VersionID([]byte("the current one"))})

	s := newServer(store, discardLogger())
	resp, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: agedOut}))
	if err != nil {
		t.Fatalf("Fetch() error = %v, want the aged-out version to still be downloadable", err)
	}

	if got := resp.Msg.GetVersion().GetVersionId(); got != agedOut {
		t.Errorf("Fetch() version_id = %q, want %q", got, agedOut)
	}
	if got := resp.Msg.GetVersion().GetRepoShasum(); got != "" {
		t.Errorf("Fetch() repo_shasum = %q, want it empty: the index no longer knows", got)
	}
	if !strings.Contains(resp.Msg.GetPresignedUrl(), dbstore.ObjectKey(agedOut)) {
		t.Errorf("Fetch() presigned_url = %q, want it to address %q", resp.Msg.GetPresignedUrl(), dbstore.ObjectKey(agedOut))
	}
}

func TestFetchUnknownVersionIsNotFound(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: dbstore.VersionID([]byte("indexed"))})

	s := newServer(store, discardLogger())
	missing := dbstore.VersionID([]byte("never published"))

	_, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: missing}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Errorf("Fetch() code = %v, want %v", got, connect.CodeNotFound)
	}
	// A version with no object must never be handed a URL that 404s on download.
	if len(store.Presigns) != 0 {
		t.Errorf("Fetch() presigned %v for a version with no object, want none", store.Presigns)
	}
}

func TestFetchMalformedVersionIDIsInvalidArgument(t *testing.T) {
	s := newServer(dbstoretest.New(), discardLogger())

	for _, id := range []string{"", "nonsense", "../../etc/passwd"} {
		_, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: id}))
		if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
			t.Errorf("Fetch(%q) code = %v, want %v", id, got, connect.CodeInvalidArgument)
		}
	}
}

func TestFetchPresignFailureIsUnavailable(t *testing.T) {
	id := dbstore.VersionID([]byte("a database"))

	store := dbstoretest.New()
	store.Objects[dbstore.ObjectKey(id)] = []byte("compressed database bytes")
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: id})
	store.PresignErr = errors.New("no credentials")

	s := newServer(store, discardLogger())
	_, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: id}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("Fetch() code = %v, want %v", got, connect.CodeUnavailable)
	}
}

func TestFetchStoreFailureIsUnavailable(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	s := newServer(store, discardLogger())
	id := dbstore.VersionID([]byte("whatever"))

	_, err := s.Fetch(context.Background(), connect.NewRequest(&fetchv1.FetchRequest{VersionId: id}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Errorf("Fetch() code = %v, want %v", got, connect.CodeUnavailable)
	}
}
