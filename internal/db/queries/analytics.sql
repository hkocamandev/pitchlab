-- name: GetAthleteSummary :one
-- The KPI strip on the athlete overview page. Expensive enough to be worth
-- caching in Redis; correct enough to be the fallback when the cache is cold
-- or unavailable.
SELECT
    count(*)                                       AS pitch_count,
    count(DISTINCT p.session_id)                   AS session_count,
    avg(m.release_speed)                           AS avg_release_speed,
    avg(m.release_spin_rate)                       AS avg_release_spin_rate,
    avg(m.release_extension)                       AS avg_release_extension,
    avg(pr.score)                                  AS avg_pitch_score,
    -- Whiff rate has swings in the denominator, not pitches: it measures how
    -- often a batter misses when he offers.
    sum(CASE WHEN p.actual_outcome = 'SWINGING_STRIKE' THEN 1 ELSE 0 END)::float
        / NULLIF(sum(CASE WHEN p.actual_outcome IN
            ('SWINGING_STRIKE', 'FOUL', 'IN_PLAY') THEN 1 ELSE 0 END), 0)
                                                   AS whiff_rate,
    sum(CASE WHEN p.actual_outcome = 'CALLED_STRIKE' THEN 1 ELSE 0 END)::float
        / NULLIF(count(*), 0)                      AS called_strike_rate,
    sum(CASE WHEN m.in_zone THEN 1 ELSE 0 END)::float
        / NULLIF(count(m.in_zone), 0)              AS in_zone_rate
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
LEFT JOIN pitch_predictions pr ON pr.pitch_id = p.id
     AND pr.model_version_id = (
         SELECT id FROM model_versions WHERE variant = 'pitching' AND is_active LIMIT 1
     )
WHERE p.pitcher_athlete_id = $1
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR p.thrown_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR p.thrown_at <= sqlc.narg('to_ts'));

-- name: GetAthletePitchTypeDistribution :many
-- Arsenal breakdown. Served by ix_pitches_pitcher_type.
SELECT
    p.pitch_type,
    max(p.pitch_name)                AS pitch_name,
    count(*)                         AS n,
    avg(m.release_speed)             AS avg_release_speed,
    avg(m.release_spin_rate)         AS avg_release_spin_rate,
    avg(m.pfx_x_arm)                 AS avg_pfx_x_arm,
    avg(m.pfx_z)                     AS avg_pfx_z,
    avg(pr.score)                    AS avg_pitch_score,
    sum(CASE WHEN p.actual_outcome = 'SWINGING_STRIKE' THEN 1 ELSE 0 END)::float
        / NULLIF(sum(CASE WHEN p.actual_outcome IN
            ('SWINGING_STRIKE', 'FOUL', 'IN_PLAY') THEN 1 ELSE 0 END), 0) AS whiff_rate
FROM pitches p
LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
LEFT JOIN pitch_predictions pr ON pr.pitch_id = p.id
     AND pr.model_version_id = (
         SELECT id FROM model_versions WHERE variant = 'pitching' AND is_active LIMIT 1
     )
WHERE p.pitcher_athlete_id = $1
  AND p.pitch_type IS NOT NULL
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR p.thrown_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR p.thrown_at <= sqlc.narg('to_ts'))
GROUP BY p.pitch_type
ORDER BY n DESC;

-- name: GetAthleteSessionTrend :many
-- Per-outing series behind the performance trend chart.
SELECT
    s.id                     AS session_id,
    s.source_game_date,
    s.started_at,
    s.pitch_count,
    s.avg_release_speed,
    s.avg_release_spin_rate,
    s.avg_pitch_score
FROM sessions s
WHERE s.pitcher_athlete_id = $1
  AND s.pitch_count > 0
ORDER BY s.started_at DESC
LIMIT $2;
