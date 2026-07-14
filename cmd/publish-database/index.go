package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"google.golang.org/protobuf/proto"
)

// encodeIndex serializes the version index for storage: protobuf wire format,
// gzip-compressed.
//
// The index is a fetchv1.ListResponse rather than a bespoke message so that the
// fetch service can serve the decoded object straight back to callers.
func encodeIndex(idx *fetchv1.ListResponse) ([]byte, error) {
	raw, err := proto.Marshal(idx)
	if err != nil {
		return nil, fmt.Errorf("marshaling index: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)

	if _, err := gz.Write(raw); err != nil {
		return nil, fmt.Errorf("compressing index: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("flushing index: %w", err)
	}

	return buf.Bytes(), nil
}

// decodeIndex reverses encodeIndex.
func decodeIndex(gzipped []byte) (*fetchv1.ListResponse, error) {
	gz, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		return nil, fmt.Errorf("reading index gzip header: %w", err)
	}
	defer gz.Close()

	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("decompressing index: %w", err)
	}

	var idx fetchv1.ListResponse
	if err := proto.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("unmarshaling index: %w", err)
	}

	return &idx, nil
}

// insertVersion returns the version list with v prepended, trimmed to the
// maxVersions most recent entries. The list is ordered newest-first.
//
// Any existing entry with the same version ID is dropped: republishing an
// identical database is content-addressed to the same ID, and should refresh
// that version's position and metadata rather than duplicate it.
//
// evicted holds the versions that fell off the end. Their objects are
// deliberately left in the bucket, so callers should log rather than delete.
func insertVersion(versions []*fetchv1.DatabaseVersion, v *fetchv1.DatabaseVersion) (kept, evicted []*fetchv1.DatabaseVersion) {
	kept = append(kept, v)
	for _, old := range versions {
		if old.GetVersionId() == v.GetVersionId() {
			continue
		}
		kept = append(kept, old)
	}

	if len(kept) > maxVersions {
		evicted = kept[maxVersions:]
		kept = kept[:maxVersions:maxVersions]
	}

	return kept, evicted
}
