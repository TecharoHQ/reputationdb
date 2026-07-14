package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// objectStore is the slice of simplestorage.Client this command needs, so that
// tests can substitute an in-memory fake. *simplestorage.Client satisfies it.
type objectStore interface {
	Get(ctx context.Context, key string, opts ...simplestorage.ClientOption) (*simplestorage.Object, error)
	Put(ctx context.Context, obj *simplestorage.Object, opts ...simplestorage.ClientOption) (*simplestorage.Object, error)
	List(ctx context.Context, opts ...simplestorage.ListOption) iter.Seq2[*simplestorage.Object, error]
}

// indexExists reports whether the index object is present.
//
// This is a List rather than a Get or Head because simplestorage wraps those
// two calls' errors with %v (client.go:288 and :323), severing the error chain
// so that a NoSuchKey cannot be told apart from a network failure with
// errors.As. List yields the underlying error untouched. The distinction
// matters: a missing index means "first publish, start empty", while a failed
// call must abort — treating the latter as the former would silently discard
// every recorded version.
func indexExists(ctx context.Context, store objectStore) (bool, error) {
	for obj, err := range store.List(ctx, simplestorage.WithPrefix(indexKey)) {
		if err != nil {
			return false, fmt.Errorf("listing %s: %w", indexKey, err)
		}
		// The prefix is a server-side optimization only; a fake store cannot
		// honor it (ListOption writes to an unexported struct), so match exactly.
		if obj.Key == indexKey {
			return true, nil
		}
	}
	return false, nil
}

// loadIndex fetches and decodes the version index, returning an empty index if
// it does not exist yet.
func loadIndex(ctx context.Context, store objectStore, lg *slog.Logger) (*fetchv1.ListResponse, error) {
	exists, err := indexExists(ctx, store)
	if err != nil {
		return nil, err
	}
	if !exists {
		lg.Info("no version index found; starting a new one", "key", indexKey)
		return &fetchv1.ListResponse{}, nil
	}

	obj, err := store.Get(ctx, indexKey)
	if err != nil {
		return nil, fmt.Errorf("getting %s: %w", indexKey, err)
	}
	defer obj.Body.Close()

	gzipped, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", indexKey, err)
	}

	idx, err := decodeIndex(gzipped)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", indexKey, err)
	}

	return idx, nil
}

// saveIndex encodes and uploads the version index, overwriting the existing one.
func saveIndex(ctx context.Context, store objectStore, idx *fetchv1.ListResponse) error {
	body, err := encodeIndex(idx)
	if err != nil {
		return err
	}

	if _, err := store.Put(ctx, &simplestorage.Object{
		Key:         indexKey,
		ContentType: "application/octet-stream",
		Size:        int64(len(body)),
		Body:        io.NopCloser(bytes.NewReader(body)),
	}, simplestorage.WithAccessType(simplestorage.AccessPrivate)); err != nil {
		return fmt.Errorf("putting %s: %w", indexKey, err)
	}

	return nil
}

// putDatabase uploads a compressed database as a private object.
func putDatabase(ctx context.Context, store objectStore, key string, body []byte) error {
	if _, err := store.Put(ctx, &simplestorage.Object{
		Key:         key,
		ContentType: "application/zstd",
		Size:        int64(len(body)),
		Body:        io.NopCloser(bytes.NewReader(body)),
	}, simplestorage.WithAccessType(simplestorage.AccessPrivate)); err != nil {
		return fmt.Errorf("putting %s: %w", key, err)
	}

	return nil
}
