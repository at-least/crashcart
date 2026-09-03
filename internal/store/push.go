package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// PushDevice is a mobile companion app instance registered by an API key.
type PushDevice struct {
	ID        int64     `json:"id"`
	APIKeyID  int64     `json:"-"`
	Token     string    `json:"-"`
	Platform  string    `json:"platform"`
	CreatedAt time.Time `json:"created_at"`
}

const pushDeviceColumns = "id, api_key_id, token, platform, created_at"

// UpsertPushDevice registers or refreshes a device's push token for an API
// key. Keyed by token (unique): a reinstall or token rotation updates the
// existing row instead of creating a duplicate that would double-send.
func UpsertPushDevice(ctx context.Context, db DB, apiKeyID int64, token, platform string) (PushDevice, error) {
	return scanOne[PushDevice](db.Query(ctx, `INSERT INTO push_devices (api_key_id, token, platform) VALUES ($1, $2, $3)
		ON CONFLICT (token) DO UPDATE SET api_key_id = EXCLUDED.api_key_id, platform = EXCLUDED.platform
		RETURNING `+pushDeviceColumns, apiKeyID, token, platform))
}

// DeletePushDevice removes a device (and its subscriptions, via cascade);
// scoped to the owning key so one key cannot delete another's device.
func DeletePushDevice(ctx context.Context, db DB, apiKeyID, id int64) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM push_devices WHERE api_key_id = $1 AND id = $2", apiKeyID, id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SubscribePush subscribes a device to a project's alerts; ok is false
// when the device does not exist or is not owned by apiKeyID (a 404, not
// an error, at the call site).
func SubscribePush(ctx context.Context, db DB, apiKeyID, deviceID, projectID int64) (ok bool, err error) {
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM push_devices WHERE id = $1 AND api_key_id = $2)", deviceID, apiKeyID).Scan(&ok); err != nil || !ok {
		return ok, err
	}
	_, err = db.Exec(ctx, "INSERT INTO push_subscriptions (device_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", deviceID, projectID)
	return true, err
}

// UnsubscribePush removes a device's subscription to a project; the join
// on push_devices keeps this scoped to the owning key the same way
// SubscribePush is.
func UnsubscribePush(ctx context.Context, db DB, apiKeyID, deviceID, projectID int64) (int64, error) {
	tag, err := db.Exec(ctx, `DELETE FROM push_subscriptions ps USING push_devices pd
		WHERE ps.device_id = pd.id AND pd.api_key_id = $1 AND pd.id = $2 AND ps.project_id = $3`, apiKeyID, deviceID, projectID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListPushSubscribers is every device subscribed to a project's alerts
// (internal/alerts.Notifier's push fan-out).
func ListPushSubscribers(ctx context.Context, db DB, projectID int64) ([]PushDevice, error) {
	rows, err := db.Query(ctx, `SELECT pd.id, pd.api_key_id, pd.token, pd.platform, pd.created_at
		FROM push_devices pd JOIN push_subscriptions ps ON ps.device_id = pd.id
		WHERE ps.project_id = $1`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PushDevice])
}
