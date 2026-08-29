package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
)

// Payloads are stored in packs: one object holding many payloads, each
// gzipped on its own (Gzip, at ingest), so the object store sees one PUT
// per PackBytes of payloads (PUT requests are what an S3 bill is made of)
// and a read is one ranged GET. Packs are rows of the packs table
// (store.SpoolPayloads places payloads in them) and the event row holds
// its payload's pack, offset and length from the start;
// retention.PackPayloads uploads a pack once it is closed.

// PackBytes is the size at which a pack closes.
const PackBytes = 8 << 20

// Gzip compresses one payload for the spool and the pack.
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

// PackKey is the object key of pack id.
func PackKey(id int64) string { return fmt.Sprintf("%s%d", PrefixEvents, id) }

// PackMember is one payload at its offset.
type PackMember struct {
	Off  int64
	Data []byte
}

// AssemblePack lays members (sorted by Off) out at their offsets; a gap —
// an offset handed out to an envelope that was rolled back — is
// zero-filled, so every Ref stays right.
func AssemblePack(members []PackMember) []byte {
	var buf bytes.Buffer
	for _, m := range members {
		if pad := m.Off - int64(buf.Len()); pad > 0 {
			buf.Write(make([]byte, pad))
		}
		buf.Write(m.Data)
	}
	return buf.Bytes()
}

// ReadPayload fetches one payload from its pack: a ranged GET, then
// gunzip. ErrNotFound when the pack is not (or no longer) there.
func ReadPayload(ctx context.Context, s Store, packID int64, off, n int64) ([]byte, error) {
	raw, err := s.GetRange(ctx, PackKey(packID), off, n)
	if err != nil {
		return nil, err
	}
	out, err := Gunzip(raw)
	if err != nil {
		return nil, fmt.Errorf("blob: payload %d@%d: %w", packID, off, err)
	}
	return out, nil
}
