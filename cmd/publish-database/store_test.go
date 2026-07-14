package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"os"
	"path/filepath"
	"testing"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// fakeStore is an in-memory objectStore.
//
// It ignores every ClientOption and ListOption it is handed: those types mutate
// unexported structs in simplestorage, so a fake outside that package cannot
// read them back. Production code must therefore never depend on the server
// honoring a list prefix for correctness.
type fakeStore struct {
	objects map[string][]byte
	putErr  error
	getErr  error
	listErr error

	puts []string // keys passed to Put, in order
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}}
}

func (f *fakeStore) Get(ctx context.Context, key string, opts ...simplestorage.ClientOption) (*simplestorage.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("simplestorage: can't get bucket/" + key + ": NoSuchKey")
	}
	return &simplestorage.Object{
		Key:  key,
		Size: int64(len(body)),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func (f *fakeStore) Put(ctx context.Context, obj *simplestorage.Object, opts ...simplestorage.ClientOption) (*simplestorage.Object, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, err
	}
	f.objects[obj.Key] = body
	f.puts = append(f.puts, obj.Key)
	return obj, nil
}

func (f *fakeStore) List(ctx context.Context, opts ...simplestorage.ListOption) iter.Seq2[*simplestorage.Object, error] {
	return func(yield func(*simplestorage.Object, error) bool) {
		if f.listErr != nil {
			yield(nil, f.listErr)
			return
		}
		for key, body := range f.objects {
			if !yield(&simplestorage.Object{Key: key, Size: int64(len(body))}, nil) {
				return
			}
		}
	}
}

// Compile-time proof that the real client satisfies the interface the fake stands in for.
var _ objectStore = (*simplestorage.Client)(nil)

func TestLoadIndexMissingReturnsEmpty(t *testing.T) {
	got, err := loadIndex(context.Background(), newFakeStore(), discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v, want nil for a first publish", err)
	}
	if len(got.GetVersions()) != 0 {
		t.Errorf("loadIndex() returned %d versions, want 0", len(got.GetVersions()))
	}
}

func TestLoadIndexExisting(t *testing.T) {
	store := newFakeStore()
	encoded, err := encodeIndex(&fetchv1.ListResponse{
		Versions: []*fetchv1.DatabaseVersion{{VersionId: "abc"}},
	})
	if err != nil {
		t.Fatalf("encodeIndex() error = %v", err)
	}
	store.objects[indexKey] = encoded

	got, err := loadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 1 || got.GetVersions()[0].GetVersionId() != "abc" {
		t.Errorf("loadIndex() = %v, want one version with ID %q", got.GetVersions(), "abc")
	}
}

// A missing index means "first publish"; a broken listing means "we don't know".
// Conflating the two would silently discard the version history.
func TestLoadIndexListErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("network is on fire")

	if _, err := loadIndex(context.Background(), store, discardLogger()); err == nil {
		t.Fatal("loadIndex() error = nil, want the list error to propagate")
	}
}

func TestLoadIndexGetErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.objects[indexKey] = []byte("present")
	store.getErr = errors.New("network is on fire")

	if _, err := loadIndex(context.Background(), store, discardLogger()); err == nil {
		t.Fatal("loadIndex() error = nil, want the get error to propagate")
	}
}

// The fake's List ignores prefixes, so loadIndex must exact-match the key.
func TestLoadIndexIgnoresOtherObjects(t *testing.T) {
	store := newFakeStore()
	store.objects["databases/somehash.mmdb.zst"] = []byte("not the index")

	got, err := loadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 0 {
		t.Errorf("loadIndex() returned %d versions, want 0", len(got.GetVersions()))
	}
}

func TestSaveIndexRoundTrips(t *testing.T) {
	store := newFakeStore()
	want := &fetchv1.ListResponse{Versions: []*fetchv1.DatabaseVersion{{VersionId: "xyz"}}}

	if err := saveIndex(context.Background(), store, want); err != nil {
		t.Fatalf("saveIndex() error = %v", err)
	}
	if len(store.puts) != 1 || store.puts[0] != indexKey {
		t.Fatalf("saveIndex() put keys = %v, want [%q]", store.puts, indexKey)
	}

	got, err := loadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 1 || got.GetVersions()[0].GetVersionId() != "xyz" {
		t.Errorf("round-trip lost the index contents: %v", got.GetVersions())
	}
}

func TestSaveIndexErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.putErr = errors.New("denied")

	if err := saveIndex(context.Background(), store, &fetchv1.ListResponse{}); err == nil {
		t.Fatal("saveIndex() error = nil, want the put error to propagate")
	}
}

func TestPutDatabase(t *testing.T) {
	store := newFakeStore()
	body := []byte("compressed database bytes")

	if err := putDatabase(context.Background(), store, "databases/hash.mmdb.zst", body); err != nil {
		t.Fatalf("putDatabase() error = %v", err)
	}

	got, ok := store.objects["databases/hash.mmdb.zst"]
	if !ok {
		t.Fatal("putDatabase() did not store the object at the expected key")
	}
	if !bytes.Equal(got, body) {
		t.Errorf("putDatabase() stored %q, want %q", got, body)
	}
}

func TestPutDatabaseErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.putErr = errors.New("denied")

	if err := putDatabase(context.Background(), store, "databases/hash.mmdb.zst", []byte("x")); err == nil {
		t.Fatal("putDatabase() error = nil, want the put error to propagate")
	}
}

// TestRunPublishesDatabaseAndIndex drives run() end to end against fakeStore,
// locking in the composition that per-function unit tests cannot see: that
// run() actually wires loadIndex/putDatabase/saveIndex together in the right
// order.
func TestRunPublishesDatabaseAndIndex(t *testing.T) {
	dir, _ := newTestRepo(t, "feat: add database")
	dbPath := filepath.Join(dir, "reputationdb.mmdb")
	raw := []byte("pretend this is an mmdb")
	if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store := newFakeStore()
	if err := run(context.Background(), discardLogger(), store, dbPath); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	wantKey := objectKey(versionID(raw))
	if _, ok := store.objects[wantKey]; !ok {
		t.Errorf("run() did not upload the database at %q; objects = %v", wantKey, store.puts)
	}
	if _, ok := store.objects[indexKey]; !ok {
		t.Fatal("run() did not write the version index")
	}

	idx, err := loadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if len(idx.GetVersions()) != 1 || idx.GetVersions()[0].GetVersionId() != versionID(raw) {
		t.Errorf("index versions = %v, want exactly one version with ID %q", idx.GetVersions(), versionID(raw))
	}

	// Republishing the same bytes is content-addressed: the index should still
	// have exactly one version afterward, not two.
	if err := run(context.Background(), discardLogger(), store, dbPath); err != nil {
		t.Fatalf("second run() error = %v", err)
	}
	idx, err = loadIndex(context.Background(), store, discardLogger())
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
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

	store := newFakeStore()
	store.putErr = errors.New("network is on fire")

	if err := run(context.Background(), discardLogger(), store, dbPath); err == nil {
		t.Fatal("run() error = nil, want the upload error to propagate")
	}

	if _, ok := store.objects[indexKey]; ok {
		t.Error("run() wrote the index even though the database upload failed")
	}
	if len(store.puts) != 0 {
		t.Errorf("run() recorded successful puts = %v, want none since Put always fails", store.puts)
	}
}
