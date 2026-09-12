-- name: InsertModelVersion :one
INSERT INTO model_versions (
    id, version, variant, target, algorithm,
    feature_columns, class_labels, run_value_table, score_mu, score_sigma,
    train_period_start, train_period_end,
    val_period_start, val_period_end, test_period_start, test_period_end,
    data_snapshot, git_sha, metrics, is_active
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
    $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
)
ON CONFLICT (version, variant) DO UPDATE SET
    metrics = EXCLUDED.metrics
RETURNING *;

-- name: DeactivateModelVariant :exec
-- Step one of activation. Must run in the same transaction as step two.
UPDATE model_versions SET is_active = false
WHERE variant = $1 AND is_active;

-- name: MarkModelVersionActive :one
-- Step two of activation.
--
-- This was first written as a single statement with a data-modifying CTE that
-- deactivated the incumbent before activating the successor. It does not work:
-- in PostgreSQL every CTE and the outer statement see the same snapshot and
-- run concurrently, so the deactivation is invisible to the activation and the
-- partial unique index on (variant) WHERE is_active rejects the write. An
-- integration test caught it.
--
-- Two statements in one transaction is the correct shape: the second sees the
-- first, and the unique index still makes two concurrent activations
-- impossible rather than merely unlikely -- one of them fails.
UPDATE model_versions SET is_active = true
WHERE id = $1
RETURNING *;

-- name: GetActiveModelVersion :one
SELECT * FROM model_versions WHERE variant = $1 AND is_active;

-- name: GetModelVersion :one
SELECT * FROM model_versions WHERE id = $1;

-- name: ListModelVersions :many
SELECT * FROM model_versions
WHERE (sqlc.narg('variant')::text IS NULL OR variant = sqlc.narg('variant'))
  AND (sqlc.narg('active')::bool IS NULL OR is_active = sqlc.narg('active'))
ORDER BY created_at DESC;

-- name: UpsertPitchPrediction :one
-- Idempotent on (pitch, model version): re-processing the same pitch with the
-- same model must not produce a second row. Values are refreshed rather than
-- ignored, so a re-run after a bug fix corrects the stored prediction.
INSERT INTO pitch_predictions (
    id, pitch_id, model_version_id,
    probabilities, predicted_class, whiff_probability,
    scored_quantity, score, inference_ms
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (pitch_id, model_version_id) DO UPDATE SET
    probabilities     = EXCLUDED.probabilities,
    predicted_class   = EXCLUDED.predicted_class,
    whiff_probability = EXCLUDED.whiff_probability,
    scored_quantity   = EXCLUDED.scored_quantity,
    score             = EXCLUDED.score,
    inference_ms      = EXCLUDED.inference_ms
RETURNING *;

-- name: ListPredictionsForPitch :many
SELECT sqlc.embed(pr), sqlc.embed(mv)
FROM pitch_predictions pr
JOIN model_versions mv ON mv.id = pr.model_version_id
WHERE pr.pitch_id = $1
  AND (sqlc.narg('variant')::text IS NULL OR mv.variant = sqlc.narg('variant'))
  AND (sqlc.narg('version')::text IS NULL OR mv.version = sqlc.narg('version'))
ORDER BY mv.variant, mv.created_at DESC;

-- name: GetActivePredictionsForPitch :many
SELECT sqlc.embed(pr), sqlc.embed(mv)
FROM pitch_predictions pr
JOIN model_versions mv ON mv.id = pr.model_version_id
WHERE pr.pitch_id = $1 AND mv.is_active
ORDER BY mv.variant;
