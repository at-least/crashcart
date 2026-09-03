package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/blob"
)

// Event payloads in the blob store (BLOB_STORE=s3). Ingest writes the
// gzipped payload into payload_spool in the same transaction as the event
// row (InsertEvents), so it is durable in Postgres before any object
// exists and the object store being down only means a growing spool. Pack
// then moves the spool into objects — one per project and week, closed at
// PackBytes or after PackAge so a quiet project still flushes — recording
// each event's place in event_packs. A payload is read from the column,
// else the spool, else its pack by a ranged GET (Payload). A week's packs
// are deleted when its partition is (internal/retention), by the packs
// table, never by bucket lifecycle rules.
const (
	// PackBytes closes a pack: one PUT per PackBytes whatever the events'
	// size — PUT requests, not bytes, are what an object-store bill is made
	// of. A single payload larger than this is a pack of its own.
	PackBytes = 8 << 20
	// PackAge closes a pack that has not reached PackBytes: minutes, not
	// seconds, so a quiet project does not pay one PUT per event — its
	// payloads wait in the spool (served from there meanwhile) until a
	// pack is worth writing or this long has passed.
	PackAge = 5 * time.Minute
	// packRows bounds one flush read; at 30 KB a payload it is well past a
	// PackBytes pack.
	packRows = 4096
)

// Payload is an event's raw payload, decoded: the payload column, else the
// spool row, else its place in a pack — one statement for the last two, so
// an event packed between two lookups cannot read as one without a
// payload. nil, nil when the event has none (imported without one, or its
// pack expired with its week under us). db is what to read the location
// with (an export passes its snapshot transaction); nil = the pool.
func (s *Store) Payload(ctx context.Context, db DB, e Event) ([]byte, error) {
	return s.payload(ctx, db, e, nil)
}

func (s *Store) payload(ctx context.Context, db DB, e Event, cache *PackReader) ([]byte, error) {
	if len(e.Payload) > 0 {
		return Gunzip(e.Payload)
	}
	if db == nil {
		db = s.Pool
	}
	loc, err := PayloadLocation(ctx, db, e.ProjectID, e.EventID, e.OccurredAt)
	if err != nil {
		return nil, err
	}
	switch {
	case len(loc.Spooled) > 0:
		return Gunzip(loc.Spooled)
	case loc.PackID == nil:
		return nil, nil
	}
	if s.Blobs == nil {
		return nil, fmt.Errorf("event %s is packed in the blob store, but BLOB_STORE is not configured", e.EventID)
	}
	key := blob.PackKey(e.ProjectID, *loc.Week, *loc.PackID)
	off, n := int64(*loc.PackOffset), int64(*loc.PackLen)
	var gz []byte
	if cache != nil {
		gz, err = cache.slice(ctx, s.Blobs, *loc.PackID, key, off, n)
	} else {
		gz, err = s.Blobs.GetRange(ctx, key, off, n)
	}
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil // the week expired between the row read and now
	}
	if err != nil {
		return nil, err
	}
	return Gunzip(gz)
}

// PackReader reads payloads with a small cache of whole packs — for a
// reader that walks events in their pack order (an export streams a
// project's events by occurred_at, which is the order packs are filled
// in), so a million events cost about one GET per PackBytes, not one each.
type PackReader struct {
	s     *Store
	packs map[int64][]byte
	order []int64 // least recently added first
}

const packReaderCap = 8

// NewPackReader is a reader over s.
func (s *Store) NewPackReader() *PackReader {
	return &PackReader{s: s, packs: map[int64][]byte{}}
}

// Payload is Store.Payload through the cache.
func (r *PackReader) Payload(ctx context.Context, db DB, e Event) ([]byte, error) {
	return r.s.payload(ctx, db, e, r)
}

func (r *PackReader) slice(ctx context.Context, store blob.Store, id int64, key string, off, n int64) ([]byte, error) {
	data, ok := r.packs[id]
	if !ok {
		var err error
		if data, err = store.Get(ctx, key); err != nil {
			return nil, err
		}
		if len(r.order) == packReaderCap {
			delete(r.packs, r.order[0])
			r.order = r.order[1:]
		}
		r.packs[id] = data
		r.order = append(r.order, id)
	}
	if off < 0 || off+n > int64(len(data)) {
		return nil, fmt.Errorf("pack %d: range %d+%d outside %d bytes", id, off, n, len(data))
	}
	return data[off : off+n], nil
}

// Pack moves spooled payloads into packs: every (project, week) group at
// PackBytes or older than PackAge, again until none is. Nothing to do
// without a store. Returns the events packed.
func (s *Store) Pack(ctx context.Context, now time.Time) (int, error) {
	return s.pack(ctx, now, false)
}

// Drain packs everything spooled regardless of age — after an import, so
// the command is complete when it returns.
func (s *Store) Drain(ctx context.Context) (int, error) {
	return s.pack(ctx, time.Now(), true)
}

func (s *Store) pack(ctx context.Context, now time.Time, force bool) (int, error) {
	if s.Blobs == nil {
		return 0, nil
	}
	packed := 0
	for {
		groups, err := SpoolGroups(ctx, s.Pool)
		if err != nil {
			return packed, err
		}
		did := false
		for _, g := range groups {
			if !force && g.Bytes < PackBytes && now.Sub(g.Oldest) < PackAge {
				continue
			}
			n, err := s.packOne(ctx, g)
			if err != nil {
				return packed, err
			}
			packed += n
			did = did || n > 0
		}
		if !did {
			return packed, nil
		}
	}
}

// packOne builds one pack for a group. The packs row comes first (its
// id names the object), the object is written, and only then are the
// event_packs rows written and the spool rows deleted, in one
// transaction — so a crash at any point leaves either the spool rows (the
// next run repacks them) or a complete pack, never a row pointing at
// bytes that were not written. An orphaned object is deleted best-effort
// here and by the week sweep otherwise.
func (s *Store) packOne(ctx context.Context, g SpoolGroupsRow) (int, error) {
	week := g.Week.UTC()
	rows, err := SpoolRows(ctx, s.Pool, g.ProjectID, week, week.Add(7*24*time.Hour), packRows)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	var buf []byte
	var places []InsertEventPackParams
	var keys []SpoolKey
	for _, r := range rows {
		if len(buf) > 0 && len(buf)+len(r.Data) > PackBytes {
			break // the pack is full; a lone oversized payload still goes in
		}
		places = append(places, InsertEventPackParams{
			ProjectID: r.ProjectID, EventID: r.EventID, OccurredAt: r.OccurredAt,
			PackOffset: int32(len(buf)), PackLen: int32(len(r.Data)),
		})
		keys = append(keys, SpoolKey{ProjectID: r.ProjectID, EventID: r.EventID, OccurredAt: r.OccurredAt})
		buf = append(buf, r.Data...)
	}
	id, err := InsertPack(ctx, s.Pool, g.ProjectID, week)
	if err != nil {
		return 0, err
	}
	for i := range places {
		places[i].PackID = id
	}
	key := blob.PackKey(g.ProjectID, week, id)
	if err := s.Blobs.Put(ctx, key, buf); err != nil {
		DeletePack(context.WithoutCancel(ctx), s.Pool, id)
		return 0, fmt.Errorf("blob store: %w", err)
	}
	err = s.Tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := InsertEventPacks(ctx, tx, places); err != nil {
			return err
		}
		if err := DeleteSpoolRows(ctx, tx, keys); err != nil {
			return err
		}
		return SetPackBytes(ctx, tx, id, int64(len(buf)))
	})
	if err != nil {
		bg := context.WithoutCancel(ctx)
		s.Blobs.Delete(bg, key)
		DeletePack(bg, s.Pool, id)
		return 0, err
	}
	return len(places), nil
}

// ExpirePacks deletes the packs (objects, then rows) of weeks past cutoff
// — the partition drop's rule — and spool rows past it. Best effort on
// the objects: a failure is returned after the rest, and the next sweep
// tries again, since the rows are only deleted once the object is.
func (s *Store) ExpirePacks(ctx context.Context, cutoff time.Time) (packs int, err error) {
	expired, err := ExpiredPacks(ctx, s.Pool, cutoff)
	if err != nil {
		return 0, err
	}
	var first error
	for _, p := range expired {
		if s.Blobs == nil {
			if first == nil {
				first = fmt.Errorf("pack %d expired but BLOB_STORE is not configured; object left behind", p.ID)
			}
			continue
		}
		if err := s.Blobs.Delete(ctx, blob.PackKey(p.ProjectID, p.Week, p.ID)); err != nil {
			if first == nil {
				first = fmt.Errorf("delete pack %d: %w", p.ID, err)
			}
			continue
		}
		if err := DeletePack(ctx, s.Pool, p.ID); err != nil {
			return packs, err
		}
		packs++
	}
	if _, err := ExpireSpool(ctx, s.Pool, cutoff); err != nil {
		return packs, err
	}
	return packs, first
}

// DeleteProjectPacks deletes the objects of a project's packs; call with
// the keys read before the project (and, by cascade, the rows) is deleted.
func (s *Store) DeleteProjectPacks(ctx context.Context, projectID int64, packs []ProjectPacksRow) error {
	var first error
	for _, p := range packs {
		if s.Blobs == nil {
			if first == nil {
				first = fmt.Errorf("pack %d: BLOB_STORE is not configured; object left behind", p.ID)
			}
			continue
		}
		if err := s.Blobs.Delete(ctx, blob.PackKey(projectID, p.Week, p.ID)); err != nil && first == nil {
			first = fmt.Errorf("delete pack %d: %w", p.ID, err)
		}
	}
	return first
}
