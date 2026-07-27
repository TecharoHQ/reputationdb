package dbstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/TecharoHQ/reputationdb/internal/dbstore/dbstoretest"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// discardLogger returns a logger that writes nowhere, for tests that only care
// about return values.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// Compile-time proof that the real client satisfies the interface the fake
// stands in for.
var _ dbstore.Store = (*simplestorage.Client)(nil)
var _ dbstore.Store = (*dbstoretest.Fake)(nil)

func TestLoadIndexMissingReturnsEmpty(t *testing.T) {
	got, err := dbstore.LoadIndex(context.Background(), dbstoretest.New(), discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v, want nil for a first publish", err)
	}
	if len(got.GetVersions()) != 0 {
		t.Errorf("LoadIndex() returned %d versions, want 0", len(got.GetVersions()))
	}
}

func TestLoadIndexExisting(t *testing.T) {
	store := dbstoretest.New()
	encoded, err := dbstore.EncodeIndex(&fetchv1.ListResponse{
		Versions: []*fetchv1.DatabaseVersion{{VersionId: "abc"}},
	})
	if err != nil {
		t.Fatalf("EncodeIndex() error = %v", err)
	}
	store.Objects[dbstore.IndexKey] = encoded

	got, err := dbstore.LoadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 1 || got.GetVersions()[0].GetVersionId() != "abc" {
		t.Errorf("LoadIndex() = %v, want one version with ID %q", got.GetVersions(), "abc")
	}
}

// A missing index means "first publish"; a broken listing means "we don't know".
// Conflating the two would silently discard the version history.
func TestLoadIndexListErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	if _, err := dbstore.LoadIndex(context.Background(), store, discardLogger()); err == nil {
		t.Fatal("LoadIndex() error = nil, want the list error to propagate")
	}
}

func TestLoadIndexGetErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.Objects[dbstore.IndexKey] = []byte("present")
	store.GetErr = errors.New("network is on fire")

	if _, err := dbstore.LoadIndex(context.Background(), store, discardLogger()); err == nil {
		t.Fatal("LoadIndex() error = nil, want the get error to propagate")
	}
}

// The fake's List ignores prefixes, so LoadIndex must exact-match the key.
func TestLoadIndexIgnoresOtherObjects(t *testing.T) {
	store := dbstoretest.New()
	store.Objects["databases/somehash.mmdb.zst"] = []byte("not the index")

	got, err := dbstore.LoadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 0 {
		t.Errorf("LoadIndex() returned %d versions, want 0", len(got.GetVersions()))
	}
}

func TestSaveIndexRoundTrips(t *testing.T) {
	store := dbstoretest.New()
	want := &fetchv1.ListResponse{Versions: []*fetchv1.DatabaseVersion{{VersionId: "xyz"}}}

	if err := dbstore.SaveIndex(context.Background(), store, want); err != nil {
		t.Fatalf("SaveIndex() error = %v", err)
	}
	if len(store.Puts) != 1 || store.Puts[0] != dbstore.IndexKey {
		t.Fatalf("SaveIndex() put keys = %v, want [%q]", store.Puts, dbstore.IndexKey)
	}

	got, err := dbstore.LoadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 1 || got.GetVersions()[0].GetVersionId() != "xyz" {
		t.Errorf("round-trip lost the index contents: %v", got.GetVersions())
	}
}

func TestSaveIndexErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.PutErr = errors.New("denied")

	if err := dbstore.SaveIndex(context.Background(), store, &fetchv1.ListResponse{}); err == nil {
		t.Fatal("SaveIndex() error = nil, want the put error to propagate")
	}
}

func TestPutDatabase(t *testing.T) {
	store := dbstoretest.New()
	body := []byte("compressed database bytes")

	if err := dbstore.PutDatabase(context.Background(), store, "databases/hash.mmdb.zst", body); err != nil {
		t.Fatalf("PutDatabase() error = %v", err)
	}

	got, ok := store.Objects["databases/hash.mmdb.zst"]
	if !ok {
		t.Fatal("PutDatabase() did not store the object at the expected key")
	}
	if !bytes.Equal(got, body) {
		t.Errorf("PutDatabase() stored %q, want %q", got, body)
	}
}

func TestPutDatabaseErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.PutErr = errors.New("denied")

	if err := dbstore.PutDatabase(context.Background(), store, "databases/hash.mmdb.zst", []byte("x")); err == nil {
		t.Fatal("PutDatabase() error = nil, want the put error to propagate")
	}
}

func TestExists(t *testing.T) {
	store := dbstoretest.New()
	store.Objects["databases/present.mmdb.zst"] = []byte("here")

	for _, tt := range []struct {
		name string
		key  string
		want bool
	}{
		{"present", "databases/present.mmdb.zst", true},
		{"absent", "databases/absent.mmdb.zst", false},
		{"prefix of a present key is not itself present", "databases/present", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dbstore.Exists(context.Background(), store, tt.key)
			if err != nil {
				t.Fatalf("Exists() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Exists(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestExistsListErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.ListErr = errors.New("network is on fire")

	if _, err := dbstore.Exists(context.Background(), store, dbstore.IndexKey); err == nil {
		t.Fatal("Exists() error = nil, want the list error to propagate")
	}
}

func TestPresignGetErrorPropagates(t *testing.T) {
	store := dbstoretest.New()
	store.PresignErr = errors.New("no credentials")

	if _, err := dbstore.PresignGet(context.Background(), store, "databases/x.mmdb.zst", time.Hour); err == nil {
		t.Fatal("PresignGet() error = nil, want the presign error to propagate")
	}
}
