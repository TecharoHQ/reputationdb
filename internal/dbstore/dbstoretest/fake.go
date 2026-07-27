// Package dbstoretest provides an in-memory dbstore.Store for tests.
package dbstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"

	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
)

// Fake is an in-memory dbstore.Store.
//
// It ignores every ClientOption and ListOption it is handed: those types mutate
// unexported structs in simplestorage, so a fake outside that package cannot
// read them back. Production code must therefore never depend on the server
// honoring a list prefix for correctness.
type Fake struct {
	Objects map[string][]byte

	// Setting any of these makes the corresponding method fail, for testing
	// error paths.
	PutErr     error
	GetErr     error
	ListErr    error
	PresignErr error

	Puts     []string // keys passed to Put, in order
	Gets     []string // keys passed to Get, in order
	Presigns []string // keys passed to PresignURL, in order
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{Objects: map[string][]byte{}}
}

func (f *Fake) Get(ctx context.Context, key string, opts ...simplestorage.ClientOption) (*simplestorage.Object, error) {
	f.Gets = append(f.Gets, key)
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	body, ok := f.Objects[key]
	if !ok {
		return nil, errors.New("simplestorage: can't get bucket/" + key + ": NoSuchKey")
	}
	return &simplestorage.Object{
		Key:  key,
		Size: int64(len(body)),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func (f *Fake) Put(ctx context.Context, obj *simplestorage.Object, opts ...simplestorage.ClientOption) (*simplestorage.Object, error) {
	if f.PutErr != nil {
		return nil, f.PutErr
	}
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, err
	}
	f.Objects[obj.Key] = body
	f.Puts = append(f.Puts, obj.Key)
	return obj, nil
}

func (f *Fake) List(ctx context.Context, opts ...simplestorage.ListOption) iter.Seq2[*simplestorage.Object, error] {
	return func(yield func(*simplestorage.Object, error) bool) {
		if f.ListErr != nil {
			yield(nil, f.ListErr)
			return
		}
		for key, body := range f.Objects {
			if !yield(&simplestorage.Object{Key: key, Size: int64(len(body))}, nil) {
				return
			}
		}
	}
}

// PresignURL returns a deterministic fake URL. Like the real S3 presigner it
// does not check that the object exists, so code that needs that guarantee is
// forced to ask for it explicitly and can be tested for doing so.
func (f *Fake) PresignURL(ctx context.Context, method string, key string, expiry time.Duration, opts ...simplestorage.ClientOption) (string, error) {
	f.Presigns = append(f.Presigns, key)
	if f.PresignErr != nil {
		return "", f.PresignErr
	}
	return fmt.Sprintf("https://fake.invalid/%s?method=%s&expiry=%s", key, method, expiry), nil
}
