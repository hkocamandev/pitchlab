-- Reference entities: people, measurement sources, and model versions.
-- All three have lifecycles independent of any individual pitch.

-- ---------------------------------------------------------------------------
-- athletes
--
-- One row per person. Pitcher and batter are *roles*, expressed through the
-- foreign keys on `pitches`, not separate entities: the same player can do
-- both, MLBAM itself uses a single player id, and splitting them would force
-- a UNION for "everything this person did".
-- ---------------------------------------------------------------------------
CREATE TABLE athletes (
    id            UUID        PRIMARY KEY,
    mlbam_id      INTEGER     NOT NULL,
    full_name     TEXT        NOT NULL,
    throws        CHAR(1),
    bats          CHAR(1),
    primary_role  TEXT        NOT NULL DEFAULT 'UNKNOWN',
    birth_date    DATE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The basis of idempotent upserts: the same athlete arriving repeatedly
    -- from the event stream must converge on one row.
    CONSTRAINT uq_athletes_mlbam_id UNIQUE (mlbam_id),
    CONSTRAINT ck_athletes_throws   CHECK (throws IS NULL OR throws IN ('L', 'R')),
    CONSTRAINT ck_athletes_bats     CHECK (bats IS NULL OR bats IN ('L', 'R', 'S')),
    CONSTRAINT ck_athletes_role     CHECK (
        primary_role IN ('PITCHER', 'BATTER', 'TWO_WAY', 'UNKNOWN')
    ),
    CONSTRAINT ck_athletes_name     CHECK (length(btrim(full_name)) > 0)
);

COMMENT ON COLUMN athletes.primary_role IS
    'Presentation hint only. The authoritative role is the FK used on pitches.';

CREATE INDEX ix_athletes_full_name_trgm
    ON athletes USING gin (full_name gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- devices
--
-- Provenance for every measurement. Small table, large payoff: the replay
-- simulator registers itself here, which is what lets simulated and real
-- data be told apart at the database level rather than by convention.
-- ---------------------------------------------------------------------------
CREATE TABLE devices (
    id                  UUID        PRIMARY KEY,
    device_key          TEXT        NOT NULL,
    device_kind         TEXT        NOT NULL,
    display_name        TEXT,
    -- Shape varies by device kind and is rarely queried, so JSONB beats a
    -- wide set of mostly-null columns. Holds things like the measurement
    -- regime: {"plate_measurement_point": "front", "zone_source": "operator"}
    calibration_profile JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_devices_key  UNIQUE (device_key),
    CONSTRAINT ck_devices_kind CHECK (
        device_kind IN ('REPLAY_SIMULATOR', 'HAWKEYE', 'TRACKMAN', 'OTHER')
    )
);

-- ---------------------------------------------------------------------------
-- model_versions
--
-- A prediction cannot be interpreted without knowing which model produced it.
-- "This pitch scored 112" is incomplete until "according to what?" is
-- answered, so the model card is stored once and referenced.
-- ---------------------------------------------------------------------------
CREATE TABLE model_versions (
    id                  UUID        PRIMARY KEY,
    version             TEXT        NOT NULL,
    variant             TEXT        NOT NULL,
    target              TEXT        NOT NULL,
    algorithm           TEXT        NOT NULL,

    -- The feature contract. Training and inference must agree on this exact
    -- ordered list; storing it makes a mismatch detectable after the fact.
    feature_columns     JSONB       NOT NULL,
    class_labels        JSONB,
    run_value_table     JSONB,
    score_mu            REAL        NOT NULL,
    score_sigma         REAL        NOT NULL,

    train_period_start  DATE        NOT NULL,
    train_period_end    DATE        NOT NULL,
    val_period_start    DATE,
    val_period_end      DATE,
    test_period_start   DATE,
    test_period_end     DATE,
    data_snapshot       TEXT        NOT NULL,
    git_sha             TEXT,

    metrics             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    is_active           BOOLEAN     NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_model_versions   UNIQUE (version, variant),
    CONSTRAINT ck_model_variant    CHECK (variant IN ('stuff', 'pitching')),
    CONSTRAINT ck_model_target     CHECK (
        target IN ('outcome_5class', 'whiff_given_swing')
    ),
    CONSTRAINT ck_model_sigma      CHECK (score_sigma > 0),
    CONSTRAINT ck_model_period     CHECK (train_period_end >= train_period_start),
    -- The 5-class model must carry the artifacts that make its score
    -- reproducible; the binary stuff model has neither.
    CONSTRAINT ck_model_outcome_artifacts CHECK (
        (target <> 'outcome_5class')
        OR (class_labels IS NOT NULL AND run_value_table IS NOT NULL)
    )
);

-- At most one active model per variant, enforced by the database rather than
-- by an application-level "deactivate then activate" sequence, which is open
-- to a race between two concurrent deployments.
CREATE UNIQUE INDEX uq_model_active_per_variant
    ON model_versions (variant) WHERE is_active;
