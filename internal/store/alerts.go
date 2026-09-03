package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type AlertRule struct {
	ProjectID       int64      `json:"project_id"`
	Type            AlertType  `json:"type"`
	Enabled         bool       `json:"enabled"`
	CooldownMinutes int32      `json:"cooldown_minutes"`
	LastTriggered   *time.Time `json:"last_triggered"`
}

const alertRuleColumns = "project_id, type, enabled, cooldown_minutes, last_triggered"

func scanAlertRule(row pgx.Row) (AlertRule, error) {
	var r AlertRule
	err := row.Scan(&r.ProjectID, &r.Type, &r.Enabled, &r.CooldownMinutes, &r.LastTriggered)
	return r, err
}

func GetAlertRule(ctx context.Context, db DB, projectID int64, typ AlertType) (AlertRule, error) {
	return scanAlertRule(db.QueryRow(ctx, "SELECT "+alertRuleColumns+" FROM alert_rules WHERE project_id = $1 AND type = $2", projectID, typ))
}

func ListAlertRules(ctx context.Context, db DB, projectID int64) ([]AlertRule, error) {
	rows, err := db.Query(ctx, "SELECT "+alertRuleColumns+" FROM alert_rules WHERE project_id = $1 ORDER BY type", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []AlertRule{}
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, rows.Err()
}

func UpsertAlertRule(ctx context.Context, db DB, projectID int64, typ AlertType, enabled bool, cooldownMinutes int32) (AlertRule, error) {
	return scanAlertRule(db.QueryRow(ctx, `INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, type) DO UPDATE SET enabled = EXCLUDED.enabled, cooldown_minutes = EXCLUDED.cooldown_minutes
		RETURNING `+alertRuleColumns, projectID, typ, enabled, cooldownMinutes))
}

// EnsureAlertRules: the six default rules (enabled, default cooldown);
// existing rows untouched.
func EnsureAlertRules(ctx context.Context, db DB, projectID int64, cooldownMinutes int32) error {
	_, err := db.Exec(ctx, `INSERT INTO alert_rules (project_id, type, enabled, cooldown_minutes)
		SELECT $1::bigint, t, true, $2::int
		FROM unnest(ARRAY['new_issue', 'regression', 'unhandled_spike', 'escalating', 'monitor_failed', 'monitor_recovered']::alert_type[]) AS t
		ON CONFLICT (project_id, type) DO NOTHING`, projectID, cooldownMinutes)
	return err
}

// ClaimAlertRule claims the cooldown atomically (no row: disabled,
// cooling down, or no rule) and returns the previous last_triggered, for
// UnclaimAlertRule when nothing could be delivered. The self-join reads
// the row before the update.
func ClaimAlertRule(ctx context.Context, db DB, projectID int64, typ AlertType) (*time.Time, error) {
	var previous *time.Time
	err := db.QueryRow(ctx, `UPDATE alert_rules a SET last_triggered = now()
		FROM alert_rules old
		WHERE old.project_id = a.project_id AND old.type = a.type
		  AND a.project_id = $1 AND a.type = $2 AND a.enabled
		  AND (a.last_triggered IS NULL OR a.last_triggered < now() - make_interval(mins => a.cooldown_minutes))
		RETURNING old.last_triggered AS previous`, projectID, typ).Scan(&previous)
	return previous, err
}

// UnclaimAlertRule gives a claim back (nothing was delivered): the
// cooldown must not eat the next alert too.
func UnclaimAlertRule(ctx context.Context, db DB, projectID int64, typ AlertType, previous *time.Time) error {
	_, err := db.Exec(ctx, "UPDATE alert_rules SET last_triggered = $3 WHERE project_id = $1 AND type = $2", projectID, typ, previous)
	return err
}

type AlertChannel struct {
	ID        int64           `json:"id"`
	ProjectID int64           `json:"project_id"`
	Kind      ChannelKind     `json:"kind"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
}

const alertChannelColumns = "id, project_id, kind, config, created_at"

func scanAlertChannel(row pgx.Row) (AlertChannel, error) {
	var c AlertChannel
	err := row.Scan(&c.ID, &c.ProjectID, &c.Kind, &c.Config, &c.CreatedAt)
	return c, err
}

func ListAlertChannels(ctx context.Context, db DB, projectID int64) ([]AlertChannel, error) {
	rows, err := db.Query(ctx, "SELECT "+alertChannelColumns+" FROM alert_channels WHERE project_id = $1 ORDER BY id", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []AlertChannel{}
	for rows.Next() {
		c, err := scanAlertChannel(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func CreateAlertChannel(ctx context.Context, db DB, projectID int64, kind ChannelKind, config json.RawMessage) (AlertChannel, error) {
	return scanAlertChannel(db.QueryRow(ctx, "INSERT INTO alert_channels (project_id, kind, config) VALUES ($1, $2, $3) RETURNING "+alertChannelColumns,
		projectID, kind, config))
}

func DeleteAlertChannel(ctx context.Context, db DB, projectID, id int64) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM alert_channels WHERE project_id = $1 AND id = $2", projectID, id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
