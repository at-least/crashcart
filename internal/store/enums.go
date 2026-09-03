package store

// The Postgres enums (internal/db/migrations/00001_baseline.sql), as plain
// Go string types. No Scan/Value methods: verified this session that pgx
// decodes/encodes a named string-kind type against an enum column with no
// custom methods needed (nullable columns use *T, matching every other
// nullable column in this package).

type AlertType string

const (
	AlertTypeNewIssue         AlertType = "new_issue"
	AlertTypeRegression       AlertType = "regression"
	AlertTypeUnhandledSpike   AlertType = "unhandled_spike"
	AlertTypeEscalating       AlertType = "escalating"
	AlertTypeMonitorFailed    AlertType = "monitor_failed"
	AlertTypeMonitorRecovered AlertType = "monitor_recovered"
)

type ChannelKind string

const (
	ChannelKindWebhook  ChannelKind = "webhook"
	ChannelKindSlack    ChannelKind = "slack"
	ChannelKindTelegram ChannelKind = "telegram"
)

type CheckinStatus string

const (
	CheckinStatusInProgress CheckinStatus = "in_progress"
	CheckinStatusOk         CheckinStatus = "ok"
	CheckinStatusError      CheckinStatus = "error"
	CheckinStatusMissed     CheckinStatus = "missed"
	CheckinStatusTimeout    CheckinStatus = "timeout"
)

type EventLevel string

const (
	EventLevelFatal   EventLevel = "fatal"
	EventLevelError   EventLevel = "error"
	EventLevelWarning EventLevel = "warning"
	EventLevelInfo    EventLevel = "info"
	EventLevelDebug   EventLevel = "debug"
)

type IssueStatus string

const (
	IssueStatusUnresolved IssueStatus = "unresolved"
	IssueStatusResolved   IssueStatus = "resolved"
	IssueStatusIgnored    IssueStatus = "ignored"
	IssueStatusRegression IssueStatus = "regression"
)

type JobKind string

const (
	JobKindSymbolicate   JobKind = "symbolicate"
	JobKindResymbolicate JobKind = "resymbolicate"
	JobKindAlert         JobKind = "alert"
)

type SessionStatus string

const (
	SessionStatusOk       SessionStatus = "ok"
	SessionStatusExited   SessionStatus = "exited"
	SessionStatusCrashed  SessionStatus = "crashed"
	SessionStatusErrored  SessionStatus = "errored"
	SessionStatusAbnormal SessionStatus = "abnormal"
)

type SymbolKind string

const (
	SymbolKindProguard  SymbolKind = "proguard"
	SymbolKindSourcemap SymbolKind = "sourcemap"
	SymbolKindDsym      SymbolKind = "dsym"
)
