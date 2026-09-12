package rehydrate

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"time"
)

// gzipCompress gzips raw with a deterministic header: a zero
// modification time and a fixed OS byte, so identical content always
// gzips to identical bytes. The budget pass and the bundle builder
// both call this, so a budget decision measured against one gzip
// output matches the bytes the bundle later ships.
func gzipCompress(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("create gzip writer: %w", err)
	}
	w.ModTime = time.Time{}
	if _, err := w.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip content: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}
