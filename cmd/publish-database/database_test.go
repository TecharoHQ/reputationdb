package main

import (
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

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
