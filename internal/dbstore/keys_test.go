package dbstore_test

import (
	"crypto/sha512"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/TecharoHQ/reputationdb/internal/dbstore"
)

func TestVersionID(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello")},
		{"binary", []byte{0x00, 0xff, 0xfe, 0x01}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := dbstore.VersionID(tt.raw)

			sum := sha512.Sum512(tt.raw)
			want := base64.RawURLEncoding.EncodeToString(sum[:])
			if got != want {
				t.Errorf("VersionID() = %q, want %q", got, want)
			}

			// URL-safe and unpadded: no +, /, or = may appear.
			if strings.ContainsAny(got, "+/=") {
				t.Errorf("VersionID() = %q, want no +, / or = characters", got)
			}
			if len(got) != dbstore.VersionIDLength {
				t.Errorf("len(VersionID()) = %d, want %d", len(got), dbstore.VersionIDLength)
			}
		})
	}
}

func TestVersionIDDiffers(t *testing.T) {
	if dbstore.VersionID([]byte("a")) == dbstore.VersionID([]byte("b")) {
		t.Error("VersionID() returned the same ID for different contents")
	}
}

func TestObjectKey(t *testing.T) {
	id := dbstore.VersionID([]byte("hello"))
	got := dbstore.ObjectKey(id)
	want := "databases/" + id + ".mmdb.zst"
	if got != want {
		t.Errorf("ObjectKey() = %q, want %q", got, want)
	}
}
