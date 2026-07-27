package main

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

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
