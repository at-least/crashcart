package store

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"
)

// A gzip compressor carries ~800 KB of state: Gzip runs once per stored
// event on the ingest path, so the writers (and readers) are pooled.
var (
	writers = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}
	readers = sync.Pool{New: func() any { return new(gzip.Reader) }}
)

// Gzip compresses data (the raw event payload, once at ingest).
func Gzip(data []byte) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(data)/3+64))
	zw := writers.Get().(*gzip.Writer)
	zw.Reset(buf)
	zw.Write(data)
	zw.Close()
	writers.Put(zw)
	return buf.Bytes()
}

// Gunzip decompresses what Gzip wrote.
func Gunzip(data []byte) ([]byte, error) {
	zr := readers.Get().(*gzip.Reader)
	defer readers.Put(zr)
	if err := zr.Reset(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}
