-- name: UpsertClientReportCounts :exec
-- One client_report item's discarded_events, added to their (reason,
-- category) bucket for that hour — not one row per report, a running sum
-- (project_usage's increment idiom, not the events/sessions rollup one:
-- there is no raw-row table under this to ever recompute from).
INSERT INTO client_report_counts (project_id, bucket, reason, category, quantity)
SELECT $1, $2, unnest(sqlc.arg(reasons)::text[]), unnest(sqlc.arg(categories)::text[]), unnest(sqlc.arg(quantities)::bigint[])
ON CONFLICT (project_id, bucket, reason, category) DO UPDATE SET
    quantity = client_report_counts.quantity + EXCLUDED.quantity;

-- name: ListClientReportCounts :many
-- Summed by reason+category over a window: the Settings page panel and
-- its API endpoint. Largest first (what's actually worth an operator's
-- attention).
SELECT reason, category, SUM(quantity)::bigint AS quantity
FROM client_report_counts
WHERE project_id = $1 AND bucket >= $2 AND bucket < $3
GROUP BY reason, category
ORDER BY quantity DESC, reason, category;
