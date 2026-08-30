package sentry

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestID(t *testing.T) {
	const hex = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	for _, in := range []string{hex, " A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4 ", "a1b2c3d4-e5f6-a1b2-c3d4-e5f6a1b2c3d4", "A1B2C3D4-E5F6-A1B2-C3D4-E5F6A1B2C3D4"} {
		if id, ok := ParseID(in); !ok || id != hex {
			t.Errorf("ParseID(%q) = %q %v", in, id, ok)
		}
	}
	for _, in := range []string{"", "a1b2", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4ff", "a1b2c3d4-e5f6-a1b2-c3d4-e5f6a1b2c3"} {
		if id, ok := ParseID(in); ok {
			t.Errorf("ParseID(%q) accepted as %q", in, id)
		}
	}
	if ID(hex).String() != hex || ID("").Ptr() != nil || *ID(hex).Ptr() != hex {
		t.Error("String / Ptr")
	}
	if DerivedID([]byte("x")) != DerivedID([]byte("x")) || DerivedID([]byte("x")) == DerivedID([]byte("y")) || len(DerivedID([]byte("x"))) != 32 {
		t.Error("DerivedID must be stable and 32 hex chars")
	}

	// JSON / text: the dashed form (a uuid rendered by the database) is normalized; garbage is rejected.
	var v struct{ ID ID }
	if err := json.Unmarshal([]byte(`{"ID":"A1B2C3D4-E5F6-A1B2-C3D4-E5F6A1B2C3D4"}`), &v); err != nil || v.ID != hex {
		t.Errorf("unmarshal dashed: %q %v", v.ID, err)
	}
	if err := json.Unmarshal([]byte(`{"ID":""}`), &v); err != nil || v.ID != "" {
		t.Errorf("unmarshal empty: %q %v", v.ID, err)
	}
	if err := json.Unmarshal([]byte(`{"ID":"nope"}`), &v); err == nil {
		t.Error("unmarshal garbage accepted")
	}

	// Postgres UUID round trip; the empty ID is NULL and NULL scans to "".
	u, err := ID(hex).UUIDValue()
	if err != nil || !u.Valid {
		t.Fatalf("UUIDValue: %+v %v", u, err)
	}
	var back ID
	if err := back.ScanUUID(u); err != nil || back != hex {
		t.Errorf("ScanUUID = %q %v", back, err)
	}
	if u, err := ID("").UUIDValue(); err != nil || u.Valid {
		t.Errorf("empty UUIDValue = %+v %v", u, err)
	}
	if _, err := ID("xyz").UUIDValue(); err == nil {
		t.Error("invalid UUIDValue accepted")
	}
	back = hex
	if err := back.ScanUUID(pgtype.UUID{}); err != nil || back != "" {
		t.Errorf("NULL scan = %q %v", back, err)
	}
}
