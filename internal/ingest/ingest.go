// Package ingest turns Sentry envelopes into rows: one COPY into events plus
// pre-folded aggregate upserts, all in a single transaction.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/sentry"
)

// Options tunes an Ingester.
type Options struct {
	Redact     bool
	SampleRate float64
	MaxEvents  int // per envelope (default 500)
	Now        func() time.Time
	Rand       func() float64
	RandID     func() int64 // random suffix source for event ids
}

// Ingester writes envelopes to Postgres.
type Ingester struct {
	pool *pgxpool.Pool
	opts Options
}

// Result summarizes one ingested envelope.
type Result struct {
	Events   int
	Sessions int
	Dropped  int // events removed by sampling
}

var (
	// ErrEmpty: nothing usable in the envelope.
	ErrEmpty = errors.New("no events in envelope")
	// ErrTooManyEvents: the envelope exceeds MaxEvents.
	ErrTooManyEvents = errors.New("too many events in envelope")
)

// New returns an Ingester bound to pool.
func New(pool *pgxpool.Pool, opts Options) *Ingester {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 500
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	if opts.RandID == nil {
		opts.RandID = rand.Int64
	}
	if opts.SampleRate <= 0 && opts.SampleRate != 0 {
		opts.SampleRate = 1
	}
	return &Ingester{pool: pool, opts: opts}
}

// Ingest parses and stores one envelope body.
func (in *Ingester) Ingest(ctx context.Context, body []byte) (Result, error) {
	now := in.opts.Now().UTC()
	env := sentry.Parse(body, now)

	var res Result
	events := env.Events[:0:0]
	for _, e := range env.Events {
		if keep(e.Level, in.opts.SampleRate, in.opts.Rand) {
			events = append(events, e)
		} else {
			res.Dropped++
		}
	}
	if len(events) == 0 && len(env.Sessions) == 0 {
		return res, ErrEmpty
	}
	if len(events) > in.opts.MaxEvents {
		return res, ErrTooManyEvents
	}
	res.Events, res.Sessions = len(events), len(env.Sessions)

	b := in.build(events, env.Sessions)

	// The id carries the event time plus a random suffix (internal/pk); a
	// clash with a row from another envelope is a unique violation, so
	// re-roll the suffixes and try again rather than fail the envelope.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		in.assignIDs(b.events, b.times)
		if err = in.write(ctx, b); !isUniqueViolation(err) {
			break
		}
	}
	return res, err
}

// assignIDs gives every event an id unique within the batch (the COPY has
// no ON CONFLICT), bumping the suffix on an in-batch clash.
func (in *Ingester) assignIDs(events []sqlc.InsertEventsParams, times []time.Time) {
	seen := make(map[int64]struct{}, len(events))
	for i := range events {
		id := pk.New(times[i], in.opts.RandID)
		for {
			if _, dup := seen[id]; !dup {
				break
			}
			id = pk.Lower(times[i]) + (id+1)%pk.Scale
		}
		seen[id] = struct{}{}
		events[i].ID = id
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (in *Ingester) write(ctx context.Context, b batch) error {
	tx, err := in.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)

	if len(b.events) > 0 {
		if _, err := q.InsertEvents(ctx, b.events); err != nil {
			return fmt.Errorf("insert events: %w", err)
		}
	}
	if err := batchErr("user_devices", q.UpsertUserDevice(ctx, b.devices).Exec); err != nil {
		return err
	}
	if err := batchErr("hourly_stats", q.UpsertHourlyStats(ctx, b.hourly).Exec); err != nil {
		return err
	}
	if err := batchErr("releases", q.UpsertRelease(ctx, b.releases).Exec); err != nil {
		return err
	}
	if err := batchErr("issues", q.UpsertIssue(ctx, b.issues).Exec); err != nil {
		return err
	}
	if err := batchErr("release_health", q.UpsertReleaseHealth(ctx, b.health).Exec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func batchErr(name string, exec func(func(int, error))) error {
	var first error
	exec(func(i int, err error) {
		if err != nil && first == nil {
			first = fmt.Errorf("%s[%d]: %w", name, i, err)
		}
	})
	return first
}

// batch is everything one envelope writes, with aggregate keys sorted so
// concurrent ingests take row locks in the same order (no deadlocks).
type batch struct {
	events []sqlc.InsertEventsParams // ids assigned by assignIDs from times
	times  []time.Time

	devices  []sqlc.UpsertUserDeviceParams
	hourly   []sqlc.UpsertHourlyStatsParams
	releases []sqlc.UpsertReleaseParams
	issues   []sqlc.UpsertIssueParams
	health   []sqlc.UpsertReleaseHealthParams
}

func (in *Ingester) build(events []*sentry.Event, sessions []sentry.Session) batch {
	var b batch
	type devKey struct{ user, device string }
	devices := map[devKey]time.Time{}
	type hourKey struct {
		hour  time.Time
		level string
	}
	hourly := map[hourKey]*sqlc.UpsertHourlyStatsParams{}
	releases := map[string]*sqlc.UpsertReleaseParams{}
	issues := map[string]*sqlc.UpsertIssueParams{}

	for _, e := range events {
		ts := e.Timestamp.UTC()
		userID := e.UserID
		message := e.Message
		tags := e.Tags
		if in.opts.Redact {
			userID = RedactUserID(userID)
			message = RedactText(message)
			tags = RedactTags(tags)
		}
		deviceID := e.DeviceID()

		var fp, loc string
		if e.ErrorType != "" {
			fp = e.Fingerprint()
			loc = e.Analyze().ErrorLocation
		}
		tagsJSON, _ := json.Marshal(tags)
		crumbsJSON, _ := json.Marshal(e.Breadcrumbs)
		if e.Breadcrumbs == nil {
			crumbsJSON = []byte("[]")
		}
		// jsonb rejects NUL characters inside strings.
		payload := bytes.ReplaceAll(e.Raw, []byte(`\u0000`), nil)

		b.times = append(b.times, ts)
		b.events = append(b.events, sqlc.InsertEventsParams{
			EventID:       nullable(e.EventID),
			Level:         e.Level,
			Message:       message,
			Platform:      nullable(e.Platform),
			Environment:   nullable(e.Environment),
			Release:       nullable(e.Release),
			DeviceID:      nullable(deviceID),
			DeviceModel:   nullable(e.DeviceModel),
			OsVersion:     nullable(e.OSVersion),
			Screen:        nullable(e.Screen),
			ErrorType:     nullable(e.ErrorType),
			ErrorLocation: nullable(loc),
			Handled:       e.Handled,
			SdkName:       nullable(e.SDKName),
			UserID:        nullable(userID),
			Fingerprint:   nullable(fp),
			Tags:          tagsJSON,
			Breadcrumbs:   crumbsJSON,
			Payload:       payload,
		})

		if userID != "" && deviceID != "" {
			k := devKey{userID, deviceID}
			if prev, ok := devices[k]; !ok || ts.After(prev) {
				devices[k] = ts
			}
		}

		crash := e.IsCrash()
		if e.Level == "error" || e.Level == "fatal" {
			k := hourKey{ts.Truncate(time.Hour), e.Level}
			h := hourly[k]
			if h == nil {
				h = &sqlc.UpsertHourlyStatsParams{Hour: k.hour, Level: k.level}
				hourly[k] = h
			}
			if crash {
				h.CrashCount++
			}
			if e.Level == "fatal" {
				h.FatalCount++
			} else if e.Handled == nil || *e.Handled {
				h.ErrorCount++
			}
		}

		if e.Release != "" {
			r := releases[e.Release]
			if r == nil {
				r = &sqlc.UpsertReleaseParams{Version: e.Release, Platform: nullable(e.Platform), FirstSeen: ts, LastSeen: ts}
				releases[e.Release] = r
			}
			if ts.Before(r.FirstSeen) {
				r.FirstSeen = ts
			}
			if ts.After(r.LastSeen) {
				r.LastSeen = ts
			}
			if crash {
				r.CrashCount++
			}
			if e.Level == "error" || e.Level == "fatal" {
				r.ErrorCount++
			}
			r.TotalEvents++
		}

		if fp != "" {
			is := issues[fp]
			if is == nil {
				is = &sqlc.UpsertIssueParams{
					Fingerprint:  fp,
					Title:        e.IssueTitle(),
					Level:        e.Level,
					ErrorType:    nullable(e.ErrorType),
					Screen:       nullable(e.Screen),
					Platform:     nullable(e.Platform),
					FirstSeen:    ts,
					LastSeen:     ts,
					FirstRelease: nullable(e.Release),
					LastRelease:  nullable(e.Release),
				}
				issues[fp] = is
			}
			is.EventCount++
			if ts.Before(is.FirstSeen) {
				is.FirstSeen = ts
				if e.Release != "" {
					is.FirstRelease = nullable(e.Release)
				}
			}
			if !ts.Before(is.LastSeen) {
				is.LastSeen = ts
				if e.Release != "" {
					is.LastRelease = nullable(e.Release)
				}
			}
		}
	}

	type healthKey struct {
		release string
		day     time.Time
	}
	health := map[healthKey]*sqlc.UpsertReleaseHealthParams{}
	for _, s := range sessions {
		d := s.StartedAt.UTC()
		k := healthKey{s.Release, time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)}
		h := health[k]
		if h == nil {
			h = &sqlc.UpsertReleaseHealthParams{Release: k.release, Day: k.day}
			health[k] = h
		}
		h.TotalSessions++
		switch s.Status {
		case "crashed":
			h.CrashedSessions++
			h.ErroredSessions++
		case "errored", "abnormal":
			h.ErroredSessions++
		}
	}

	for k, ts := range devices {
		b.devices = append(b.devices, sqlc.UpsertUserDeviceParams{UserID: k.user, DeviceID: k.device, LastSeen: ts})
	}
	sort.Slice(b.devices, func(i, j int) bool {
		if b.devices[i].UserID != b.devices[j].UserID {
			return b.devices[i].UserID < b.devices[j].UserID
		}
		return b.devices[i].DeviceID < b.devices[j].DeviceID
	})
	for _, h := range hourly {
		b.hourly = append(b.hourly, *h)
	}
	sort.Slice(b.hourly, func(i, j int) bool {
		if !b.hourly[i].Hour.Equal(b.hourly[j].Hour) {
			return b.hourly[i].Hour.Before(b.hourly[j].Hour)
		}
		return b.hourly[i].Level < b.hourly[j].Level
	})
	for _, r := range releases {
		b.releases = append(b.releases, *r)
	}
	sort.Slice(b.releases, func(i, j int) bool { return b.releases[i].Version < b.releases[j].Version })
	for _, is := range issues {
		b.issues = append(b.issues, *is)
	}
	sort.Slice(b.issues, func(i, j int) bool { return b.issues[i].Fingerprint < b.issues[j].Fingerprint })
	for _, h := range health {
		b.health = append(b.health, *h)
	}
	sort.Slice(b.health, func(i, j int) bool {
		if b.health[i].Release != b.health[j].Release {
			return b.health[i].Release < b.health[j].Release
		}
		return b.health[i].Day.Before(b.health[j].Day)
	})
	return b
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
