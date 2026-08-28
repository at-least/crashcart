package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/newlix/crashcart/internal/db"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/testdb"
)

func TestPartitions(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -40)

	created, err := db.EnsurePartitions(ctx, pool, old, old)
	if err != nil || len(created) != 1 {
		t.Fatalf("ensure: %v %v", created, err)
	}
	if again, _ := db.EnsurePartitions(ctx, pool, old, old); len(again) != 0 {
		t.Error("ensure must be idempotent")
	}
	insert := func(ts time.Time) {
		_, err := pool.Exec(ctx, `INSERT INTO events (id, level, message, payload) VALUES ($1, 'info', 'x', '{}')`, pk.New(ts, func() int64 { return 1 }))
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(old)                     // → events_pYYYYMMDD (old day)
	insert(now)                     // → today's partition (testdb ensured it)
	insert(now.AddDate(0, 0, -100)) // → events_default (no partition)

	var inDefault, inOld int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM events_default").Scan(&inDefault)
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM "+created[0]).Scan(&inOld)
	if inDefault != 1 || inOld != 1 {
		t.Fatalf("routing: default=%d old=%d", inDefault, inOld)
	}

	dropped, err := db.DropPartitionsBefore(ctx, pool, now.AddDate(0, 0, -30))
	if err != nil || len(dropped) != 1 || dropped[0] != created[0] {
		t.Fatalf("drop: %v %v", dropped, err)
	}
	var total int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&total)
	if total != 2 {
		t.Errorf("after drop: %d rows (today + default stray expected)", total)
	}
	// export still reads the whole table transparently
	var todayN int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE id >= $1", pk.Lower(now.Add(-time.Hour))).Scan(&todayN)
	if todayN != 1 {
		t.Errorf("range over partitions: %d", todayN)
	}
}
