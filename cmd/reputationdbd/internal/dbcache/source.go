// Package dbcache keeps a copy of the newest published reputation database on
// local disk, memory-maps it for lookups, and swaps in a newer build when one
// is published.
//
// It is the in-process counterpart of caddy/maat's store: the same rotation
// logic, but it resolves the current build by reading the version index
// straight out of the bucket through internal/dbstore rather than over the
// public fetch API. reputationdbd is the server behind that API, so calling it
// would be a round trip through its own front door.
package dbcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/TecharoHQ/reputationdb/internal/dbstore"
)

// Build describes the newest published database as the bucket currently sees
// it.
type Build struct {
	// VersionID identifies the contents: the same content-addressed ID the
	// fetch API serves. An unchanged ID means unchanged data.
	VersionID string
	// Key is the bucket key the compressed database object lives at.
	Key string
	// CreatedAt is when the build was published, or the zero time if the index
	// entry didn't say.
	CreatedAt time.Time
}

// Source resolves the newest published database and opens its bytes.
type Source interface {
	// Current reports the newest published build. It returns an error rather
	// than a zero Build when nothing has been published.
	Current(ctx context.Context) (Build, error)
	// Open returns the zstd-compressed database stored at key. The caller
	// closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// bucketSource reads the version index and the database objects straight out of
// the bucket that cmd/publish-database writes to.
type bucketSource struct {
	store dbstore.Store
	lg    *slog.Logger
}

// NewBucketSource returns a Source backed by an object store.
func NewBucketSource(store dbstore.Store, lg *slog.Logger) Source {
	return &bucketSource{store: store, lg: lg}
}

// Current returns the first entry in the version index.
//
// dbstore.InsertVersion prepends, so the index is ordered newest-first and the
// newest build is entry zero.
func (s *bucketSource) Current(ctx context.Context) (Build, error) {
	idx, err := dbstore.LoadIndex(ctx, s.store, s.lg)
	if err != nil {
		return Build{}, fmt.Errorf("can't read the database version index: %w", err)
	}

	versions := idx.GetVersions()
	if len(versions) == 0 {
		return Build{}, errors.New("the database version index is empty; nothing has been published yet")
	}

	newest := versions[0]
	id := newest.GetVersionId()
	if id == "" {
		return Build{}, errors.New("the newest database version has no version ID")
	}

	// AsTime() on a nil timestamp is the Unix epoch, not the zero time, so
	// check for the message before converting: callers read the zero time as
	// "the index didn't say" and fall back to the mmdb's own build epoch.
	var createdAt time.Time
	if ts := newest.GetCreatedAt(); ts != nil {
		createdAt = ts.AsTime()
	}

	return Build{
		VersionID: id,
		Key:       dbstore.ObjectKey(id),
		CreatedAt: createdAt,
	}, nil
}

// Open streams the compressed database object at key.
func (s *bucketSource) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("can't read %s from the bucket: %w", key, err)
	}
	return obj.Body, nil
}

// Interface guards
var (
	_ Source = (*bucketSource)(nil)
)
