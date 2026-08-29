-- name: UpsertReleases :exec
-- Records the releases an envelope mentions. The conflict update only
-- fires when it would change something (a new platform, an earlier
-- first_seen), so the usual envelope leaves the row untouched.
INSERT INTO releases (project_id, release, platforms, first_seen)
SELECT sqlc.arg(project_id)::bigint, u.r, CASE WHEN u.p = '' THEN '{}'::text[] ELSE ARRAY[u.p] END, u.t
FROM (SELECT unnest(sqlc.arg(releases)::text[]) AS r, unnest(sqlc.arg(platforms)::text[]) AS p,
             unnest(sqlc.arg(first_seens)::timestamptz[]) AS t) AS u
ON CONFLICT (project_id, release) DO UPDATE SET
    platforms  = (SELECT array_agg(DISTINCT x ORDER BY x) FROM unnest(releases.platforms || EXCLUDED.platforms) AS x),
    first_seen = LEAST(releases.first_seen, EXCLUDED.first_seen)
WHERE NOT releases.platforms @> EXCLUDED.platforms OR EXCLUDED.first_seen < releases.first_seen;

-- name: ListReleases :many
-- Newest first (by first appearance).
SELECT * FROM releases WHERE project_id = $1 ORDER BY first_seen DESC, release DESC LIMIT $2;
