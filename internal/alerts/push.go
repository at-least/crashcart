package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/crashcartapp/crashcart/internal/store"
)

// FCMEndpoint is the Firebase Cloud Messaging HTTP v1 base; tests override it.
var FCMEndpoint = "https://fcm.googleapis.com"

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// fcmCredentials is FCM_SERVICE_ACCOUNT_JSON parsed once: an OAuth2 token
// source (self-refreshing) plus the Firebase project id the send URL needs.
type fcmCredentials struct {
	tokens    oauth2.TokenSource
	projectID string
}

// fcmAuth resolves to (access token, project id). n.FCMToken (tests) skips
// google.CredentialsFromJSON entirely, so a push test needs no real service
// account key.
func (n *Notifier) fcmAuth(ctx context.Context) (token, projectID string, err error) {
	if n.FCMToken != nil {
		return n.FCMToken(ctx)
	}
	n.fcmOnce.Do(func() {
		if n.Cfg.FCMServiceAccountJSON == "" {
			n.fcmErr = errors.New("FCM_SERVICE_ACCOUNT_JSON not set")
			return
		}
		var sa struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal([]byte(n.Cfg.FCMServiceAccountJSON), &sa); err != nil || sa.ProjectID == "" {
			n.fcmErr = fmt.Errorf("FCM_SERVICE_ACCOUNT_JSON: missing project_id")
			return
		}
		creds, err := google.CredentialsFromJSON(context.Background(), []byte(n.Cfg.FCMServiceAccountJSON), fcmScope)
		if err != nil {
			n.fcmErr = fmt.Errorf("FCM_SERVICE_ACCOUNT_JSON: %w", err)
			return
		}
		n.fcmCreds = &fcmCredentials{tokens: creds.TokenSource, projectID: sa.ProjectID}
	})
	if n.fcmErr != nil {
		return "", "", n.fcmErr
	}
	tok, err := n.fcmCreds.tokens.Token()
	if err != nil {
		return "", "", fmt.Errorf("FCM token: %w", err)
	}
	return tok.AccessToken, n.fcmCreds.projectID, nil
}

// sendPush delivers one alert to one device via FCM HTTP v1 — the same
// call for iOS and Android: FCM hands iOS delivery to APNs itself once the
// app registered with Firebase Messaging.
func (n *Notifier) sendPush(ctx context.Context, d store.PushDevice, payload Payload) error {
	token, projectID, err := n.fcmAuth(ctx)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	title, body, _ := strings.Cut(TelegramText(payload), "\n")
	msg := map[string]any{
		"message": map[string]any{
			"token":        d.Token,
			"notification": map[string]string{"title": title, "body": body},
			"data": map[string]string{
				"type": payload.Type, "project_slug": payload.ProjectSlug,
				"fingerprint": payload.Fingerprint, "url": payload.URL,
			},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, FCMEndpoint+"/v1/projects/"+projectID+"/messages:send", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := n.client().Do(req)
	if err != nil {
		return fmt.Errorf("push: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push: %s", resp.Status)
	}
	return nil
}
