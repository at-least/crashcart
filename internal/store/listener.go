package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crashcartapp/crashcart/internal/metrics"

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
		started := time.Now()
		err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it was healthy for a while: not a reconnect storm
		}
		ListenerReconnects.Inc()
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
		// A LISTEN connection is idle for minutes at a time: a NAT or
		// proxy dropping it would never be noticed (no FIN reaches us,
		// nothing is ever written). Wait in bounded slices and ping in
		// between, which both detects a dead socket and keeps it alive.
		wait, cancel := context.WithTimeout(ctx, ListenKeepalive)
		n, err := conn.WaitForNotification(wait)
		cancel()
		if err != nil {
			if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if err := conn.Ping(ctx); err != nil {
				return fmt.Errorf("keepalive: %w", err)
			}
			continue
		}
		l.deliver(n.Channel, n.Payload)
	}
}

// ListenerReconnects counts LISTEN connection losses (a rising count
// means something between the process and Postgres drops idle sockets).
var ListenerReconnects = metrics.NewCounter("crashcart_listener_reconnects_total", "LISTEN connection losses followed by a reconnect.")

// ListenKeepalive is how long WaitForNotification blocks before the
// connection is pinged.
var ListenKeepalive = 45 * time.Second

func (l *Listener) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}
