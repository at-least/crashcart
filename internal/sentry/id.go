package sentry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// ID is a 128-bit identifier as 32 lowercase hex characters — the form
// Sentry uses for event ids and CrashCart for fingerprints. It is stored
// as a Postgres UUID (16 bytes in every key and index); the Go side keeps
// the hex text so URLs, JSON and the SDK protocol never see dashes.
type ID string

// ParseID accepts 32 hex characters, or a 36-character dashed UUID, in any
// case, and normalizes it.
func ParseID(s string) (ID, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 36 {
		s = strings.ReplaceAll(s, "-", "")
	}
	if len(s) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", false
	}
	return ID(s), true
}

// DerivedID is the ID for something that has no proper one: the first 16
// bytes of sha256(seed) — stable, so the same input maps to the same row.
func DerivedID(seed []byte) ID {
	sum := sha256.Sum256(seed)
	return ID(hex.EncodeToString(sum[:16]))
}

func (id ID) String() string { return string(id) }

// UnmarshalText accepts the dashed UUID form too (e.g. a uuid the database
// rendered as text in a JSON job argument) and normalizes it.
func (id *ID) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*id = ""
		return nil
	}
	v, ok := ParseID(string(b))
	if !ok {
		return fmt.Errorf("invalid id %q", string(b))
	}
	*id = v
	return nil
}

// Ptr is the nullable form: nil for the empty ID.
func (id ID) Ptr() *ID {
	if id == "" {
		return nil
	}
	return &id
}

// ScanUUID implements pgtype.UUIDScanner.
func (id *ID) ScanUUID(v pgtype.UUID) error {
	if !v.Valid {
		*id = ""
		return nil
	}
	*id = ID(hex.EncodeToString(v.Bytes[:]))
	return nil
}

// UUIDValue implements pgtype.UUIDValuer.
func (id ID) UUIDValue() (pgtype.UUID, error) {
	if id == "" {
		return pgtype.UUID{}, nil
	}
	b, err := hex.DecodeString(string(id))
	if err != nil || len(b) != 16 {
		return pgtype.UUID{}, fmt.Errorf("invalid id %q", string(id))
	}
	var u pgtype.UUID
	copy(u.Bytes[:], b)
	u.Valid = true
	return u, nil
}
