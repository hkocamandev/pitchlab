-- name: UpsertDevice :one
-- Idempotent by device_key. The replay simulator registers itself on every
-- run, which is how simulated rows stay distinguishable from real ones.
INSERT INTO devices (id, device_key, device_kind, display_name, calibration_profile)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (device_key) DO UPDATE SET
    device_kind         = EXCLUDED.device_kind,
    display_name        = COALESCE(EXCLUDED.display_name, devices.display_name),
    calibration_profile = EXCLUDED.calibration_profile
RETURNING *;

-- name: GetDeviceByKey :one
SELECT * FROM devices WHERE device_key = $1;

-- name: ListDevices :many
SELECT * FROM devices ORDER BY device_key;
