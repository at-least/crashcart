package web

import (
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
)

// Short aliases so templates stay readable.
type (
	sqlcIssue         = sqlc.Issue
	sqlcEvent         = sqlc.Event
	sqlcAlertChannel  = sqlc.AlertChannel
	sqlcAttachmentRow = sqlc.ListAttachmentsRow
)

type sentryFrame = sentry.Frame

// storeIssueFilter is a test helper: all issues of a project.
func storeIssueFilter(projectID int64) store.IssueFilter {
	return store.IssueFilter{ProjectID: projectID}
}
