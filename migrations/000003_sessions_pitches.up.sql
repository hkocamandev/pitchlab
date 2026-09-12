-- ---------------------------------------------------------------------------
-- sessions
--
-- A pitcher's outing. In historical Statcast this is (game, pitcher); in a
-- bullpen it is a training session. The abstraction earns its place by
-- carrying three architectural roles at once: the WebSocket channel
-- boundary, the Kafka partition key, and the aggregation unit for anomaly
-- detection.
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id                    UUID        PRIMARY KEY,
    external_session_uid  TEXT        NOT NULL,
    pitcher_athlete_id    UUID        NOT NULL
                              REFERENCES athletes(id) ON DELETE RESTRICT,
    device_id             UUID        REFERENCES devices(id) ON DELETE RESTRICT,

    external_game_ref     TEXT,
    -- The real calendar date of the source game. Kept separate from
    -- started_at, which is synthetic under replay.
    source_game_date      DATE,
    opponent_team         TEXT,

    started_at            TIMESTAMPTZ NOT NULL,
    ended_at              TIMESTAMPTZ,
    status                TEXT        NOT NULL DEFAULT 'ACTIVE',
    is_synthetic          BOOLEAN     NOT NULL DEFAULT false,

    -- Denormalized so the session list page does not run one aggregate per
    -- row. Updated incrementally; pitch_count cross-checks the rest.
    pitch_count           INTEGER     NOT NULL DEFAULT 0,
    avg_release_speed     REAL,
    avg_release_spin_rate REAL,
    avg_pitch_score       REAL,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_sessions_external_uid UNIQUE (external_session_uid),
    CONSTRAINT ck_sessions_status       CHECK (
        status IN ('ACTIVE', 'COMPLETED', 'ABORTED')
    ),
    CONSTRAINT ck_sessions_time_order   CHECK (
        ended_at IS NULL OR ended_at >= started_at
    ),
    CONSTRAINT ck_sessions_pitch_count  CHECK (pitch_count >= 0)
);

-- Most common query: this pitcher's recent outings. One composite index
-- serves both the filter and the ordering, so no sort step is needed.
CREATE INDEX ix_sessions_pitcher_started
    ON sessions (pitcher_athlete_id, started_at DESC);

-- Active sessions are a tiny fraction of the table, so a partial index stays
-- small and makes "what is live right now" nearly free.
CREATE INDEX ix_sessions_status_started
    ON sessions (status, started_at DESC) WHERE status = 'ACTIVE';

-- ---------------------------------------------------------------------------
-- pitches
--
-- The central event. Carries identity, sequencing, and pre-pitch context.
-- Outcome-bearing fields are deliberately limited to actual_outcome, which
-- exists for prediction-versus-reality display and production model
-- monitoring, and never reaches a feature vector.
-- ---------------------------------------------------------------------------
CREATE TABLE pitches (
    id                    UUID        PRIMARY KEY,
    external_pitch_uid    TEXT        NOT NULL,
    session_id            UUID        NOT NULL
                              REFERENCES sessions(id) ON DELETE CASCADE,
    pitcher_athlete_id    UUID        NOT NULL
                              REFERENCES athletes(id) ON DELETE RESTRICT,
    batter_athlete_id     UUID        REFERENCES athletes(id) ON DELETE RESTRICT,

    -- sequencing
    at_bat_number         SMALLINT    NOT NULL,
    pitch_number          SMALLINT    NOT NULL,
    session_pitch_index   INTEGER     NOT NULL,
    thrown_at             TIMESTAMPTZ NOT NULL,

    -- pre-pitch context; Statcast documents each of these as "Pre-pitch"
    balls                 SMALLINT    NOT NULL,
    strikes               SMALLINT    NOT NULL,
    outs_when_up          SMALLINT    NOT NULL,
    inning                SMALLINT    NOT NULL,
    inning_topbot         TEXT,
    stand                 CHAR(1)     NOT NULL,
    p_throws              CHAR(1)     NOT NULL,
    on_1b                 BOOLEAN     NOT NULL DEFAULT false,
    on_2b                 BOOLEAN     NOT NULL DEFAULT false,
    on_3b                 BOOLEAN     NOT NULL DEFAULT false,
    bat_score             SMALLINT,
    fld_score             SMALLINT,
    n_thruorder_pitcher   SMALLINT,

    pitch_type            TEXT,
    pitch_name            TEXT,

    actual_outcome        TEXT,
    prediction_status     TEXT        NOT NULL DEFAULT 'PENDING',

    -- provenance for end-to-end tracing
    correlation_id        UUID,
    source_event_id       UUID,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The sole enforcer of consumer idempotency. An application-level
    -- select-then-insert races between processor instances; a unique
    -- constraint is atomic regardless of concurrency.
    CONSTRAINT uq_pitches_external_uid UNIQUE (external_pitch_uid),

    -- There is deliberately no second unique constraint on
    -- (session_id, at_bat_number, pitch_number).
    --
    -- It was there as a "second line of defence", and it broke the thing it
    -- was meant to protect. ON CONFLICT names a single arbiter index, so a
    -- redelivered pitch -- which collides on both -- resolves cleanly against
    -- external_pitch_uid but raises a hard error on the other constraint when
    -- two consumers race. A concurrency test caught it.
    --
    -- The constraint was also close to redundant: external_pitch_uid is
    -- derived from game, at-bat and pitch number, and an at-bat has exactly
    -- one pitcher, so it already identifies a pitch uniquely. A speculative
    -- guard that costs a demonstrated failure mode is not worth keeping.

    -- The rulebook. A device reporting balls = 5 is reporting a fault, and
    -- it should be stopped here rather than silently recorded.
    CONSTRAINT ck_pitches_balls       CHECK (balls BETWEEN 0 AND 3),
    CONSTRAINT ck_pitches_strikes     CHECK (strikes BETWEEN 0 AND 2),
    CONSTRAINT ck_pitches_outs        CHECK (outs_when_up BETWEEN 0 AND 2),
    CONSTRAINT ck_pitches_inning      CHECK (inning >= 1),
    CONSTRAINT ck_pitches_seq         CHECK (
        at_bat_number >= 1 AND pitch_number >= 1 AND session_pitch_index >= 1
    ),
    CONSTRAINT ck_pitches_stand       CHECK (stand IN ('L', 'R')),
    CONSTRAINT ck_pitches_throws      CHECK (p_throws IN ('L', 'R')),
    CONSTRAINT ck_pitches_topbot      CHECK (
        inning_topbot IS NULL OR inning_topbot IN ('Top', 'Bot')
    ),
    -- Mirrors the ML class set, so a casing typo cannot slip through.
    CONSTRAINT ck_pitches_outcome     CHECK (
        actual_outcome IS NULL OR actual_outcome IN (
            'BALL', 'CALLED_STRIKE', 'SWINGING_STRIKE', 'FOUL', 'IN_PLAY', 'OTHER'
        )
    ),
    CONSTRAINT ck_pitches_pred_status CHECK (
        prediction_status IN ('PENDING', 'OK', 'FAILED', 'SKIPPED')
    )
);

COMMENT ON COLUMN pitches.actual_outcome IS
    'Ground truth for prediction-vs-reality display and model monitoring. '
    'Never a model feature.';

-- Session timeline: the session detail page's primary query.
CREATE INDEX ix_pitches_session_seq ON pitches (session_id, session_pitch_index);

-- Pitcher analytics: recent pitches, trend charts.
CREATE INDEX ix_pitches_pitcher_time ON pitches (pitcher_athlete_id, thrown_at DESC);

-- Arsenal distribution, aggregated per pitch type.
CREATE INDEX ix_pitches_pitcher_type ON pitches (pitcher_athlete_id, pitch_type)
    WHERE pitch_type IS NOT NULL;

-- Tracing: "which pitch had this correlation id".
CREATE INDEX ix_pitches_correlation ON pitches (correlation_id)
    WHERE correlation_id IS NOT NULL;

-- Backfill: find pitches whose prediction failed. Expected to be a tiny
-- fraction of the table, so the partial index stays negligible.
CREATE INDEX ix_pitches_pred_failed ON pitches (prediction_status)
    WHERE prediction_status = 'FAILED';

-- ---------------------------------------------------------------------------
-- pitch_measurements
--
-- Separate from pitches because the two differ in provenance (scoreboard vs
-- device), null profile (context is always present, sensor readings are not),
-- lifecycle (measurements can be re-normalized without touching context),
-- and access pattern. Also row width: the session timeline reads every pitch
-- row and needs none of these twenty-odd floats.
-- ---------------------------------------------------------------------------
CREATE TABLE pitch_measurements (
    pitch_id              UUID        PRIMARY KEY
                              REFERENCES pitches(id) ON DELETE CASCADE,
    device_id             UUID        REFERENCES devices(id) ON DELETE RESTRICT,

    -- raw device readings, named as Statcast names them
    release_speed         REAL,
    release_spin_rate     REAL,
    spin_axis             REAL,
    release_pos_x         REAL,
    release_pos_y         REAL,
    release_pos_z         REAL,
    release_extension     REAL,
    pfx_x                 REAL,
    pfx_z                 REAL,
    plate_x               REAL,
    plate_z               REAL,
    sz_top                REAL,
    sz_bot                REAL,
    effective_speed       REAL,
    arm_angle             REAL,

    -- derived, handedness-neutral frame; positive means arm side / inside
    pfx_x_arm             REAL,
    release_pos_x_arm     REAL,
    plate_x_bat           REAL,
    zone_height_norm      REAL,
    in_zone               BOOLEAN,

    -- Which normalization pass produced the derived columns. Without this a
    -- partial re-normalization silently mixes two conventions.
    normalization_version SMALLINT    NOT NULL DEFAULT 1,
    measurement_quality   TEXT        NOT NULL DEFAULT 'OK',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Generous physical bounds: the intent is to catch faults, not to reject
    -- normal variation. A 250 mph pitch is a sensor error and must not reach
    -- the training set.
    CONSTRAINT ck_meas_speed   CHECK (
        release_speed IS NULL OR release_speed BETWEEN 40 AND 110
    ),
    CONSTRAINT ck_meas_spin    CHECK (
        release_spin_rate IS NULL OR release_spin_rate BETWEEN 0 AND 4000
    ),
    CONSTRAINT ck_meas_axis    CHECK (
        spin_axis IS NULL OR spin_axis BETWEEN 0 AND 360
    ),
    CONSTRAINT ck_meas_ext     CHECK (
        release_extension IS NULL OR release_extension BETWEEN 3 AND 9
    ),
    CONSTRAINT ck_meas_relz    CHECK (
        release_pos_z IS NULL OR release_pos_z BETWEEN 0 AND 8
    ),
    CONSTRAINT ck_meas_quality CHECK (
        measurement_quality IN ('OK', 'PARTIAL', 'SUSPECT')
    ),
    CONSTRAINT ck_meas_norm_version CHECK (normalization_version >= 1)
);

-- No index on release_speed.
--
-- The design listed one for "velocity filtering and anomaly baselines", but
-- neither access path actually reaches it: the pitcher-scoped pitch search
-- drives off ix_pitches_pitcher_time and then joins measurements by primary
-- key, and the anomaly baseline query does the same. An index no query uses
-- costs every write and returns nothing, so it is not created. The catalogue
-- check in TestEveryIndexIsUsedBySomething is what surfaced this.
