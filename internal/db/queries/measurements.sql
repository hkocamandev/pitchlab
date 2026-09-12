-- name: UpsertPitchMeasurement :one
-- Keyed on pitch_id, so re-processing a pitch replaces its measurement rather
-- than duplicating it. The update path is also what a re-normalization pass
-- uses: it rewrites the derived columns and bumps normalization_version
-- without touching the pitch's context row.
INSERT INTO pitch_measurements (
    pitch_id, device_id,
    release_speed, release_spin_rate, spin_axis,
    release_pos_x, release_pos_y, release_pos_z, release_extension,
    pfx_x, pfx_z, plate_x, plate_z, sz_top, sz_bot,
    effective_speed, arm_angle,
    pfx_x_arm, release_pos_x_arm, plate_x_bat, zone_height_norm, in_zone,
    normalization_version, measurement_quality
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18, $19, $20, $21, $22, $23, $24
)
ON CONFLICT (pitch_id) DO UPDATE SET
    device_id             = EXCLUDED.device_id,
    release_speed         = EXCLUDED.release_speed,
    release_spin_rate     = EXCLUDED.release_spin_rate,
    spin_axis             = EXCLUDED.spin_axis,
    release_pos_x         = EXCLUDED.release_pos_x,
    release_pos_y         = EXCLUDED.release_pos_y,
    release_pos_z         = EXCLUDED.release_pos_z,
    release_extension     = EXCLUDED.release_extension,
    pfx_x                 = EXCLUDED.pfx_x,
    pfx_z                 = EXCLUDED.pfx_z,
    plate_x               = EXCLUDED.plate_x,
    plate_z               = EXCLUDED.plate_z,
    sz_top                = EXCLUDED.sz_top,
    sz_bot                = EXCLUDED.sz_bot,
    effective_speed       = EXCLUDED.effective_speed,
    arm_angle             = EXCLUDED.arm_angle,
    pfx_x_arm             = EXCLUDED.pfx_x_arm,
    release_pos_x_arm     = EXCLUDED.release_pos_x_arm,
    plate_x_bat           = EXCLUDED.plate_x_bat,
    zone_height_norm      = EXCLUDED.zone_height_norm,
    in_zone               = EXCLUDED.in_zone,
    normalization_version = EXCLUDED.normalization_version,
    measurement_quality   = EXCLUDED.measurement_quality
RETURNING *;

-- name: GetPitchMeasurement :one
SELECT * FROM pitch_measurements WHERE pitch_id = $1;

-- name: CountMeasurementsByNormalizationVersion :many
-- Tells you whether a re-normalization pass finished, or left the table
-- holding two conventions at once.
SELECT normalization_version, count(*) AS n
FROM pitch_measurements
GROUP BY normalization_version
ORDER BY normalization_version;
