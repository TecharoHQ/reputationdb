package dbstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"time"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// Store is the slice of simplestorage.Client this repository needs, so that
// tests can substitute an in-memory fake. *simplestorage.Client satisfies it.
type Store interface {
	Get(ctx context.Context, key string, opts ...simplestorage.ClientOption) (*simplestorage.Object, error)
	Put(ctx context.Context, obj *simplestorage.Object, opts ...simplestorage.ClientOption) (*simplestorage.Object, error)
	List(ctx context.Context, opts ...simplestorage.ListOption) iter.Seq2[*simplestorage.Object, error]
	PresignURL(ctx context.Context, method string, key string, expiry time.Duration, opts ...simplestorage.ClientOption) (string, error)
}

// readSeekNopCloser adapts a *bytes.Reader into an io.ReadSeekCloser.
//
// The AWS SDK's retry middleware rewinds the request body to retry a failed
// upload, which it can only do if the body is an io.Seeker. io.NopCloser hides
// the *bytes.Reader's Seek method, leaving the ~314MB database upload with no
// retry tolerance at all.
type readSeekNopCloser struct{ *bytes.Reader }

func (readSeekNopCloser) Close() error { return nil }

// Exists reports whether an object is present at key.
//
// This is a List rather than a Get or Head because Client.Get and Client.Head
// wrap their errors with %v, not %w, severing the error chain so that a
// NoSuchKey cannot be told apart from a network failure with errors.As. List
// yields the underlying error untouched. The distinction matters: for the
// index, "missing" means "first publish, start empty" while a failed call must
// abort — treating the latter as the former would silently discard every
// recorded version.
func Exists(ctx context.Context, store Store, key string) (bool, error) {
	for obj, err := range store.List(ctx, simplestorage.WithPrefix(key)) {
		if err != nil {
			return false, fmt.Errorf("listing %s: %w", key, err)
		}
		// The prefix is a server-side optimization only; a fake store cannot
		// honor it (ListOption writes to an unexported struct), so match exactly.
		if obj.Key == key {
			return true, nil
		}
	}
	return false, nil
}

// LoadIndex fetches and decodes the version index, returning an empty index if
// it does not exist yet.
func LoadIndex(ctx context.Context, store Store, lg *slog.Logger) (*fetchv1.ListResponse, error) {
	exists, err := Exists(ctx, store, IndexKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		lg.Info("no version index found; starting a new one", "key", IndexKey)
		return &fetchv1.ListResponse{}, nil
	}

	obj, err := store.Get(ctx, IndexKey)
	if err != nil {
		return nil, fmt.Errorf("getting %s: %w", IndexKey, err)
	}
	defer obj.Body.Close()

	gzipped, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", IndexKey, err)
	}

	idx, err := DecodeIndex(gzipped)
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", IndexKey, err)
	}

	return idx, nil
}

// SaveIndex encodes and uploads the version index, overwriting the existing one.
func SaveIndex(ctx context.Context, store Store, idx *fetchv1.ListResponse) error {
	body, err := EncodeIndex(idx)
	if err != nil {
		return err
	}

	if _, err := store.Put(ctx, &simplestorage.Object{
		Key:         IndexKey,
		ContentType: "application/octet-stream",
		Size:        int64(len(body)),
		Body:        readSeekNopCloser{bytes.NewReader(body)},
	}, simplestorage.WithAccessType(simplestorage.AccessPrivate)); err != nil {
		return fmt.Errorf("putting %s: %w", IndexKey, err)
	}

	return nil
}

// PutDatabase uploads a compressed database as a private object.
func PutDatabase(ctx context.Context, store Store, key string, body []byte) error {
	if _, err := store.Put(ctx, &simplestorage.Object{
		Key:         key,
		ContentType: "application/zstd",
		Size:        int64(len(body)),
		Body:        readSeekNopCloser{bytes.NewReader(body)},
	}, simplestorage.WithAccessType(simplestorage.AccessPrivate)); err != nil {
		return fmt.Errorf("putting %s: %w", key, err)
	}

	return nil
}

// PresignGet returns a time-limited download URL for a bucket key.
//
// Database objects are uploaded private, so this is the only way a client can
// read one. Presigning does not check that the object exists; callers that care
// must ask Exists first.
func PresignGet(ctx context.Context, store Store, key string, expiry time.Duration) (string, error) {
	url, err := store.PresignURL(ctx, http.MethodGet, key, expiry)
	if err != nil {
		return "", fmt.Errorf("presigning a GET for %s: %w", key, err)
	}
	return url, nil
}
