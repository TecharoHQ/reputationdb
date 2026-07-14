package main

import (
	"fmt"
	"testing"
	"time"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// testVersion builds a DatabaseVersion with a distinguishable ID and timestamp.
func testVersion(id string, at time.Time) *fetchv1.DatabaseVersion {
	return &fetchv1.DatabaseVersion{
		CreatedAt:         timestamppb.New(at),
		RepoShasum:        "abc123",
		RepoCommitMessage: "commit for " + id,
		VersionId:         id,
	}
}

func TestEncodeDecodeIndexRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	want := &fetchv1.ListResponse{
		Versions: []*fetchv1.DatabaseVersion{
			testVersion("one", at),
			testVersion("two", at.Add(-time.Hour)),
		},
	}

	encoded, err := encodeIndex(want)
	if err != nil {
		t.Fatalf("encodeIndex() error = %v", err)
	}

	// gzip magic number: the object must actually be gzipped.
	if len(encoded) < 2 || encoded[0] != 0x1f || encoded[1] != 0x8b {
		t.Errorf("encodeIndex() did not produce gzip data, first bytes = %x", encoded[:min(2, len(encoded))])
	}

	got, err := decodeIndex(encoded)
	if err != nil {
		t.Fatalf("decodeIndex() error = %v", err)
	}

	if len(got.GetVersions()) != 2 {
		t.Fatalf("decodeIndex() returned %d versions, want 2", len(got.GetVersions()))
	}
	if got.GetVersions()[0].GetVersionId() != "one" {
		t.Errorf("versions[0].VersionId = %q, want %q", got.GetVersions()[0].GetVersionId(), "one")
	}
	if got.GetVersions()[1].GetRepoCommitMessage() != "commit for two" {
		t.Errorf("versions[1].RepoCommitMessage = %q, want %q", got.GetVersions()[1].GetRepoCommitMessage(), "commit for two")
	}
	if !got.GetVersions()[0].GetCreatedAt().AsTime().Equal(at) {
		t.Errorf("versions[0].CreatedAt = %v, want %v", got.GetVersions()[0].GetCreatedAt().AsTime(), at)
	}
}

func TestDecodeIndexEmpty(t *testing.T) {
	encoded, err := encodeIndex(&fetchv1.ListResponse{})
	if err != nil {
		t.Fatalf("encodeIndex() error = %v", err)
	}

	got, err := decodeIndex(encoded)
	if err != nil {
		t.Fatalf("decodeIndex() error = %v", err)
	}
	if len(got.GetVersions()) != 0 {
		t.Errorf("decodeIndex() returned %d versions, want 0", len(got.GetVersions()))
	}
}

func TestDecodeIndexGarbage(t *testing.T) {
	if _, err := decodeIndex([]byte("this is not gzip")); err == nil {
		t.Error("decodeIndex() error = nil, want an error for non-gzip input")
	}
}

func TestInsertVersionPrepends(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	existing := []*fetchv1.DatabaseVersion{testVersion("old", at.Add(-time.Hour))}

	kept, evicted := insertVersion(existing, testVersion("new", at))

	if len(kept) != 2 {
		t.Fatalf("insertVersion() kept %d versions, want 2", len(kept))
	}
	if kept[0].GetVersionId() != "new" {
		t.Errorf("kept[0].VersionId = %q, want %q (newest first)", kept[0].GetVersionId(), "new")
	}
	if kept[1].GetVersionId() != "old" {
		t.Errorf("kept[1].VersionId = %q, want %q", kept[1].GetVersionId(), "old")
	}
	if len(evicted) != 0 {
		t.Errorf("insertVersion() evicted %d versions, want 0", len(evicted))
	}
}

func TestInsertVersionIntoEmpty(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	kept, evicted := insertVersion(nil, testVersion("first", at))

	if len(kept) != 1 || kept[0].GetVersionId() != "first" {
		t.Errorf("insertVersion(nil, ...) kept = %v, want one entry with ID %q", kept, "first")
	}
	if len(evicted) != 0 {
		t.Errorf("insertVersion() evicted %d versions, want 0", len(evicted))
	}
}

func TestInsertVersionTrimsToMax(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// Fill the index to capacity: v0 (newest) .. v9 (oldest).
	var existing []*fetchv1.DatabaseVersion
	for i := range maxVersions {
		existing = append(existing, testVersion(fmt.Sprintf("v%d", i), at.Add(-time.Duration(i)*time.Hour)))
	}

	kept, evicted := insertVersion(existing, testVersion("newest", at.Add(time.Hour)))

	if len(kept) != maxVersions {
		t.Fatalf("insertVersion() kept %d versions, want %d", len(kept), maxVersions)
	}
	if kept[0].GetVersionId() != "newest" {
		t.Errorf("kept[0].VersionId = %q, want %q", kept[0].GetVersionId(), "newest")
	}
	if last := kept[len(kept)-1].GetVersionId(); last != "v8" {
		t.Errorf("kept[last].VersionId = %q, want %q", last, "v8")
	}
	if len(evicted) != 1 {
		t.Fatalf("insertVersion() evicted %d versions, want 1", len(evicted))
	}
	if evicted[0].GetVersionId() != "v9" {
		t.Errorf("evicted[0].VersionId = %q, want %q (the oldest)", evicted[0].GetVersionId(), "v9")
	}
}

func TestInsertVersionDeduplicates(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	existing := []*fetchv1.DatabaseVersion{
		testVersion("a", at.Add(-time.Hour)),
		testVersion("dupe", at.Add(-2*time.Hour)),
		testVersion("b", at.Add(-3*time.Hour)),
	}

	// Republishing an identical database produces the same version ID.
	kept, evicted := insertVersion(existing, testVersion("dupe", at))

	if len(kept) != 3 {
		t.Fatalf("insertVersion() kept %d versions, want 3 (no duplicate entry)", len(kept))
	}
	if kept[0].GetVersionId() != "dupe" {
		t.Errorf("kept[0].VersionId = %q, want %q (moved to newest)", kept[0].GetVersionId(), "dupe")
	}
	if !kept[0].GetCreatedAt().AsTime().Equal(at) {
		t.Errorf("kept[0].CreatedAt = %v, want the new timestamp %v", kept[0].GetCreatedAt().AsTime(), at)
	}
	for _, v := range kept[1:] {
		if v.GetVersionId() == "dupe" {
			t.Error("insertVersion() left a stale duplicate entry in the index")
		}
	}
	if len(evicted) != 0 {
		t.Errorf("insertVersion() evicted %d versions, want 0 (a re-publish evicts nothing)", len(evicted))
	}
}

func TestInsertVersionDoesNotMutateInput(t *testing.T) {
	at := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	existing := []*fetchv1.DatabaseVersion{testVersion("old", at.Add(-time.Hour))}

	insertVersion(existing, testVersion("new", at))

	if len(existing) != 1 || existing[0].GetVersionId() != "old" {
		t.Error("insertVersion() mutated its input slice")
	}
}
