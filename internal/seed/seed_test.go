package seed

import (
	"context"
	"log/slog"
	"testing"

	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/ingest"
	"github.com/crashcartapp/crashcart/internal/retention"
	"github.com/crashcartapp/crashcart/internal/testdb"
)

func TestRun(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	in := &ingest.Ingester{Store: st, Cfg: config.Config{}, Log: slog.Default()}
	if err := Run(ctx, in, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := retention.RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProject(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var issues, events, sessions, hourly, rules, shots int
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
	q("SELECT count(*) FROM attachments a JOIN events e USING (project_id, event_id, occurred_at) WHERE a.project_id = $1 AND a.content_type = 'image/png' AND e.handled = false", &shots, p.ID)
	if issues < 6 {
		t.Errorf("issues = %d, want >= 6", issues)
	}
	if events < 1500 || events > 3000 {
		t.Errorf("events = %d, want ~2000", events)
	}
	if sessions == 0 {
		t.Error("no sessions")
	}
	if shots < 20 {
		t.Errorf("screenshots on unhandled events = %d, want >= 20", shots)
	}
	if hourly == 0 {
		t.Error("event_stats_hourly empty")
	}
	if rules != 4 {
		t.Errorf("alert rules = %d, want 4", rules)
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
	if err := retention.RollupAll(ctx, st, config.Config{}); err != nil {
		t.Fatal(err)
	}
}
