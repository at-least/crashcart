// Package ingest is the Sentry-compatible write path:
//
//	POST /api/{project_id}/envelope/   (envelope protocol — every modern SDK)
//	POST /api/{project_id}/store/      (legacy single-event JSON)
//
// The DSN public key authenticates the request. Per envelope, one
// transaction writes events (payload gzipped), sessions and the folded
// issue upserts. The only work deferred to the job worker is
// symbolication that needs a symbol file not yet cached, and alert
// delivery.
package ingest

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
	"github.com/crashcartapp/crashcart/internal/sentry"
	"github.com/crashcartapp/crashcart/internal/store"
)

// Limits on what one request may carry.
const (
	MaxBody      = 20 << 20 // 20 MB envelope
	MaxEvents    = 500
	WriteTimeout = 30 * time.Second // the write outlives a client that hangs up
	keyCacheTTL  = 10 * time.Second // a rotated DSN key stops working within this
	// SymbolicateBudget is how long one envelope may spend in the
	// Symbolicator (the dSYM sidecar) before the remaining native events
	// are left to the job worker.
	SymbolicateBudget = 3 * time.Second
	// FatalKeepFactor: crashes get this many times sample_keep_first
	// before sampling starts — more samples of what matters most, still
	// bounded (a crash loop must not fill the database).
	FatalKeepFactor = 5
)

// Symbolicator resolves frames at ingest. ok=false stores the event as-is
// (a later symbol upload re-queues it); retry=true means the resolver
// failed transiently (sidecar down, budget exhausted), so a "symbolicate"
// job is queued to finish the work.
type Symbolicator interface {
	Resolve(ctx context.Context, projectID int64, ev *sentry.Event) (frames []sentry.Frame, ok, retry bool)
}

// Ingester holds the dependencies of the write path.
type Ingester struct {
	Store   *store.Store
	Cfg     config.Config
	Symbols Symbolicator // may be nil
	Log     *slog.Logger

	mu          sync.Mutex
	byKey       map[string]cachedProject
	warned      map[int64]time.Time // last platform-mismatch warning per project
	quotaWarned map[int64]time.Time // last quota warning per project
	exhausted   map[int64]exhaustedQuota
}

// exhaustedQuota remembers that a project's daily quota was hit, so the
// envelopes that follow are refused before any work is done (the SDKs
// also stop sending on the rate-limit header, but not every one does).
// It expires at the next UTC day, or when the quota is changed.
type exhaustedQuota struct {
	quota int32
	until time.Time
}

func nextUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

// quotaExhausted reports whether the project's quota is known to be used
// up for the day (see exhaustedQuota).
func (in *Ingester) quotaExhausted(p sqlc.Project, now time.Time) bool {
	if p.DailyQuota <= 0 {
		return false
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	e, ok := in.exhausted[p.ID]
	return ok && e.quota == p.DailyQuota && now.Before(e.until)
}

// checkQuota is the last statement of the ingest transaction: it counts n
// events against the project's UTC day and fails with ErrQuota (rolling
// everything back) when that crosses the daily quota (0 = unlimited). The
// count lives in project_usage, so it is exact across replicas; being
// last, the row lock — the one row every envelope of a project touches —
// is held only until the commit, not across the whole write. Sessions
// ride along with events, so a rejected envelope loses its sessions too —
// acceptable for a project that is being flooded.
func (in *Ingester) checkQuota(ctx context.Context, q *sqlc.Queries, p sqlc.Project, n int, now time.Time) error {
	if p.DailyQuota <= 0 || n == 0 {
		return nil
	}
	total, err := q.AddProjectUsage(ctx, sqlc.AddProjectUsageParams{ProjectID: p.ID, Day: now.UTC().Truncate(24 * time.Hour), Events: int64(n)})
	if err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	if total <= int64(p.DailyQuota) {
		return nil
	}
	in.mu.Lock()
	if in.exhausted == nil {
		in.exhausted = map[int64]exhaustedQuota{}
	}
	in.exhausted[p.ID] = exhaustedQuota{quota: p.DailyQuota, until: nextUTCDay(now)}
	warn := now.Sub(in.quotaWarned[p.ID]) > time.Minute
	if warn {
		if in.quotaWarned == nil {
			in.quotaWarned = map[int64]time.Time{}
		}
		in.quotaWarned[p.ID] = now
	}
	in.mu.Unlock()
	if warn {
		in.Log.Warn("ingest: daily quota exceeded", "project", p.Slug, "quota", p.DailyQuota, "today", total-int64(n))
	}
	return ErrQuota
}

type cachedProject struct {
	p   sqlc.Project
	exp time.Time
}

// Result summarizes one Ingest call.
type Result struct {
	Received    int         // events parsed
	Stored      int         // events written
	Sampled     int         // events counted but not stored
	Duplicates  int         // resent events already stored
	Mismatched  int         // events whose platform family is not the project's
	Invalid     int         // event items that did not parse
	Sessions    int         // session rows written
	NewIssues   []sentry.ID // fingerprints created by this envelope
	Regressions []sentry.ID // fingerprints flipped to 'regression'
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
	rl := auth.RateLimit(in.Cfg.RateLimit, auth.SentryKey)
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
		c = cachedProject{p: p, exp: time.Now().Add(keyCacheTTL)}
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

// ErrQuota: the project's daily quota is used up; nothing was written.
var ErrQuota = errors.New("daily quota exceeded")

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
	ctx, cancel := detached(r)
	defer cancel()
	res, err := in.Ingest(ctx, p, env, now)
	if err != nil {
		in.fail(w, err)
		return
	}
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
	ctx, cancel := detached(r)
	defer cancel()
	res, err := in.Ingest(ctx, p, env, now)
	if err != nil {
		in.fail(w, err)
		return
	}
	in.ok(w, res, firstEventID(env))
}

// detached is the context for the write once the body is in hand: a crashing
// mobile process closes its connection right after sending, and that must
// not roll back the transaction (the SDK would resend from its cache).
func detached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), WriteTimeout)
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
		return string(env.Events[0].EventID)
	}
	return ""
}

func (in *Ingester) ok(w http.ResponseWriter, res Result, id string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "received": res.Received, "stored": res.Stored, "sessions": res.Sessions, "invalid": res.Invalid, "mismatched": res.Mismatched})
}

func (in *Ingester) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnauthorized) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if errors.Is(err, ErrQuota) {
		// Sentry's rate-limit header: SDKs stop sending until the next UTC day.
		secs := int(time.Until(nextUTCDay(time.Now())).Seconds()) + 1
		w.Header().Set("X-Sentry-Rate-Limits", strconv.Itoa(secs)+":error;transaction;session:project:quota")
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, `{"error":"daily quota exceeded"}`, http.StatusTooManyRequests)
		return
	}
	in.Log.Error("ingest", "err", err)
	http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
}

// prepared is one event after analysis, before the write.
type prepared struct {
	ev           *sentry.Event
	frames       []sentry.Frame // symbolicated when Resolve succeeded
	symbolicated bool
	retry        bool // Resolve failed transiently: queue a job
	fingerprint  sentry.ID
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

	res.Invalid = env.Invalid
	if len(env.Events) > 0 && in.quotaExhausted(p, now) {
		return res, ErrQuota
	}
	expected := ""
	if p.Platform != nil {
		expected = *p.Platform
	}

	// 1. Analyze. Symbolicate now when a mapping (or the sidecar) can
	//    resolve the frames — the fingerprint is computed on the best
	//    frames we have, so the issue is right from the start. The sidecar
	//    gets one time budget per envelope; what it cannot finish goes to
	//    the job worker.
	preps := make([]*prepared, 0, len(env.Events))
	seenEvent := map[sentry.ID]bool{}
	symCtx, cancelSym := context.WithTimeout(ctx, SymbolicateBudget)
	defer cancelSym()
	for _, ev := range env.Events {
		if seenEvent[ev.EventID] {
			res.Duplicates++ // the same event twice in one envelope
			continue
		}
		seenEvent[ev.EventID] = true
		if in.Cfg.PIIRedact {
			redact(ev)
		}
		if fam := sentry.Family(ev.Platform, ev.SDKName); !sentry.Accepts(expected, fam) {
			res.Mismatched++
			in.warnMismatch(p, ev, fam)
		}
		pr := &prepared{ev: ev, frames: ev.Frames()}
		if in.Symbols != nil && ev.NeedsSymbolication() {
			fr, ok, retry := in.Symbols.Resolve(symCtx, p.ID, ev)
			if ok {
				pr.frames, pr.symbolicated = fr, true
			}
			pr.retry = retry && !ok
		}
		pr.fingerprint = sentry.Fingerprint(ev, pr.frames)
		pr.location = sentry.ErrorLocation(pr.frames)
		preps = append(preps, pr)
	}

	// 2. One transaction: drop resends, fold by fingerprint, upsert issues,
	//    decide sampling, write events, sessions, jobs; the quota bump
	//    last, so its per-project row lock is held only until the commit.
	var jobs []sqlc.EnqueueJobParams
	err := in.Store.Tx(ctx, func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error {
		// An envelope the SDK resends (after a timeout, or from its crash
		// cache) carries event_ids already stored: those must not be
		// counted twice.
		if len(preps) > 0 {
			ids := make([]sentry.ID, len(preps))
			from, to := preps[0].ev.Timestamp, preps[0].ev.Timestamp
			for i, pr := range preps {
				ids[i] = pr.ev.EventID
				if pr.ev.Timestamp.Before(from) {
					from = pr.ev.Timestamp
				}
				if pr.ev.Timestamp.After(to) {
					to = pr.ev.Timestamp
				}
			}
			existing, err := q.ExistingEventIDs(ctx, sqlc.ExistingEventIDsParams{ProjectID: p.ID, Column2: ids, FromAt: from, ToAt: to.Add(time.Microsecond)})
			if err != nil {
				return fmt.Errorf("dedupe: %w", err)
			}
			if len(existing) > 0 {
				stored := map[sentry.ID]bool{}
				for _, id := range existing {
					stored[id] = true
				}
				kept := preps[:0]
				for _, pr := range preps {
					if stored[pr.ev.EventID] {
						res.Duplicates++
					} else {
						kept = append(kept, pr)
					}
				}
				preps = kept
			}
		}
		// Releases mentioned by this envelope (events and sessions).
		type relSeen struct {
			platform string
			at       time.Time
		}
		rels := map[string]relSeen{}
		note := func(release, platform string, at time.Time) {
			if release == "" {
				return
			}
			cur, ok := rels[release]
			if !ok {
				rels[release] = relSeen{platform, at.UTC()}
				return
			}
			if cur.platform == "" {
				cur.platform = platform
			}
			if at.Before(cur.at) {
				cur.at = at.UTC()
			}
			rels[release] = cur
		}
		for _, pr := range preps {
			note(pr.ev.Release, pr.ev.Platform, pr.ev.Timestamp)
		}
		for _, s := range env.Sessions {
			note(s.Release, "", s.StartedAt)
		}
		if len(rels) > 0 {
			rp := sqlc.UpsertReleasesParams{ProjectID: p.ID}
			for r, seen := range rels {
				rp.Releases = append(rp.Releases, r)
				rp.Platforms = append(rp.Platforms, seen.platform)
				rp.FirstSeens = append(rp.FirstSeens, seen.at)
			}
			if err := q.UpsertReleases(ctx, rp); err != nil {
				return fmt.Errorf("upsert releases: %w", err)
			}
		}

		groups := map[sentry.ID][]*prepared{}
		var order []sentry.ID
		for _, pr := range preps {
			if pr.fingerprint == "" {
				pr.store = p.SampleRate >= 1 || mrand.Float64() < p.SampleRate
				continue
			}
			if _, seen := groups[pr.fingerprint]; !seen {
				order = append(order, pr.fingerprint)
			}
			groups[pr.fingerprint] = append(groups[pr.fingerprint], pr)
		}
		for _, fp := range order {
			g := groups[fp]
			first := g[0]
			var minAt, maxAt time.Time
			var lastRelease *string
			level := "error"
			seenRel := map[string]bool{}
			var releases []string
			for _, pr := range g {
				if !seenRel[pr.ev.Release] {
					seenRel[pr.ev.Release] = true
					releases = append(releases, pr.ev.Release)
				}
				at := pr.ev.Timestamp.UTC()
				if minAt.IsZero() || at.Before(minAt) {
					minAt = at
				}
				if !at.Before(maxAt) {
					maxAt = at
					lastRelease = nilIfEmpty(pr.ev.Release)
				}
				if pr.ev.Level == "fatal" {
					level = "fatal"
				}
			}
			// Sampling: keep the first N per issue (FatalKeepFactor × N
			// for crashes), then a fraction. When nothing can be sampled
			// out (sample_rate 1) the stored count is known up front and
			// goes into the upsert; otherwise the decision needs the
			// issue's count and is a second update.
			keepAll := p.SampleRate >= 1
			stored := int64(0)
			if keepAll {
				for _, pr := range g {
					pr.store = true
				}
				stored = int64(len(g))
			}
			row, err := q.UpsertIssue(ctx, sqlc.UpsertIssueParams{
				ProjectID: p.ID, Fingerprint: fp, Title: first.ev.IssueTitle(), Level: sqlc.EventLevel(level),
				ErrorType: nilIfEmpty(first.ev.ErrorType), Screen: nilIfEmpty(first.ev.Screen), Platform: nilIfEmpty(first.ev.Platform),
				EventCount: int64(len(g)), StoredCount: stored, FirstSeen: minAt, LastSeen: maxAt, FirstRelease: lastRelease,
				Releases: releases,
			})
			if err != nil {
				return fmt.Errorf("upsert issue: %w", err)
			}
			if row.Created {
				res.NewIssues = append(res.NewIssues, fp)
				jobs = append(jobs, alertJob(p.ID, "new_issue", fp))
			} else if row.Status == "regression" && row.UpdatedAt.After(now.Add(-time.Second)) {
				res.Regressions = append(res.Regressions, fp)
				jobs = append(jobs, alertJob(p.ID, "regression", fp))
			}
			if keepAll {
				continue
			}
			// The upsert held the row lock, so row.EventCount is the exact
			// sequence position of this group's events.
			prev := row.EventCount - int64(len(g))
			for i, pr := range g {
				seq := prev + int64(i) + 1
				keep := int64(p.SampleKeepFirst)
				if pr.ev.Level == "fatal" {
					keep *= FatalKeepFactor
				}
				pr.store = seq <= keep || mrand.Float64() < p.SampleRate
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
			tags, _ := json.Marshal(ev.Tags)
			var symbols json.RawMessage
			if pr.symbolicated {
				symbols, _ = json.Marshal(pr.frames)
			}
			rows = append(rows, store.EventInsert{
				OccurredAt: ev.Timestamp.UTC(), ProjectID: p.ID, EventID: ev.EventID, Level: ev.Level, Message: ev.Message,
				Platform: nilIfEmpty(ev.Platform), Environment: nilIfEmpty(ev.Environment), Release: nilIfEmpty(ev.Release),
				DeviceID: nilIfEmpty(ev.DeviceID()), DeviceModel: nilIfEmpty(ev.DeviceModel), OSVersion: nilIfEmpty(ev.OSVersion),
				Screen: nilIfEmpty(ev.Screen), ErrorType: nilIfEmpty(ev.ErrorType), ErrorLocation: nilIfEmpty(pr.location),
				Handled: ev.Handled, SDKName: nilIfEmpty(ev.SDKName), UserID: nilIfEmpty(ev.UserID),
				Fingerprint: pr.fingerprint.Ptr(), Symbolicated: pr.symbolicated,
				Tags: tags, Symbols: symbols, Payload: store.Gzip(ev.Raw),
			})
			if pr.retry && pr.fingerprint != "" {
				args, _ := json.Marshal(map[string]any{"event": ev.EventID, "at": ev.Timestamp})
				jobs = append(jobs, sqlc.EnqueueJobParams{Kind: "symbolicate", ProjectID: p.ID, Args: args, RunAfter: now})
			}
		}
		if err := store.InsertEvents(ctx, tx, rows); err != nil {
			return fmt.Errorf("insert events: %w", err)
		}
		res.Stored = len(rows)
		if len(rows) > 0 {
			hours := map[time.Time]bool{}
			var buckets []time.Time
			for _, r := range rows {
				h := r.OccurredAt.Truncate(time.Hour)
				if !hours[h] {
					hours[h] = true
					buckets = append(buckets, h)
				}
			}
			if err := q.MarkEventStatsDirty(ctx, sqlc.MarkEventStatsDirtyParams{ProjectID: p.ID, Buckets: buckets}); err != nil {
				return fmt.Errorf("mark stats: %w", err)
			}
		}

		sessions := make([]store.SessionInsert, 0, len(env.Sessions))
		for _, s := range env.Sessions {
			sessions = append(sessions, store.SessionInsert{
				StartedAt: s.StartedAt.UTC(), ProjectID: p.ID, Sid: sessionID(s), Release: s.Release, Environment: nilIfEmpty(s.Environment),
				Status: s.Status, Count: int32(max(s.Count, 1)),
			})
		}
		if err := store.InsertSessions(ctx, tx, sessions); err != nil {
			return fmt.Errorf("insert sessions: %w", err)
		}
		res.Sessions = len(sessions)
		if len(sessions) > 0 {
			hours := map[time.Time]bool{}
			var buckets []time.Time
			for _, s := range sessions {
				h := s.StartedAt.Truncate(time.Hour)
				if !hours[h] {
					hours[h] = true
					buckets = append(buckets, h)
				}
			}
			if err := q.MarkSessionStatsDirty(ctx, sqlc.MarkSessionStatsDirtyParams{ProjectID: p.ID, Buckets: buckets}); err != nil {
				return fmt.Errorf("mark stats: %w", err)
			}
		}
		if len(jobs) > 0 {
			jp := sqlc.EnqueueJobsParams{}
			for _, j := range jobs {
				jp.Kinds = append(jp.Kinds, string(j.Kind))
				jp.ProjectIds = append(jp.ProjectIds, j.ProjectID)
				jp.Args = append(jp.Args, j.Args)
				jp.RunAfters = append(jp.RunAfters, j.RunAfter)
			}
			if err := q.EnqueueJobs(ctx, jp); err != nil {
				return fmt.Errorf("enqueue jobs: %w", err)
			}
		}
		res.Jobs = len(jobs)
		return in.checkQuota(ctx, q, p, len(env.Events), now)
	})
	if err != nil {
		res = Result{Received: res.Received, Invalid: res.Invalid}
	}
	return res, err
}

// warnMismatch logs a platform mismatch at most once a minute per project:
// a wrong DSN in one app would otherwise write a line per event. The
// events are stored regardless; the viewer shows the same mismatch.
func (in *Ingester) warnMismatch(p sqlc.Project, ev *sentry.Event, family string) {
	in.mu.Lock()
	last, ok := in.warned[p.ID]
	if !ok || time.Since(last) > time.Minute {
		if in.warned == nil {
			in.warned = map[int64]time.Time{}
		}
		in.warned[p.ID] = time.Now()
		ok = false
	}
	in.mu.Unlock()
	if !ok {
		in.Log.Warn("ingest: platform mismatch", "project", p.Slug, "expected", *p.Platform, "got", family, "platform", ev.Platform, "sdk", ev.SDKName)
	}
}

// sessionID keys a session row: the SDK's sid (so status updates of one
// session land on the same row), or a random one for aggregate rows,
// which are counts and never updated.
func sessionID(s sentry.Session) string {
	if s.SID != "" {
		return s.SID
	}
	var b [16]byte
	rand.Read(b[:])
	return "agg-" + hex.EncodeToString(b[:])
}

func alertJob(projectID int64, typ string, fp sentry.ID) sqlc.EnqueueJobParams {
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
