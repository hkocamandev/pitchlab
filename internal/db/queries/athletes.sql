-- name: UpsertAthlete :one
-- Idempotent by mlbam_id. The event stream re-announces the same athlete on
-- every pitch, so this must converge rather than accumulate. Name and
-- handedness are refreshed; created_at is not touched.
INSERT INTO athletes (
    id, mlbam_id, full_name, throws, bats, primary_role, birth_date
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (mlbam_id) DO UPDATE SET
    full_name    = EXCLUDED.full_name,
    throws       = COALESCE(EXCLUDED.throws, athletes.throws),
    bats         = COALESCE(EXCLUDED.bats, athletes.bats),
    primary_role = CASE
        WHEN athletes.primary_role = 'UNKNOWN' THEN EXCLUDED.primary_role
        WHEN athletes.primary_role <> EXCLUDED.primary_role
             AND EXCLUDED.primary_role <> 'UNKNOWN' THEN 'TWO_WAY'
        ELSE athletes.primary_role
    END,
    birth_date   = COALESCE(EXCLUDED.birth_date, athletes.birth_date),
    updated_at   = now()
RETURNING *;

-- name: GetAthlete :one
SELECT * FROM athletes WHERE id = $1;

-- name: GetAthleteByMLBAMID :one
SELECT * FROM athletes WHERE mlbam_id = $1;

-- name: ListAthletes :many
-- Keyset pagination on (full_name, id): offsets shift as rows are inserted,
-- which would repeat or skip records in a continuously growing table.
SELECT a.*,
       (SELECT count(*) FROM sessions s WHERE s.pitcher_athlete_id = a.id) AS session_count,
       (SELECT count(*) FROM pitches p WHERE p.pitcher_athlete_id = a.id)  AS pitch_count
FROM athletes a
WHERE (sqlc.narg('search')::text IS NULL OR a.full_name ILIKE '%' || sqlc.narg('search') || '%')
  AND (sqlc.narg('role')::text IS NULL OR a.primary_role = sqlc.narg('role'))
  AND (sqlc.narg('throws')::text IS NULL OR a.throws = sqlc.narg('throws'))
  AND (
      sqlc.narg('cursor_name')::text IS NULL
      OR (a.full_name, a.id) > (sqlc.narg('cursor_name')::text, sqlc.narg('cursor_id')::uuid)
  )
ORDER BY a.full_name, a.id
LIMIT $1;

-- name: GetAthleteTotals :one
-- first_pitch_at and last_pitch_at are genuinely NULL for an athlete with no
-- pitches. They are left uncast: an explicit ::timestamptz makes sqlc emit a
-- non-pointer time.Time, which then fails at scan time on exactly the rows
-- that need handling. The interface{} sqlc infers is converted in the handler.
SELECT
    (SELECT count(*) FROM sessions s WHERE s.pitcher_athlete_id = $1)::bigint AS session_count,
    (SELECT count(*) FROM pitches p  WHERE p.pitcher_athlete_id = $1)::bigint AS pitch_count,
    (SELECT min(p.thrown_at) FROM pitches p WHERE p.pitcher_athlete_id = $1) AS first_pitch_at,
    (SELECT max(p.thrown_at) FROM pitches p WHERE p.pitcher_athlete_id = $1) AS last_pitch_at;
