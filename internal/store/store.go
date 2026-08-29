// Package store is the data-access layer: a pgx pool plus the sqlc-generated
// queries, the few dynamic queries sqlc cannot express (event listing with
// optional filters, tag breakdowns), and the object store for the bytes
// that are not in Postgres (event payloads, symbol files).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crashcartapp/crashcart/internal/blob"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// Store wraps the pool. Queries are usable directly (auto-commit) or via Tx.
type Store struct {
	Pool  *pgxpool.Pool
	Blobs blob.Store
	*sqlc.Queries
}

// New builds a Store on an open pool and an object store.
func New(pool *pgxpool.Pool, blobs blob.Store) *Store {
	return &Store{Pool: pool, Blobs: blobs, Queries: sqlc.New(pool)}
}

// Payload reads an event's raw payload: from payload_spool while its pack
// is still being filled, from the pack in the object store once uploaded.
// nil, nil when the event has none (its pack expired, or it was imported
// without one).
func (s *Store) Payload(ctx context.Context, e sqlc.Event) ([]byte, error) {
	if e.PackID == nil || e.PackOffset == nil || e.PackLen == nil {
		return nil, nil
	}
	b, err := s.SpooledPayload(ctx, sqlc.SpooledPayloadParams{PackID: *e.PackID, Offset: *e.PackOffset})
	if err == nil {
		return blob.Gunzip(b)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	b, err = blob.ReadPayload(ctx, s.Blobs, *e.PackID, int64(*e.PackOffset), int64(*e.PackLen))
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// PayloadPlace is where a payload sits: the event row's pack_id,
// pack_offset and pack_len.
type PayloadPlace struct {
	PackID int64
	Offset int32
	Len    int32
}

// SpoolPayloads reserves room for gzipped payloads in a pack and writes
// them to the spool, inside the caller's transaction: it claims the
// fullest open pack no other transaction holds (or opens one), advances
// its counter by the total, and returns each payload's place for its
// event row. The pack's row lock is held until commit, so concurrent
// envelopes use different packs; a rollback returns the bytes.
func SpoolPayloads(ctx context.Context, q *sqlc.Queries, payloads [][]byte) ([]PayloadPlace, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	var total int64
	for _, p := range payloads {
		total += int64(len(p))
	}
	pack, err := q.ClaimOpenPack(ctx)
	id := pack.ID
	if errors.Is(err, pgx.ErrNoRows) {
		var opened sqlc.OpenPackRow
		opened, err = q.OpenPack(ctx)
		id = opened.ID
	}
	if err != nil {
		return nil, fmt.Errorf("claim pack: %w", err)
	}
	adv, err := q.AdvancePack(ctx, sqlc.AdvancePackParams{ID: id, N: total, MaxBytes: blob.PackBytes})
	if err != nil {
		return nil, fmt.Errorf("advance pack: %w", err)
	}
	places := make([]PayloadPlace, len(payloads))
	sp := sqlc.SpoolPayloadsParams{PackIds: make([]int64, len(payloads)), Offsets: make([]int32, len(payloads)), Datas: payloads}
	off := adv.Off
	for i, p := range payloads {
		places[i] = PayloadPlace{PackID: id, Offset: int32(off), Len: int32(len(p))}
		sp.PackIds[i], sp.Offsets[i] = id, int32(off)
		off += int64(len(p))
	}
	if err := q.SpoolPayloads(ctx, sp); err != nil {
		return nil, fmt.Errorf("spool payloads: %w", err)
	}
	return places, nil
}

// Tx runs fn inside a transaction with a transaction-scoped Queries.
func (s *Store) Tx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx, q *sqlc.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(ctx, tx, s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Leader keys for RunAsLeader (pg advisory lock ids).
const (
	LeaderSpikeCheck int64 = 0x63726173 + 1 // "cras" + n
	LeaderSweep      int64 = 0x63726173 + 2
	LeaderRollup     int64 = 0x63726173 + 3
)

// RunAsLeader runs fn while holding the session advisory lock key, and
// reports false without running it when another replica holds the lock —
// so scheduled work (sweeps, spike checks) runs once per
// deployment, not once per replica.
func (s *Store) RunAsLeader(ctx context.Context, key int64, fn func()) (bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	fn()
	return true, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }
