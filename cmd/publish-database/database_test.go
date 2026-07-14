package main

import (
	"crypto/sha512"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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
			got := versionID(tt.raw)

			sum := sha512.Sum512(tt.raw)
			want := base64.RawURLEncoding.EncodeToString(sum[:])
			if got != want {
				t.Errorf("versionID() = %q, want %q", got, want)
			}

			// URL-safe and unpadded: no +, /, or = may appear.
			if strings.ContainsAny(got, "+/=") {
				t.Errorf("versionID() = %q, want no +, / or = characters", got)
			}
			if len(got) != 86 {
				t.Errorf("len(versionID()) = %d, want 86", len(got))
			}
		})
	}
}

func TestVersionIDDiffers(t *testing.T) {
	if versionID([]byte("a")) == versionID([]byte("b")) {
		t.Error("versionID() returned the same ID for different contents")
	}
}

func TestObjectKey(t *testing.T) {
	id := versionID([]byte("hello"))
	got := objectKey(id)
	want := "databases/" + id + ".mmdb.zst"
	if got != want {
		t.Errorf("objectKey() = %q, want %q", got, want)
	}
}

func TestCompressDatabase(t *testing.T) {
	// Repetitive input so the compressed form is meaningfully smaller.
	raw := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1000))

	compressed, err := compressDatabase(raw)
	if err != nil {
		t.Fatalf("compressDatabase() error = %v", err)
	}

	if len(compressed) >= len(raw) {
		t.Errorf("compressDatabase() produced %d bytes from %d, want smaller", len(compressed), len(raw))
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader() error = %v", err)
	}
	defer dec.Close()

	got, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll() error = %v", err)
	}
	if string(got) != string(raw) {
		t.Error("round-trip through compressDatabase() did not preserve contents")
	}
}

func TestCompressDatabaseEmpty(t *testing.T) {
	compressed, err := compressDatabase([]byte{})
	if err != nil {
		t.Fatalf("compressDatabase() error = %v", err)
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader() error = %v", err)
	}
	defer dec.Close()

	got, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DecodeAll() = %d bytes, want 0", len(got))
	}
}
