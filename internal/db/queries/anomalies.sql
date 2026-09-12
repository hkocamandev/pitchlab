-- name: InsertAnomaly :one
-- Idempotent via the unique index on
-- (session_id, metric, COALESCE(pitch_type, ''), detector_version).
-- COALESCE is not cosmetic: NULL <> NULL in a plain unique constraint, so
-- without it the rows that most need protection -- those with no pitch type --
-- would duplicate freely on a re-consumed event.
INSERT INTO performance_anomalies (
    id, athlete_id, session_id, metric, pitch_type,
    observed_value, baseline_median, robust_sigma, z_robust,
    direction, severity, window_sessions, detector_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (session_id, metric, COALESCE(pitch_type, ''), detector_version)
DO NOTHING
RETURNING *;

-- name: GetAnomaly :one
SELECT * FROM performance_anomalies WHERE id = $1;

-- name: ListAnomaliesByAthlete :many
SELECT * FROM performance_anomalies
WHERE athlete_id = $1
  AND (sqlc.narg('severity')::text IS NULL OR severity = sqlc.narg('severity'))
  AND (sqlc.narg('metric')::text IS NULL OR metric = sqlc.narg('metric'))
  AND (
      sqlc.narg('acknowledged')::bool IS NULL
      OR (sqlc.narg('acknowledged')::bool AND acknowledged_at IS NOT NULL)
      OR (NOT sqlc.narg('acknowledged')::bool AND acknowledged_at IS NULL)
  )
  AND (sqlc.narg('cursor_detected')::timestamptz IS NULL
       OR (detected_at, id) < (sqlc.narg('cursor_detected')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY detected_at DESC, id DESC
LIMIT $2;

-- name: ListAnomaliesBySession :many
SELECT * FROM performance_anomalies
WHERE session_id = $1
ORDER BY detected_at DESC;

-- name: CountOpenAnomalies :one
-- Served by the partial index ix_anomaly_open.
SELECT count(*) FROM performance_anomalies
WHERE athlete_id = $1 AND acknowledged_at IS NULL;

-- name: AcknowledgeAnomaly :one
-- Idempotent: a second call returns no row, which is how the handler
-- distinguishes "already acknowledged" from "not found".
UPDATE performance_anomalies
SET acknowledged_at = now()
WHERE id = $1 AND acknowledged_at IS NULL
RETURNING *;

-- name: GetAnomalyBaselineWindow :many
-- Trailing per-session medians for one pitcher and pitch type, newest first.
--
-- This feeds the robust z-score: median and MAD over the previous N sessions.
-- Median rather than mean, and MAD rather than standard deviation, because a
-- single injury outing inside the window would otherwise inflate sigma and
-- mask every later real anomaly.
SELECT
    s.id                                    AS session_id,
    s.started_at,
    count(*)                                AS pitch_count,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_speed)     AS median_release_speed,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_spin_rate) AS median_release_spin_rate,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_pos_x_arm) AS median_release_pos_x_arm,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_pos_z)     AS median_release_pos_z,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_extension) AS median_release_extension,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.pfx_x_arm)         AS median_pfx_x_arm,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.pfx_z)             AS median_pfx_z
FROM sessions s
JOIN pitches p            ON p.session_id = s.id
JOIN pitch_measurements m ON m.pitch_id = p.id
WHERE s.pitcher_athlete_id = $1
  AND p.pitch_type = $2
  AND s.started_at < sqlc.arg('before')::timestamptz
  AND m.measurement_quality <> 'SUSPECT'
GROUP BY s.id, s.started_at
-- Thin outings are a noisy baseline; better to have fewer, sounder points.
HAVING count(*) >= sqlc.arg('min_pitches')::int
ORDER BY s.started_at DESC
LIMIT sqlc.arg('window_size')::int;

-- name: GetSessionMetricMedians :one
-- The session under evaluation, shaped like one row of the baseline window.
SELECT
    count(*)                                AS pitch_count,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_speed)     AS median_release_speed,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_spin_rate) AS median_release_spin_rate,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_pos_x_arm) AS median_release_pos_x_arm,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_pos_z)     AS median_release_pos_z,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.release_extension) AS median_release_extension,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.pfx_x_arm)         AS median_pfx_x_arm,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY m.pfx_z)             AS median_pfx_z
FROM pitches p
JOIN pitch_measurements m ON m.pitch_id = p.id
WHERE p.session_id = $1
  AND p.pitch_type = $2
  AND m.measurement_quality <> 'SUSPECT';
