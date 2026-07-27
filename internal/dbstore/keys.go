// Package dbstore holds the on-Tigris layout of published reputation
// databases: how a database's contents map to a version ID, where that
// version's object lives, and how the index of recent versions is encoded.
//
// It exists so that the publisher (cmd/publish-database) and the API server
// (cmd/reputationdbd) agree on that layout by construction, rather than by two
// copies of the same constants drifting apart.
package dbstore

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"
)

const (
	// DatabasePrefix is the folder every published database object lives under.
	DatabasePrefix = "databases/"
	// IndexKey is the bucket-root object holding the version index.
	IndexKey = "versions.pb.gz"
	// MaxVersions is how many of the most recent versions the index retains.
	MaxVersions = 10
	// VersionIDLength is how many characters VersionID returns: the unpadded
	// base64 encoding of 64 bytes.
	VersionIDLength = 86
)

// VersionID returns the identity of a database: the unpadded URL-safe base64
// encoding of the SHA-512 of its uncompressed contents.
//
// The raw bytes are hashed rather than the compressed upload so that the ID
// describes the database itself. Recompressing an identical database at a
// different zstd level yields the same version ID.
func VersionID(raw []byte) string {
	sum := sha512.Sum512(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ObjectKey returns the bucket key a database with the given version ID is
// stored at.
func ObjectKey(id string) string {
	return fmt.Sprintf("%s%s.mmdb.zst", DatabasePrefix, id)
}
