package main

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const (
	// databasePrefix is the folder every published database object lives under.
	databasePrefix = "databases/"
	// indexKey is the bucket-root object holding the version index.
	indexKey = "versions.pb.gz"
	// maxVersions is how many of the most recent versions the index retains.
	maxVersions = 10
)

// versionID returns the identity of a database: the unpadded URL-safe base64
// encoding of the SHA-512 of its uncompressed contents.
//
// The raw bytes are hashed rather than the compressed upload so that the ID
// describes the database itself. Recompressing an identical database at a
// different zstd level yields the same version ID.
func versionID(raw []byte) string {
	sum := sha512.Sum512(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// objectKey returns the bucket key a database with the given version ID is
// stored at.
func objectKey(id string) string {
	return fmt.Sprintf("%s%s.mmdb.zst", databasePrefix, id)
}

// compressDatabase zstd-compresses a database for upload.
func compressDatabase(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}
	defer enc.Close()

	// The mmdb is already fully in memory (it must be, to be hashed), so
	// EncodeAll avoids the extra plumbing of a streaming encoder for no gain.
	return enc.EncodeAll(raw, make([]byte, 0, len(raw)/3)), nil
}
