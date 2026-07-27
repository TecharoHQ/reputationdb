package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/TecharoHQ/reputationdb/internal/dbstore/dbstoretest"
)

// TestRunPublishesDatabaseAndIndex drives run() end to end against the fake
// store, locking in the composition that per-function unit tests cannot see:
// that run() actually wires LoadIndex/PutDatabase/SaveIndex together in the
// right order.
func TestRunPublishesDatabaseAndIndex(t *testing.T) {
	dir, _ := newTestRepo(t, "feat: add database")
	dbPath := filepath.Join(dir, "reputationdb.mmdb")
	raw := []byte("pretend this is an mmdb")
	if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := dbstoretest.New()
	if err := run(context.Background(), discardLogger(), store, dbPath); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	wantKey := dbstore.ObjectKey(dbstore.VersionID(raw))
	if _, ok := store.Objects[wantKey]; !ok {
		t.Errorf("run() did not upload the database at %q; objects = %v", wantKey, store.Puts)
	}
	if _, ok := store.Objects[dbstore.IndexKey]; !ok {
		t.Fatal("run() did not write the version index")
	}

	idx, err := dbstore.LoadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v", err)
	}
	if len(idx.GetVersions()) != 1 || idx.GetVersions()[0].GetVersionId() != dbstore.VersionID(raw) {
		t.Errorf("index versions = %v, want exactly one version with ID %q", idx.GetVersions(), dbstore.VersionID(raw))
	}

	// Republishing the same bytes is content-addressed: the index should still
	// have exactly one version afterward, not two.
	if err := run(context.Background(), discardLogger(), store, dbPath); err != nil {
		t.Fatalf("second run() error = %v", err)
	}
	idx, err = dbstore.LoadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("LoadIndex() error = %v", err)
	}
	if len(idx.GetVersions()) != 1 {
		t.Errorf("republishing identical bytes produced %d versions, want 1", len(idx.GetVersions()))
	}
}

// TestRunFailedUploadNeverWritesIndex locks in the ordering invariant: if the
// database upload fails, run() must return an error and must not have written
// the index, so the index can never advertise a database object that does not
// exist.
func TestRunFailedUploadNeverWritesIndex(t *testing.T) {
	dir, _ := newTestRepo(t, "feat: add database")
	dbPath := filepath.Join(dir, "reputationdb.mmdb")
	if err := os.WriteFile(dbPath, []byte("pretend this is an mmdb"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := dbstoretest.New()
	store.PutErr = errors.New("network is on fire")

	if err := run(context.Background(), discardLogger(), store, dbPath); err == nil {
		t.Fatal("run() error = nil, want the upload error to propagate")
	}

	if _, ok := store.Objects[dbstore.IndexKey]; ok {
		t.Error("run() wrote the index even though the database upload failed")
	}
	if len(store.Puts) != 0 {
		t.Errorf("run() recorded successful puts = %v, want none since Put always fails", store.Puts)
	}
}
