package dbcache_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/TecharoHQ/reputationdb/internal/dbstore/dbstoretest"
	"google.golang.org/protobuf/types/known/timestamppb"
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

func TestBucketSourceCurrentReturnsTheNewestVersion(t *testing.T) {
	published := time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC)
	newest := dbstore.VersionID([]byte("the newest database"))
	older := dbstore.VersionID([]byte("an older database"))

	store := dbstoretest.New()
	seedIndex(t, store,
		&fetchv1.DatabaseVersion{VersionId: newest, CreatedAt: timestamppb.New(published)},
		&fetchv1.DatabaseVersion{VersionId: older},
	)

	src := dbcache.NewBucketSource(store, discardLogger())
	got, err := src.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}

	if got.VersionID != newest {
		t.Errorf("Current() version = %q, want the first index entry %q", got.VersionID, newest)
	}
	if want := dbstore.ObjectKey(newest); got.Key != want {
		t.Errorf("Current() key = %q, want %q", got.Key, want)
	}
	if !got.CreatedAt.Equal(published) {
		t.Errorf("Current() created_at = %v, want %v", got.CreatedAt, published)
	}
}

// An index entry written before created_at existed must report the zero time,
// not the Unix epoch: the cache uses "is it zero" to decide whether to fall
// back to the mmdb's own build epoch.
func TestBucketSourceCurrentWithNoTimestamp(t *testing.T) {
	id := dbstore.VersionID([]byte("no timestamp"))

	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{VersionId: id})

	src := dbcache.NewBucketSource(store, discardLogger())
	got, err := src.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("Current() created_at = %v, want the zero time", got.CreatedAt)
	}
}

func TestBucketSourceCurrentEmptyIndex(t *testing.T) {
	src := dbcache.NewBucketSource(dbstoretest.New(), discardLogger())

	if _, err := src.Current(context.Background()); err == nil {
		t.Fatal("Current() error = nil for an empty index, want an error: there is nothing to load")
	}
}

func TestBucketSourceCurrentEntryWithNoVersionID(t *testing.T) {
	store := dbstoretest.New()
	seedIndex(t, store, &fetchv1.DatabaseVersion{RepoShasum: "abc"})

	src := dbcache.NewBucketSource(store, discardLogger())
	if _, err := src.Current(context.Background()); err == nil {
		t.Fatal("Current() error = nil for an entry with no version ID, want an error")
	}
}

func TestBucketSourceCurrentStoreFailure(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	src := dbcache.NewBucketSource(store, discardLogger())
	if _, err := src.Current(context.Background()); err == nil {
		t.Fatal("Current() error = nil, want the store error to propagate")
	}
}

func TestBucketSourceOpen(t *testing.T) {
	id := dbstore.VersionID([]byte("a database"))
	key := dbstore.ObjectKey(id)

	store := dbstoretest.New()
	store.Objects[key] = []byte("compressed database bytes")

	src := dbcache.NewBucketSource(store, discardLogger())
	body, err := src.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if string(got) != "compressed database bytes" {
		t.Errorf("Open() body = %q, want the stored object", got)
	}
}

func TestBucketSourceOpenMissingObject(t *testing.T) {
	src := dbcache.NewBucketSource(dbstoretest.New(), discardLogger())

	if _, err := src.Open(context.Background(), dbstore.ObjectKey("nope")); err == nil {
		t.Fatal("Open() error = nil for an object that isn't there, want an error")
	}
}
