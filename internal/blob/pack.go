package blob

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Payloads are stored in packs: one object holding many payloads, each
// gzipped on its own (Gzip, at ingest), so the object store sees one PUT
// per PackBytes of payloads (PUT requests are what an S3 bill is made of)
// and a read is one ranged GET. Each process fills a pack of its own
// (store.PackAllocator) and the event row holds the Ref of its payload
// from the start; retention.PackPayloads uploads a pack once it is
// closed.

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

// Ref locates one payload inside a pack: "<key>#<offset>#<length>".
type Ref string

// NewRef builds a Ref.
func NewRef(key string, off, n int64) Ref { return Ref(fmt.Sprintf("%s#%d#%d", key, off, n)) }

// ParseRef splits a Ref.
func ParseRef(r string) (key string, off, n int64, ok bool) {
	i := strings.LastIndexByte(r, '#')
	if i < 0 {
		return "", 0, 0, false
	}
	j := strings.LastIndexByte(r[:i], '#')
	if j < 0 {
		return "", 0, 0, false
	}
	off, err1 := strconv.ParseInt(r[j+1:i], 10, 64)
	n, err2 := strconv.ParseInt(r[i+1:], 10, 64)
	if err1 != nil || err2 != nil || off < 0 || n <= 0 || r[:j] == "" {
		return "", 0, 0, false
	}
	return r[:j], off, n, true
}

// PackKey names a pack by the UTC day it was opened (so the events/
// lifecycle rule applies) and a random id (so processes never collide).
func PackKey(now time.Time) string {
	b := make([]byte, 12)
	rand.Read(b)
	return fmt.Sprintf("%s%s/%s", PrefixEvents, now.UTC().Format("2006-01-02"), hex.EncodeToString(b))
}

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

// ReadRef fetches one payload by its Ref: a ranged GET of the pack, then
// gunzip. ErrNotFound when the pack is not (or no longer) there.
func ReadRef(ctx context.Context, s Store, ref string) ([]byte, error) {
	key, off, n, ok := ParseRef(ref)
	if !ok {
		return nil, fmt.Errorf("blob: bad payload ref %q", ref)
	}
	raw, err := s.GetRange(ctx, key, off, n)
	if err != nil {
		return nil, err
	}
	out, err := Gunzip(raw)
	if err != nil {
		return nil, fmt.Errorf("blob: payload %s: %w", ref, err)
	}
	return out, nil
}
