-- ---------------------------------------------------------------------------
-- pitch_predictions
--
-- Separate from pitches because one pitch has N predictions: the outcome
-- model and the stuff model each produce one, and retraining adds more
-- without deleting the old ones, so "is v1.1 actually better than v1.0" can
-- be answered retrospectively on the same pitches. Columns on `pitches`
-- would need an ALTER for every new variant.
-- ---------------------------------------------------------------------------
CREATE TABLE pitch_predictions (
    id                UUID        PRIMARY KEY,
    pitch_id          UUID        NOT NULL
                          REFERENCES pitches(id) ON DELETE CASCADE,
    -- RESTRICT, not CASCADE: a model version must not be removable while
    -- predictions reference it, or those predictions become uninterpretable.
    model_version_id  UUID        NOT NULL
                          REFERENCES model_versions(id) ON DELETE RESTRICT,

    -- Five columns would need a migration if the class set ever changes;
    -- single-class lookups are rare enough that JSONB is the better trade.
    probabilities     JSONB,
    -- Denormalized argmax: computing it from JSONB on every read is wasteful
    -- and it never changes once written.
    predicted_class   TEXT,
    -- Binary stuff model output. Null for the 5-class model, and vice versa.
    whiff_probability REAL,

    scored_quantity   REAL        NOT NULL,
    score             REAL        NOT NULL,

    inference_ms      REAL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Idempotency at the prediction layer: re-processing the same pitch with
    -- the same model must not create a second row.
    CONSTRAINT uq_prediction_pitch_model UNIQUE (pitch_id, model_version_id),
    CONSTRAINT ck_pred_class CHECK (
        predicted_class IS NULL OR predicted_class IN (
            'BALL', 'CALLED_STRIKE', 'SWINGING_STRIKE', 'FOUL', 'IN_PLAY'
        )
    ),
    CONSTRAINT ck_pred_score CHECK (score BETWEEN 0 AND 200),
    CONSTRAINT ck_pred_whiff CHECK (
        whiff_probability IS NULL OR whiff_probability BETWEEN 0 AND 1
    ),
    -- Exactly one shape of output: a class distribution or a whiff
    -- probability, never both and never neither.
    CONSTRAINT ck_pred_output_shape CHECK (
        (probabilities IS NOT NULL AND predicted_class IS NOT NULL
             AND whiff_probability IS NULL)
        OR (probabilities IS NULL AND predicted_class IS NULL
             AND whiff_probability IS NOT NULL)
    )
);

CREATE INDEX ix_pred_pitch ON pitch_predictions (pitch_id);

-- "What did this model version produce, and since when."
CREATE INDEX ix_pred_model ON pitch_predictions (model_version_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- performance_anomalies
--
-- Session-level, not pitch-level, and produced minutes after the session
-- ends, so it has both a different granularity and a different lifecycle
-- from a pitch. Persisted rather than only pushed over WebSocket, because
-- alert history is a product feature and a notification must not be lost to
-- a dropped connection.
-- ---------------------------------------------------------------------------
CREATE TABLE performance_anomalies (
    id                UUID        PRIMARY KEY,
    athlete_id        UUID        NOT NULL
                          REFERENCES athletes(id) ON DELETE RESTRICT,
    session_id        UUID        NOT NULL
                          REFERENCES sessions(id) ON DELETE CASCADE,

    metric            TEXT        NOT NULL,
    pitch_type        TEXT,
    observed_value    REAL        NOT NULL,
    -- Baseline stored alongside the finding so that "why was this flagged"
    -- is answerable from the row alone. Recomputing it later would give a
    -- different answer, because the trailing window has moved on.
    baseline_median   REAL        NOT NULL,
    robust_sigma      REAL        NOT NULL,
    z_robust          REAL        NOT NULL,
    direction         TEXT        NOT NULL,
    severity          TEXT        NOT NULL,
    window_sessions   SMALLINT    NOT NULL,
    detector_version  TEXT        NOT NULL,

    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at   TIMESTAMPTZ,

    CONSTRAINT ck_anomaly_metric CHECK (
        metric IN (
            'release_speed', 'release_spin_rate', 'release_pos_x_arm',
            'release_pos_z', 'release_extension', 'pfx_x_arm', 'pfx_z'
        )
    ),
    CONSTRAINT ck_anomaly_dir      CHECK (direction IN ('INCREASE', 'DECREASE')),
    CONSTRAINT ck_anomaly_sev      CHECK (severity IN ('MEDIUM', 'HIGH')),
    CONSTRAINT ck_anomaly_sigma    CHECK (robust_sigma > 0),
    CONSTRAINT ck_anomaly_window   CHECK (window_sessions >= 1)
);

-- Idempotency for the anomaly consumer. COALESCE is required because
-- pitch_type is nullable and NULL <> NULL in a plain unique constraint,
-- which would let duplicates through on exactly the rows that need it.
CREATE UNIQUE INDEX uq_anomaly_session_metric
    ON performance_anomalies (
        session_id, metric, COALESCE(pitch_type, ''), detector_version
    );

CREATE INDEX ix_anomaly_athlete_time
    ON performance_anomalies (athlete_id, detected_at DESC);

-- Open-alerts view; partial so it stays small as history accumulates.
CREATE INDEX ix_anomaly_open
    ON performance_anomalies (athlete_id, detected_at DESC)
    WHERE acknowledged_at IS NULL;
