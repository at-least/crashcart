package web

import (
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/store"
)

// Short aliases so templates stay readable.
type (
	sqlcIssue        = sqlc.Issue
	sqlcEvent        = sqlc.Event
	sqlcAlertChannel = sqlc.AlertChannel
)

type sentryFrame = sentry.Frame

// storeIssueFilter is a test helper: all issues of a project.
func storeIssueFilter(projectID int64) store.IssueFilter {
	return store.IssueFilter{ProjectID: projectID}
}
