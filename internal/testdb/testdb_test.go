package testdb

import (
	"context"
	"testing"
)

func TestFreshSchema(t *testing.T) {
	st := New(t)
	var n int
	if err := st.Pool.QueryRow(context.Background(), "SELECT count(*) FROM projects").Scan(&n); err != nil || n != 0 {
		t.Fatalf("projects: %d %v", n, err)
	}
	if _, err := st.Pool.Exec(context.Background(), "SELECT * FROM event_stats_hourly"); err != nil {
		t.Fatal(err)
	}
}
