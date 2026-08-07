// Package dbcachetest provides an in-memory dbcache.Source and the compressed
// test databases to feed it.
package dbcachetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/TecharoHQ/reputationdb"
	"github.com/TecharoHQ/reputationdb/cmd/reputationdbd/internal/dbcache"
	"github.com/TecharoHQ/reputationdb/internal/dbstore"
	"github.com/klauspost/compress/zstd"
	"github.com/maxmind/mmdbwriter"
)

// Fake is an in-memory dbcache.Source. The zero value is not usable; call New.
type Fake struct {
	mu      sync.Mutex
	build   dbcache.Build
	objects map[string][]byte

	// currents and opens count the calls, so tests can assert that an
	// unchanged version did not trigger another download.
	currents int
	opens    int

	// CurrentErr and OpenErr make the corresponding method fail, for testing
	// error paths. Guard them with the same lock as everything else so a test
	// can flip them while the refresh goroutine is running.
	currentErr error
	openErr    error
}

// New returns a Fake with nothing published.
func New() *Fake {
	return &Fake{objects: map[string][]byte{}}
}

// Publish makes versionID the newest build, stored under the same bucket key
// production would use.
func (f *Fake) Publish(versionID string, createdAt time.Time, compressed []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := dbstore.ObjectKey(versionID)
	f.objects[key] = compressed
	f.build = dbcache.Build{VersionID: versionID, Key: key, CreatedAt: createdAt}
}

// SetCurrentErr makes Current fail with err, or succeed again when err is nil.
func (f *Fake) SetCurrentErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentErr = err
}

// SetOpenErr makes Open fail with err, or succeed again when err is nil.
func (f *Fake) SetOpenErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openErr = err
}

// Currents returns how many times Current has been called.
func (f *Fake) Currents() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currents
}

// Opens returns how many times Open has been called, which is how many times
// the database has been downloaded.
func (f *Fake) Opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func (f *Fake) Current(ctx context.Context) (dbcache.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.currents++
	if f.currentErr != nil {
		return dbcache.Build{}, f.currentErr
	}
	if f.build.VersionID == "" {
		return dbcache.Build{}, errors.New("dbcachetest: nothing published")
	}
	return f.build, nil
}

func (f *Fake) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, errors.New("dbcachetest: no object at " + key)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// CompressedDatabase returns a zstd-compressed reputation database containing
// exactly the given CIDRs, each recorded as a datacentre address.
//
// It produces the same record schema the real builds use, so a Result decoded
// from it exercises the same decoding path as production data.
func CompressedDatabase(cidrs ...string) ([]byte, error) {
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "Techaro-Veil-Datacenter",
		IPVersion:    6,
		RecordSize:   28,
	})
	if err != nil {
		return nil, err
	}

	for _, cidr := range cidrs {
		rec := reputationdb.Record{}
		rec.Add(reputationdb.ListMembership{
			Repository: "github.com/hexydec/ip-ranges",
			List:       "output/datacentres.txt",
			Provider:   "datacentres",
			Category:   reputationdb.CategoryByteDatacenter,
		})

		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		if err := tree.Insert(network, rec.DataType()); err != nil {
			return nil, err
		}
	}

	var raw bytes.Buffer
	if _, err := tree.WriteTo(&raw); err != nil {
		return nil, err
	}

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer enc.Close()

	return enc.EncodeAll(raw.Bytes(), nil), nil
}

// Interface guards
var (
	_ dbcache.Source = (*Fake)(nil)
)
