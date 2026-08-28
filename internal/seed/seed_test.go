package seed

import (
	"context"
	"log/slog"
	"testing"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/ingest"
	"github.com/newlix/crashcart/internal/testdb"
)

func TestRun(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	if err := Run(ctx, in, "demo"); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProject(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var issues, events, sessions, hourly, rules int
	q := func(sql string, dst *int, args ...any) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q("SELECT count(*) FROM issues WHERE project_id = $1", &issues, p.ID)
	q("SELECT count(*) FROM events WHERE project_id = $1", &events, p.ID)
	q("SELECT count(*) FROM sessions WHERE project_id = $1", &sessions, p.ID)
	q("SELECT count(*) FROM event_stats_hourly WHERE project_id = $1", &hourly, p.ID)
	q("SELECT count(*) FROM alert_rules WHERE project_id = $1 AND enabled", &rules, p.ID)
	if issues < 6 {
		t.Errorf("issues = %d, want >= 6", issues)
	}
	if events < 1500 || events > 3000 {
		t.Errorf("events = %d, want ~2000", events)
	}
	if sessions == 0 {
		t.Error("no sessions")
	}
	if hourly == 0 {
		t.Error("event_stats_hourly empty")
	}
	if rules != 3 {
		t.Errorf("alert rules = %d, want 3", rules)
	}
	var releases int
	q("SELECT count(DISTINCT release) FROM events WHERE project_id = $1", &releases, p.ID)
	if releases != 3 {
		t.Errorf("releases = %d, want 3", releases)
	}
	// Re-running seeds more data but must not fail (project exists).
	if err := Run(ctx, in, "demo"); err != nil {
		t.Fatal(err)
	}
}
