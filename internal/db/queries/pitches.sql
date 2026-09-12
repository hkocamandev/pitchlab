-- name: InsertPitch :one
-- The idempotency boundary of the whole pipeline.
--
-- Kafka delivers at least once, so this insert will see the same pitch twice
-- whenever a consumer dies between doing its work and committing its offset.
-- DO NOTHING plus the unique constraint on external_pitch_uid makes that
-- harmless, and returning no row is how the caller learns it was a duplicate.
--
-- An application-level select-then-insert would race between processor
-- instances; the constraint is atomic regardless of concurrency.
INSERT INTO pitches (
    id, external_pitch_uid, session_id, pitcher_athlete_id, batter_athlete_id,
    at_bat_number, pitch_number, session_pitch_index, thrown_at,
    balls, strikes, outs_when_up, inning, inning_topbot,
    stand, p_throws, on_1b, on_2b, on_3b,
    bat_score, fld_score, n_thruorder_pitcher,
    pitch_type, pitch_name, actual_outcome, prediction_status,
    correlation_id, source_event_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28
)
ON CONFLICT (external_pitch_uid) DO NOTHING
RETURNING *;

-- name: GetPitchByExternalUID :one
SELECT * FROM pitches WHERE external_pitch_uid = $1;

-- name: GetPitch :one
SELECT * FROM pitches WHERE id = $1;

-- name: NextSessionPitchIndex :one
-- Sequence number within the outing. Safe because a session maps to one
-- Kafka partition and a partition is consumed by a single goroutine, so
-- writes for one session are serialized by construction.
SELECT COALESCE(max(session_pitch_index), 0) + 1 AS next_index
FROM pitches WHERE session_id = $1;

-- name: SetPitchPredictionStatus :exec
UPDATE pitches SET prediction_status = $2 WHERE id = $1;

-- name: ListPitchesBySession :many
-- The session timeline, and the opening snapshot for a live WebSocket
-- connection. Served by ix_pitches_session_seq.
SELECT
    sqlc.embed(p),
    sqlc.embed(m)
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
WHERE p.session_id = $1
  AND (sqlc.narg('cursor_index')::int IS NULL
       OR p.session_pitch_index > sqlc.narg('cursor_index')::int)
ORDER BY p.session_pitch_index
LIMIT $2;

-- name: ListPitchesBySessionDesc :many
SELECT
    sqlc.embed(p),
    sqlc.embed(m)
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
WHERE p.session_id = $1
  AND (sqlc.narg('cursor_index')::int IS NULL
       OR p.session_pitch_index < sqlc.narg('cursor_index')::int)
ORDER BY p.session_pitch_index DESC
LIMIT $2;

-- name: ListPitchesByPitcher :many
-- pitcher_athlete_id is required by the API, not optional: without it this
-- becomes a sequential scan over millions of rows. Cheaper to make the
-- expensive query impossible than to optimize it later.
SELECT
    sqlc.embed(p),
    sqlc.embed(m)
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
WHERE p.pitcher_athlete_id = $1
  AND (sqlc.narg('pitch_type')::text IS NULL OR p.pitch_type = sqlc.narg('pitch_type'))
  AND (sqlc.narg('outcome')::text IS NULL OR p.actual_outcome = sqlc.narg('outcome'))
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR p.thrown_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR p.thrown_at <= sqlc.narg('to_ts'))
  AND (sqlc.narg('min_speed')::real IS NULL OR m.release_speed >= sqlc.narg('min_speed'))
  AND (sqlc.narg('max_speed')::real IS NULL OR m.release_speed <= sqlc.narg('max_speed'))
  AND (sqlc.narg('cursor_thrown')::timestamptz IS NULL
       OR (p.thrown_at, p.id) < (sqlc.narg('cursor_thrown')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY p.thrown_at DESC, p.id DESC
LIMIT $2;

-- name: ListFailedPredictionPitches :many
-- Backfill after an ML outage. Served by the partial index
-- ix_pitches_pred_failed, which is negligible in size because these rows are
-- expected to be a fraction of a percent.
SELECT * FROM pitches
WHERE prediction_status = 'FAILED'
ORDER BY thrown_at
LIMIT $1;

-- name: FindPitchByCorrelationID :many
-- Tracing support: resolve a correlation id seen in logs back to the pitch.
SELECT * FROM pitches WHERE correlation_id = $1 ORDER BY session_pitch_index;
