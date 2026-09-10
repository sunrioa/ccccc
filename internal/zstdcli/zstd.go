// Package zstdcli retains CCT's interface but uses an embedded Go codec in
// Session Relay. No zstd executable or shell is needed on the destination.
package zstdcli

import (
	"bytes"
	"fmt"
	"github.com/klauspost/compress/zstd"
	"io"
	"os"
)

const DefaultHeadBytes = 1 << 20
const MaxDecompressedBytes = 128 << 20

func Available() bool { return true }
func decoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxDecompressedBytes), zstd.WithDecoderMaxWindow(64<<20))
}
func DecompressHead(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultHeadBytes
	}
	if maxBytes > MaxDecompressedBytes {
		return nil, fmt.Errorf("head exceeds limit")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	d, e := decoder(f)
	if e != nil {
		return nil, e
	}
	defer d.Close()
	b, e := io.ReadAll(io.LimitReader(d, maxBytes))
	if e != nil {
		return nil, e
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty zstd stream")
	}
	return b, nil
}
func Decompress(b []byte) ([]byte, error) {
	d, e := decoder(bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	defer d.Close()
	out, e := io.ReadAll(io.LimitReader(d, MaxDecompressedBytes+1))
	if e != nil {
		return nil, e
	}
	if len(out) > MaxDecompressedBytes {
		return nil, fmt.Errorf("zstd decompression exceeds %d bytes", MaxDecompressedBytes)
	}
	return out, nil
}
func Compress(b []byte) ([]byte, error) {
	if len(b) > MaxDecompressedBytes {
		return nil, fmt.Errorf("input exceeds limit")
	}
	w, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if e != nil {
		return nil, e
	}
	defer w.Close()
	return w.EncodeAll(b, nil), nil
}
