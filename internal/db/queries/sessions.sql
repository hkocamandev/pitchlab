-- name: UpsertSession :one
-- Idempotent by external_session_uid. Replaying the same outing twice must
-- reuse the session rather than create a second one.
INSERT INTO sessions (
    id, external_session_uid, pitcher_athlete_id, device_id,
    external_game_ref, source_game_date, opponent_team,
    started_at, status, is_synthetic
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
ON CONFLICT (external_session_uid) DO UPDATE SET
    device_id     = COALESCE(EXCLUDED.device_id, sessions.device_id),
    opponent_team = COALESCE(EXCLUDED.opponent_team, sessions.opponent_team),
    updated_at    = now()
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: GetSessionByExternalUID :one
SELECT * FROM sessions WHERE external_session_uid = $1;

-- name: ListSessionsByAthlete :many
-- Served by ix_sessions_pitcher_started: the composite index covers both the
-- filter and the ordering, so there is no sort step.
SELECT * FROM sessions
WHERE pitcher_athlete_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('cursor_started')::timestamptz IS NULL
       OR (started_at, id) < (sqlc.narg('cursor_started')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY started_at DESC, id DESC
LIMIT $2;

-- name: ListActiveSessions :many
-- Served by the partial index ix_sessions_status_started, which stays tiny
-- because active sessions are a negligible fraction of the table.
SELECT * FROM sessions
WHERE status = 'ACTIVE'
ORDER BY started_at DESC
LIMIT $1;

-- name: RecomputeSessionAggregates :one
-- Refreshes the denormalized counters from the underlying rows.
--
-- The averages on `sessions` exist so the session-list page does not run one
-- aggregate per row. Recomputing from source (rather than incrementing) means
-- a replayed or repaired session converges on the truth instead of drifting,
-- and pitch_count cross-checks the rest.
UPDATE sessions s SET
    pitch_count           = agg.n,
    avg_release_speed     = agg.avg_speed,
    avg_release_spin_rate = agg.avg_spin,
    avg_pitch_score       = agg.avg_score,
    updated_at            = now()
FROM (
    SELECT
        count(*)                    AS n,
        avg(m.release_speed)        AS avg_speed,
        avg(m.release_spin_rate)    AS avg_spin,
        avg(pr.score)               AS avg_score
    FROM pitches p
    LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
    LEFT JOIN pitch_predictions pr ON pr.pitch_id = p.id
         AND pr.model_version_id = (
             SELECT id FROM model_versions
             WHERE variant = 'pitching' AND is_active LIMIT 1
         )
    WHERE p.session_id = $1
) AS agg
WHERE s.id = $1
RETURNING s.*;

-- name: CloseSession :one
-- Terminal transition. The WHERE clause makes it idempotent and makes a
-- second attempt distinguishable: no row returned means it was already closed.
UPDATE sessions
SET status = 'COMPLETED', ended_at = COALESCE(ended_at, now()), updated_at = now()
WHERE id = $1 AND status = 'ACTIVE'
RETURNING *;

-- name: GetSessionOutcomeDistribution :many
SELECT actual_outcome, count(*) AS n
FROM pitches
WHERE session_id = $1 AND actual_outcome IS NOT NULL
GROUP BY actual_outcome;

-- name: GetSessionLiveMetrics :one
-- Rebuilds an outing's running counters from the rows themselves.
--
-- This is the fallback for the Redis live-state key, and it is also the
-- argument for that key existing: these numbers change once per pitch, and
-- updating a session row that often would mean row-lock contention and
-- constant vacuuming. The cache is only ever an accelerator, and this query
-- is the proof -- every value it holds is recomputable here.
--
-- Aggregates are wrapped in COALESCE and cast explicitly for the same reason
-- as in analytics.sql: sqlc infers a bare aggregate as non-nullable and then
-- panics on the NULL an empty group returns. The paired count columns are how
-- a caller tells a real zero from an empty one.
SELECT
    count(*)::bigint                                                     AS pitch_count,
    count(*) FILTER (WHERE p.actual_outcome = 'SWINGING_STRIKE')::bigint AS whiff_count,
    COALESCE(avg(m.release_speed), 0)::float8                            AS avg_release_speed,
    count(m.release_speed)::bigint                                       AS speed_count,
    COALESCE(avg(m.release_spin_rate), 0)::float8                        AS avg_release_spin_rate,
    count(m.release_spin_rate)::bigint                                   AS spin_count,
    COALESCE(avg(pr.score), 0)::float8                                   AS avg_pitch_score,
    count(pr.score)::bigint                                              AS score_count
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
LEFT JOIN pitch_predictions pr ON pr.pitch_id = p.id
     AND pr.model_version_id = (
         SELECT id FROM model_versions
         WHERE variant = 'pitching' AND is_active LIMIT 1
     )
WHERE p.session_id = $1;
