-- name: GetAthleteSummary :one
-- The KPI strip on the athlete overview page. Expensive enough to be worth
-- caching in Redis; correct enough to be the fallback when the cache is cold
-- or unavailable.
--
-- Every aggregate is wrapped in COALESCE and cast explicitly. Two reasons:
-- sqlc infers a bare aggregate as non-nullable, which panics when scanning
-- the NULL an empty group produces, and it mis-infers the integer division
-- guard as an integer type. The handler suppresses these values when
-- pitch_count is zero, which is the only case where the zero is a lie.
SELECT
    count(*)::bigint                               AS pitch_count,
    count(DISTINCT p.session_id)::bigint           AS session_count,
    COALESCE(avg(m.release_speed), 0)::float8      AS avg_release_speed,
    COALESCE(avg(m.release_spin_rate), 0)::float8  AS avg_release_spin_rate,
    COALESCE(avg(m.release_extension), 0)::float8  AS avg_release_extension,
    COALESCE(avg(pr.score), 0)::float8             AS avg_pitch_score,
    -- Whiff rate has swings in the denominator, not pitches: it measures how
    -- often a batter misses when he offers.
    COALESCE(
        sum(CASE WHEN p.actual_outcome = 'SWINGING_STRIKE' THEN 1 ELSE 0 END)::float8
        / NULLIF(sum(CASE WHEN p.actual_outcome IN
            ('SWINGING_STRIKE', 'FOUL', 'IN_PLAY') THEN 1 ELSE 0 END), 0),
        0)::float8                                 AS whiff_rate,
    COALESCE(
        sum(CASE WHEN p.actual_outcome = 'CALLED_STRIKE' THEN 1 ELSE 0 END)::float8
        / NULLIF(count(*), 0),
        0)::float8                                 AS called_strike_rate,
    COALESCE(
        sum(CASE WHEN m.in_zone THEN 1 ELSE 0 END)::float8
        / NULLIF(count(m.in_zone), 0),
        0)::float8                                 AS in_zone_rate
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
    max(p.pitch_name)                              AS pitch_name,
    count(*)::bigint                               AS n,
    COALESCE(avg(m.release_speed), 0)::float8      AS avg_release_speed,
    COALESCE(avg(m.release_spin_rate), 0)::float8  AS avg_release_spin_rate,
    COALESCE(avg(m.pfx_x_arm), 0)::float8          AS avg_pfx_x_arm,
    COALESCE(avg(m.pfx_z), 0)::float8              AS avg_pfx_z,
    COALESCE(avg(pr.score), 0)::float8             AS avg_pitch_score,
    COALESCE(
        sum(CASE WHEN p.actual_outcome = 'SWINGING_STRIKE' THEN 1 ELSE 0 END)::float8
        / NULLIF(sum(CASE WHEN p.actual_outcome IN
            ('SWINGING_STRIKE', 'FOUL', 'IN_PLAY') THEN 1 ELSE 0 END), 0),
        0)::float8                                 AS whiff_rate
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
