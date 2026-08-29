// Package ingest is the Sentry-compatible write path:
//
//	POST /api/{project_id}/envelope/   (envelope protocol — every modern SDK)
//	POST /api/{project_id}/store/      (legacy single-event JSON)
//
// The DSN public key authenticates the request. Per envelope, one
// transaction writes events, sessions and the folded issue upserts; the
// only work deferred to the job worker is symbolication that needs a
// symbol file not yet cached, and alert delivery.
package ingest

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/newlix/crashcart/internal/auth"
	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/db/sqlc"
	"github.com/newlix/crashcart/internal/pk"
	"github.com/newlix/crashcart/internal/sentry"
	"github.com/newlix/crashcart/internal/store"
)

// Limits on what one request may carry.
const (
	MaxBody   = 20 << 20 // 20 MB envelope
	MaxEvents = 500
)

// Symbolicator resolves frames inline when a mapping is already cached.
// ok=false means "not now" — the event is stored as-is and a job is queued.
type Symbolicator interface {
	Inline(ctx context.Context, projectID int64, ev *sentry.Event) (frames []sentry.Frame, ok bool)
}

// Ingester holds the dependencies of the write path.
type Ingester struct {
	Store   *store.Store
	Cfg     config.Config
	Symbols Symbolicator // may be nil
	Log     *slog.Logger

	mu    sync.Mutex
	byKey map[string]cachedProject
}

type cachedProject struct {
	p   sqlc.Project
	exp time.Time
}

// Result summarizes one Ingest call.
type Result struct {
	Received    int      // events parsed
	Stored      int      // events written
	Sampled     int      // events counted but not stored
	Duplicates  int      // resent events already stored
	Invalid     int      // event items that did not parse
	Sessions    int      // session rows written
	NewIssues   []string // fingerprints created by this envelope
	Regressions []string // fingerprints flipped to 'regression'
	Jobs        int
}

// Handler routes the Sentry endpoints. The {project} path value is the
// numeric DSN project id; it is checked against the key's project.
func (in *Ingester) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project}/envelope/", in.serveEnvelope)
	mux.HandleFunc("POST /api/{project}/envelope", in.serveEnvelope)
	mux.HandleFunc("POST /api/{project}/store/", in.serveStore)
	mux.HandleFunc("POST /api/{project}/store", in.serveStore)
	rl := auth.RateLimit(in.Store, in.Cfg.RateLimit, auth.SentryKey)
	return auth.Chain(mux, auth.CORS(in.Cfg.CORSOrigin), rl)
}

// Project resolves the DSN key (cached 60 s) and checks the path id.
func (in *Ingester) Project(r *http.Request) (sqlc.Project, error) {
	key := auth.SentryKey(r)
	if key == "" {
		return sqlc.Project{}, errUnauthorized
	}
	in.mu.Lock()
	c, ok := in.byKey[key]
	in.mu.Unlock()
	if !ok || time.Now().After(c.exp) {
		p, err := in.Store.GetProjectByKey(r.Context(), key)
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.Project{}, errUnauthorized
		}
		if err != nil {
			return sqlc.Project{}, err
		}
		c = cachedProject{p: p, exp: time.Now().Add(time.Minute)}
		in.mu.Lock()
		if in.byKey == nil {
			in.byKey = map[string]cachedProject{}
		}
		in.byKey[key] = c
		in.mu.Unlock()
	}
	if pid := r.PathValue("project"); pid != "" && pid != fmt.Sprint(c.p.ID) && pid != c.p.Slug {
		return sqlc.Project{}, errUnauthorized
	}
	return c.p, nil
}

var errUnauthorized = errors.New("unauthorized")

func (in *Ingester) serveEnvelope(w http.ResponseWriter, r *http.Request) {
	p, err := in.Project(r)
	if err != nil {
		in.fail(w, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now().UTC()
	env := sentry.Parse(body, now)
	if len(env.Events) > MaxEvents {
		http.Error(w, `{"error":"too many events"}`, http.StatusRequestEntityTooLarge)
		return
	}
	if env.Invalid > 0 {
		in.Log.Warn("ingest: unparseable event item", "project", p.Slug, "invalid", env.Invalid, "sdk", r.Header.Get("User-Agent"))
	}
	res, err := in.Ingest(detached(r), p, env, now)
	if err != nil {
		in.fail(w, err)
		return
	}
	res.Invalid = env.Invalid
	in.ok(w, res, firstEventID(env))
}

func (in *Ingester) serveStore(w http.ResponseWriter, r *http.Request) {
	p, err := in.Project(r)
	if err != nil {
		in.fail(w, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now().UTC()
	var env sentry.Envelope
	if ev := sentry.ParseEvent("", now, body, now); ev != nil {
		env.Events = append(env.Events, ev)
	}
	res, err := in.Ingest(detached(r), p, env, now)
	if err != nil {
		in.fail(w, err)
		return
	}
	in.ok(w, res, firstEventID(env))
}

// detached is the context for the write once the body is in hand: a crashing
// mobile process closes its connection right after sending, and that must
// not roll back the transaction (the SDK would resend from its cache).
func detached(r *http.Request) context.Context {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	_ = cancel // bounded by the timeout; nothing to release early
	return ctx
}

// readBody reads the request body, transparently decoding the
// Content-Encoding SDKs use (sentry-python gzips every envelope; others
// send deflate). Both the wire size and the decoded size are capped at
// MaxBody so a compression bomb cannot exhaust memory.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(w, r.Body, MaxBody)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, errors.New("invalid gzip body")
		}
		defer gz.Close()
		rd = gz
	case "deflate":
		zr, err := zlib.NewReader(rd)
		if err != nil {
			return nil, errors.New("invalid deflate body")
		}
		defer zr.Close()
		rd = zr
	default:
		return nil, errors.New("unsupported content-encoding")
	}
	body, err := io.ReadAll(io.LimitReader(rd, MaxBody+1))
	if err != nil {
		return nil, errors.New("body too large or truncated")
	}
	if len(body) > MaxBody {
		return nil, errors.New("body too large")
	}
	return body, nil
}

func firstEventID(env sentry.Envelope) string {
	if len(env.Events) > 0 {
		return env.Events[0].EventID
	}
	return ""
}

func (in *Ingester) ok(w http.ResponseWriter, res Result, id string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "received": res.Received, "stored": res.Stored, "sessions": res.Sessions, "invalid": res.Invalid})
}

func (in *Ingester) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnauthorized) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	in.Log.Error("ingest", "err", err)
	http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
}

// prepared is one event after analysis, before the write.
type prepared struct {
	id           int64
	ev           *sentry.Event
	frames       []sentry.Frame // symbolicated when inline succeeded
	symbolicated bool
	fingerprint  string
	location     string
	store        bool
}

// Ingest writes one parsed envelope for project p.
func (in *Ingester) Ingest(ctx context.Context, p sqlc.Project, env sentry.Envelope, now time.Time) (Result, error) {
	var res Result
	res.Received = len(env.Events)
	if len(env.Events) == 0 && len(env.Sessions) == 0 {
		return res, nil
	}

	// 0. Ids are derived from the event's own id, so an envelope the SDK
	//    resends (after a timeout, or from its crash cache) maps to the same
	//    rows: those are dropped here, before they could be counted twice.
	events, dupes, err := in.dedupe(ctx, env.Events)
	if err != nil {
		return res, err
	}
	res.Duplicates = dupes

	// 1. Analyze. Inline symbolication when a mapping is cached; the
	//    fingerprint is computed on the best frames we have.
	preps := make([]*prepared, 0, len(events))
	groups := map[string][]*prepared{}
	var order []string
	for _, ev := range events {
		if in.Cfg.PIIRedact {
			redact(ev)
		}
		pr := &prepared{id: eventPK(ev), ev: ev, frames: ev.Frames()}
		if in.Symbols != nil && ev.NeedsSymbolication() {
			if fr, ok := in.Symbols.Inline(ctx, p.ID, ev); ok {
				pr.frames, pr.symbolicated = fr, true
			}
		}
		pr.fingerprint = sentry.Fingerprint(ev, pr.frames)
		pr.location = sentry.ErrorLocation(pr.frames)
		preps = append(preps, pr)
		if pr.fingerprint != "" {
			if _, seen := groups[pr.fingerprint]; !seen {
				order = append(order, pr.fingerprint)
			}
			groups[pr.fingerprint] = append(groups[pr.fingerprint], pr)
		} else {
			pr.store = in.Cfg.PIIRedact || rand.Float64() < p.SampleRate || p.SampleRate >= 1
		}
	}

	// 2. One transaction: issues (folded per fingerprint), sampling, events, sessions, jobs.
	var jobs []sqlc.EnqueueJobParams
	err = in.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		for _, fp := range order {
			g := groups[fp]
			first := g[0]
			var minID, maxID int64
			var lastRelease *string
			level := "error"
			for _, pr := range g {
				id := pk.Lower(pr.ev.Timestamp)
				if minID == 0 || id < minID {
					minID = id
				}
				if id >= maxID {
					maxID = id
					lastRelease = nilIfEmpty(pr.ev.Release)
				}
				if pr.ev.Level == "fatal" {
					level = "fatal"
				}
			}
			row, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
				ProjectID: p.ID, Fingerprint: fp, Title: first.ev.IssueTitle(), Level: level,
				ErrorType: nilIfEmpty(first.ev.ErrorType), Screen: nilIfEmpty(first.ev.Screen), Platform: nilIfEmpty(first.ev.Platform),
				EventCount: int64(len(g)), StoredCount: 0, FirstSeen: minID, LastSeen: maxID, FirstRelease: lastRelease,
			})
			if err != nil {
				return fmt.Errorf("upsert issue: %w", err)
			}
			if row.Created {
				res.NewIssues = append(res.NewIssues, fp)
				jobs = append(jobs, alertJob(p.ID, "new_issue", fp))
			} else if row.Status == "regression" && row.UpdatedAt.After(now.Add(-time.Second)) && regressedNow(row) {
				res.Regressions = append(res.Regressions, fp)
				jobs = append(jobs, alertJob(p.ID, "regression", fp))
			}
			// Sampling: keep the first N per issue, then a fraction; fatal always.
			prev := row.EventCount - int64(len(g))
			stored := int64(0)
			for i, pr := range g {
				seq := prev + int64(i) + 1
				pr.store = pr.ev.Level == "fatal" || seq <= int64(p.SampleKeepFirst) || p.SampleRate >= 1 || rand.Float64() < p.SampleRate
				if pr.store {
					stored++
				}
			}
			if stored > 0 {
				if err := q.AddIssueStored(ctx, sqlc.AddIssueStoredParams{ProjectID: p.ID, Fingerprint: fp, StoredCount: stored}); err != nil {
					return err
				}
			}
		}

		rows := make([]store.EventInsert, 0, len(preps))
		for _, pr := range preps {
			if !pr.store {
				res.Sampled++
				continue
			}
			ev := pr.ev
			id := pr.id
			tags, _ := json.Marshal(ev.Tags)
			crumbs, _ := json.Marshal(nonNil(ev.Breadcrumbs))
			var symbols json.RawMessage
			if pr.symbolicated {
				symbols, _ = json.Marshal(pr.frames)
			}
			rows = append(rows, store.EventInsert{
				ID: id, ProjectID: p.ID, EventID: ev.EventID, Level: ev.Level, Message: ev.Message,
				Platform: nilIfEmpty(ev.Platform), Environment: nilIfEmpty(ev.Environment), Release: nilIfEmpty(ev.Release),
				DeviceID: nilIfEmpty(ev.DeviceID()), DeviceModel: nilIfEmpty(ev.DeviceModel), OSVersion: nilIfEmpty(ev.OSVersion),
				Screen: nilIfEmpty(ev.Screen), ErrorType: nilIfEmpty(ev.ErrorType), ErrorLocation: nilIfEmpty(pr.location),
				Handled: ev.Handled, SDKName: nilIfEmpty(ev.SDKName), UserID: nilIfEmpty(ev.UserID),
				Fingerprint: nilIfEmpty(pr.fingerprint), Symbolicated: pr.symbolicated,
				Tags: tags, Breadcrumbs: crumbs, Payload: ev.Raw, Symbols: symbols,
			})
			if !pr.symbolicated && ev.NeedsSymbolication() && pr.fingerprint != "" {
				args, _ := json.Marshal(map[string]any{"event": id})
				jobs = append(jobs, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: p.ID, Args: args, RunAfter: now})
			}
		}
		if err := store.InsertEvents(ctx, tx, rows); err != nil {
			return fmt.Errorf("insert events: %w", err)
		}
		res.Stored = len(rows)

		for _, s := range env.Sessions {
			if err := q.InsertSession(ctx, sqlc.InsertSessionParams{
				ID: sessionID(s), ProjectID: p.ID, Release: s.Release, Environment: nilIfEmpty(s.Environment),
				Status: s.Status, Count: int32(max(s.Count, 1)),
			}); err != nil {
				return fmt.Errorf("insert session: %w", err)
			}
			res.Sessions++
		}
		for _, j := range jobs {
			if err := q.EnqueueJob(ctx, j); err != nil {
				return err
			}
		}
		res.Jobs = len(jobs)
		return nil
	})
	return res, err
}

// regressedNow is true when the upsert flipped the status in this call:
// the row is a regression and its updated_at is fresh. Callers already
// checked the timestamp; this exists to keep the intent readable.
func regressedNow(row sqlc.UpsertIssueRow) bool { return row.Status == "regression" }

// eventPK derives the primary key from the event's own id: the millisecond
// from its timestamp, the low part from a hash of event_id. Two deliveries
// of one event collide (and the second is dropped); two different events
// in the same millisecond collide with the same 1/1000 odds a random low
// part had. Within one envelope a collision is bumped to the next slot.
func eventPK(ev *sentry.Event) int64 {
	h := fnv.New32a()
	h.Write([]byte(ev.EventID))
	return pk.Lower(ev.Timestamp) + int64(h.Sum32()%pk.Scale)
}

// dedupe resolves in-envelope id collisions and drops events whose id is
// already stored.
func (in *Ingester) dedupe(ctx context.Context, events []*sentry.Event) ([]*sentry.Event, int, error) {
	if len(events) == 0 {
		return events, 0, nil
	}
	seen := map[int64]string{}
	ids := make([]int64, 0, len(events))
	var out []*sentry.Event
	dupes := 0
	for _, ev := range events {
		id := eventPK(ev)
		for prev, ok := seen[id]; ok && prev != ev.EventID && id%pk.Scale < pk.Scale-1; prev, ok = seen[id] {
			id++ // different event, same slot: next slot in the same millisecond
		}
		if prev, ok := seen[id]; ok && prev == ev.EventID {
			dupes++ // the same event twice in one envelope
			continue
		}
		seen[id] = ev.EventID
		ev.Timestamp = pk.Time(id) // keep timestamp and id consistent after a bump
		ids = append(ids, id)
		out = append(out, ev)
	}
	existing, err := in.Store.ExistingEventIDs(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("dedupe: %w", err)
	}
	if len(existing) == 0 {
		return out, dupes, nil
	}
	skip := map[int64]bool{}
	for _, id := range existing {
		skip[id] = true
	}
	kept := out[:0]
	for i, ev := range out {
		if skip[ids[i]] {
			dupes++
			continue
		}
		kept = append(kept, ev)
	}
	return kept, dupes, nil
}

// sessionID makes every update of one session (same sid, same start) land
// on the same row: the random low part of the id is replaced by a hash of
// the sid, so the upsert in InsertSession dedupes ok → exited/crashed.
func sessionID(s sentry.Session) int64 {
	if s.SID == "" {
		return pk.New(s.StartedAt)
	}
	h := fnv.New32a()
	h.Write([]byte(s.SID))
	return pk.Lower(s.StartedAt) + int64(h.Sum32()%pk.Scale)
}

func alertJob(projectID int64, typ, fp string) sqlc.EnqueueJobParams {
	args, _ := json.Marshal(map[string]any{"type": typ, "fingerprint": fp})
	return sqlc.EnqueueJobParams{Kind: "alert", ProjectID: projectID, Args: args, RunAfter: time.Now()}
}

func redact(ev *sentry.Event) {
	ev.Message = RedactText(ev.Message)
	ev.Tags = RedactTags(ev.Tags)
	ev.UserID = RedactUserID(ev.UserID)
	for i := range ev.Breadcrumbs {
		ev.Breadcrumbs[i].Message = RedactText(ev.Breadcrumbs[i].Message)
	}
	// The raw payload is stored verbatim otherwise; with redaction on we
	// scrub the whole document textually so nothing leaks via detail views.
	ev.Raw = []byte(RedactText(string(ev.Raw)))
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
