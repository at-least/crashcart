package store

import (
	"bytes"
	"compress/gzip"
)

// Payloads are stored gzipped (events.payload): compressed once at ingest,
// decoded on the few reads (the event page, the JSON event endpoint, the
// symbolication job, export).

// Gzip compresses one payload for storage.
func Gzip(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

// Gunzip is the inverse.
func Gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
