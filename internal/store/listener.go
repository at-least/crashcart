package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notification channels (pg_notify names raised by the schema's triggers).
const (
	ChannelJobs   = "crashcart_jobs"   // payload: ""              — a job was queued
	ChannelIssues = "crashcart_issues" // payload: project id text — an issue was created or regressed
)

// Listener holds one dedicated connection in LISTEN on the channels and
// fans notifications out to subscribers. It reconnects with backoff, so a
// database restart only costs a few missed wake-ups — every consumer keeps
// a slow poll as its fallback.
type Listener struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger

	mu   sync.Mutex
	subs map[*subscription]struct{}
}

type subscription struct {
	channel, key string
	ch           chan string
}

// Subscribe returns a channel that receives the payload of every
// notification on channel whose payload equals key ("" = any). Deliveries
// coalesce: the channel is buffered by one and a pending wake-up is not
// duplicated. Call stop to unsubscribe.
func (l *Listener) Subscribe(channel, key string) (<-chan string, func()) {
	s := &subscription{channel: channel, key: key, ch: make(chan string, 1)}
	l.mu.Lock()
	if l.subs == nil {
		l.subs = map[*subscription]struct{}{}
	}
	l.subs[s] = struct{}{}
	l.mu.Unlock()
	return s.ch, func() {
		l.mu.Lock()
		delete(l.subs, s)
		l.mu.Unlock()
	}
}

func (l *Listener) deliver(channel, payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs {
		if s.channel != channel || (s.key != "" && s.key != payload) {
			continue
		}
		select {
		case s.ch <- payload:
		default: // a wake-up is already pending
		}
	}
}

// Run blocks until ctx is done, keeping a LISTEN connection open.
func (l *Listener) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		l.log().Warn("notify: listener disconnected, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (l *Listener) listen(ctx context.Context) error {
	pc, err := l.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pc.Hijack() // ours until closed: a LISTEN connection cannot go back to the pool
	defer conn.Close(context.WithoutCancel(ctx))
	for _, ch := range []string{ChannelJobs, ChannelIssues} {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return err
		}
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		l.deliver(n.Channel, n.Payload)
	}
}

func (l *Listener) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}
