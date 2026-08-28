package api

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"
)

// decompress handles the encodings Sentry SDKs use for envelopes.
func decompress(encoding string, body io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		return gzip.NewReader(body)
	case "deflate":
		return zlib.NewReader(body)
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", encoding)
	}
}
