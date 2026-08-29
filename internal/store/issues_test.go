package store

import (
	"testing"
	"time"
)

func TestIssueFilterWhere(t *testing.T) {
	f := IssueFilter{ProjectID: 1, Status: "resolved", Level: "fatal", Release: "1.0", Query: "50%", From: time.Unix(10, 0), To: time.Unix(20, 0)}
	where, args := f.where()
	want := "project_id = $1 AND status = $2 AND level = $3 AND (first_release = $4 OR last_release = $4) AND (title ILIKE $5 OR error_type ILIKE $5) AND last_seen >= $6 AND last_seen < $7"
	if where != want {
		t.Errorf("where = %q", where)
	}
	if len(args) != 7 || args[4] != `%50\%%` {
		t.Errorf("args = %v", args)
	}
	if _, ok := issueSorts["drop table"]; ok {
		t.Error("sort allowlist leaked")
	}
}
