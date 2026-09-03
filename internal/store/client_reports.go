package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpsertClientReportCounts adds one client_report item's discarded_events
// to their (reason, category) bucket for that hour — not one row per
// report, a running sum kept by an ON CONFLICT increment (not the
// events/sessions rollup idiom: there is no raw-row table under this to
// ever recompute from).
func UpsertClientReportCounts(ctx context.Context, db DB, projectID int64, bucket time.Time, reasons, categories []string, quantities []int64) error {
	_, err := db.Exec(ctx, `INSERT INTO client_report_counts (project_id, bucket, reason, category, quantity)
		SELECT $1, $2, unnest($3::text[]), unnest($4::text[]), unnest($5::bigint[])
		ON CONFLICT (project_id, bucket, reason, category) DO UPDATE SET
		    quantity = client_report_counts.quantity + EXCLUDED.quantity`,
		projectID, bucket, reasons, categories, quantities)
	return err
}

// ClientReportCount is a (reason, category) sum over a window.
type ClientReportCount struct {
	Reason   string `json:"reason"`
	Category string `json:"category"`
	Quantity int64  `json:"quantity"`
}

// ListClientReportCounts: summed by reason+category over a window: the
// Settings page panel and its API endpoint. Largest first (what's
// actually worth an operator's attention).
func ListClientReportCounts(ctx context.Context, db DB, projectID int64, from, to time.Time) ([]ClientReportCount, error) {
	rows, err := db.Query(ctx, `SELECT reason, category, SUM(quantity)::bigint AS quantity
		FROM client_report_counts
		WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
		GROUP BY reason, category
		ORDER BY quantity DESC, reason, category`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ClientReportCount])
}
