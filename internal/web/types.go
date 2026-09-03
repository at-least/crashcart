package web

import (
	"github.com/at-least/crashcart/internal/sentry"
	"github.com/at-least/crashcart/internal/store"
)

// Short aliases so templates stay readable.
type (
	storeIssue         = store.Issue
	storeEvent         = store.Event
	storeAlertChannel  = store.AlertChannel
	storeAttachmentRow = store.AttachmentMeta
)

type sentryFrame = sentry.Frame

// storeIssueFilter is a test helper: all issues of a project.
func storeIssueFilter(projectID int64) store.IssueFilter {
	return store.IssueFilter{ProjectID: projectID}
}
