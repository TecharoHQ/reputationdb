package fetchv1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
