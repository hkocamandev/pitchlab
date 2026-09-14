"""Load a slice of the processed snapshot into PostgreSQL.

This is a temporary bridge so the API has real data to serve before the Kafka
pipeline exists. The replay simulator takes over this job in a later phase and
this script goes away; until then it deliberately mirrors what the stream
processor will do, so the two cannot drift:

  * session identity is the deterministic UUIDv5 of (game, pitcher), so a
    reload converges on the same rows rather than duplicating them;
  * external_pitch_uid is the natural key the consumer will use for
    idempotency, and inserts use ON CONFLICT DO NOTHING;
  * derived measurement columns come from the same feature code the model
    was trained on, so the normalization is not reimplemented here;
  * the outcome is written to actual_outcome only, never anywhere a feature
    builder could reach.

Usage:
    .venv/bin/python ml/scripts/load_to_postgres.py --sessions 20
"""

from __future__ import annotations

import argparse
import logging
import sys
import uuid
from pathlib import Path

import pandas as pd
import psycopg
from psycopg.rows import dict_row

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, features, labels

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)-5s %(message)s"
)
log = logging.getLogger("load")

# Stable namespace so identifiers are reproducible across runs and processes.
PITCHLAB_NAMESPACE = uuid.UUID("6f1d5b84-6a2a-4f1e-9f3a-0b3f0f0e2d11")

DEFAULT_DSN = "postgres://pitchlab:pitchlab@localhost:5432/pitchlab"
DEVICE_KEY = "replay-sim-01"


def session_uid(game_pk, pitcher_id) -> str:
    return f"{int(game_pk)}:{int(pitcher_id)}"


def session_id(game_pk, pitcher_id) -> uuid.UUID:
    return uuid.uuid5(PITCHLAB_NAMESPACE, "session:" + session_uid(game_pk, pitcher_id))


def pitch_uid(game_pk, at_bat, pitch_no) -> str:
    return f"{int(game_pk)}:{int(at_bat)}:{int(pitch_no)}"


def athlete_id(mlbam_id) -> uuid.UUID:
    return uuid.uuid5(PITCHLAB_NAMESPACE, f"athlete:{int(mlbam_id)}")


def opt(value):
    """None for anything pandas considers missing."""
    if value is None or pd.isna(value):
        return None
    return value


def opt_float(value):
    v = opt(value)
    return None if v is None else float(v)


def opt_int(value):
    v = opt(value)
    return None if v is None else int(v)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", default=DEFAULT_DSN)
    ap.add_argument("--snapshot", type=Path, default=None)
    ap.add_argument("--sessions", type=int, default=20,
                    help="how many pitcher outings to load")
    ap.add_argument("--season", type=int, default=2025)
    args = ap.parse_args()

    snapshot = args.snapshot or sorted(config.RAW_DIR.glob("statcast_*.parquet"))[-1]

    log.info("loading %s", snapshot.name)
    cols = [
        *features.required_raw_columns(),
        "pitch_name", "game_type", "inning_topbot", "player_name",
    ]
    raw = pd.read_parquet(snapshot, columns=sorted(set(cols)))
    raw = raw[raw["game_year"] == args.season]

    df = features.build_features(labels.attach_labels(raw))
    ref = features.build_arsenal_reference(df)
    df = features.attach_arsenal_features(df, ref)

    # Pick the busiest outings so the dashboard has something to show.
    groups = (
        df.groupby(["game_pk", "pitcher"]).size()
        .sort_values(ascending=False).head(args.sessions)
    )
    keys = set(groups.index)
    df = df[df.set_index(["game_pk", "pitcher"]).index.isin(keys)].copy()
    log.info("selected %d outings, %s pitches", len(keys), f"{len(df):,}")

    with psycopg.connect(args.dsn, row_factory=dict_row, autocommit=False) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT id FROM devices WHERE device_key = %s", (DEVICE_KEY,))
            row = cur.fetchone()
            if row is None:
                log.error("device %s is missing; run `make seed` first", DEVICE_KEY)
                return 1
            device_id = row["id"]

            load_athletes(cur, df)
            load_sessions(cur, df, device_id)
            n = load_pitches(cur, df, device_id)
            cur.execute("""
                UPDATE sessions s SET
                    pitch_count = agg.n,
                    avg_release_speed = agg.speed,
                    avg_release_spin_rate = agg.spin,
                    updated_at = now()
                FROM (
                    SELECT p.session_id,
                           count(*) AS n,
                           avg(m.release_speed) AS speed,
                           avg(m.release_spin_rate) AS spin
                    FROM pitches p
                    LEFT JOIN pitch_measurements m ON m.pitch_id = p.id
                    GROUP BY p.session_id
                ) agg
                WHERE s.id = agg.session_id
            """)
        conn.commit()

    log.info("loaded %s pitches across %d sessions", f"{n:,}", len(keys))
    log.info("try: curl 'http://localhost:8081/api/v1/sessions?status=ACTIVE'")
    return 0


def load_athletes(cur, df: pd.DataFrame) -> None:
    people: dict[int, dict] = {}

    for mlbam, hand in df[["pitcher", "p_throws"]].drop_duplicates().itertuples(index=False):
        people.setdefault(int(mlbam), {})["throws"] = opt(hand)
        people[int(mlbam)]["role"] = "PITCHER"
    for mlbam, side in df[["batter", "stand"]].drop_duplicates().itertuples(index=False):
        entry = people.setdefault(int(mlbam), {})
        entry["bats"] = opt(side)
        # A person who both pitches and bats is one athlete with two roles,
        # not two rows. This is why Pitcher and Batter are not separate tables.
        entry["role"] = "TWO_WAY" if entry.get("role") == "PITCHER" else "BATTER"

    rows = [
        (
            str(athlete_id(mlbam)), mlbam,
            f"Player {mlbam}", info.get("throws"), info.get("bats"), info["role"],
        )
        for mlbam, info in people.items()
    ]

    cur.executemany("""
        INSERT INTO athletes (id, mlbam_id, full_name, throws, bats, primary_role)
        VALUES (%s, %s, %s, %s, %s, %s)
        ON CONFLICT (mlbam_id) DO UPDATE SET
            throws = COALESCE(EXCLUDED.throws, athletes.throws),
            bats   = COALESCE(EXCLUDED.bats, athletes.bats),
            updated_at = now()
    """, rows)
    log.info("athletes: %d", len(rows))


def load_sessions(cur, df: pd.DataFrame, device_id) -> None:
    rows = []
    for (game_pk, pitcher), group in df.groupby(["game_pk", "pitcher"]):
        game_date = pd.to_datetime(group["game_date"].iloc[0])
        # Statcast gives a date, not a clock time. A plausible first-pitch
        # time is synthesized here and marked synthetic, exactly as the replay
        # simulator will: the real date is kept separately in source_game_date
        # so the two are never confused.
        started_at = game_date + pd.Timedelta(hours=19, minutes=10)
        rows.append((
            str(session_id(game_pk, pitcher)),
            session_uid(game_pk, pitcher),
            str(athlete_id(pitcher)),
            str(device_id),
            str(int(game_pk)),
            game_date.date(),
            started_at.to_pydatetime(),
        ))

    cur.executemany("""
        INSERT INTO sessions (
            id, external_session_uid, pitcher_athlete_id, device_id,
            external_game_ref, source_game_date, started_at,
            status, is_synthetic
        ) VALUES (%s, %s, %s, %s, %s, %s, %s, 'COMPLETED', true)
        ON CONFLICT (external_session_uid) DO NOTHING
    """, rows)
    log.info("sessions: %d", len(rows))


def load_pitches(cur, df: pd.DataFrame, device_id) -> int:
    pitch_rows, meas_rows = [], []

    for (game_pk, pitcher), group in df.groupby(["game_pk", "pitcher"]):
        group = group.sort_values(["at_bat_number", "pitch_number"])
        sid = str(session_id(game_pk, pitcher))
        started = pd.to_datetime(group["game_date"].iloc[0]) + pd.Timedelta(hours=19, minutes=10)

        for idx, (_, r) in enumerate(group.iterrows(), start=1):
            pid = str(uuid.uuid5(
                PITCHLAB_NAMESPACE,
                "pitch:" + pitch_uid(game_pk, r["at_bat_number"], r["pitch_number"])))

            pitch_rows.append((
                pid,
                pitch_uid(game_pk, r["at_bat_number"], r["pitch_number"]),
                sid,
                str(athlete_id(pitcher)),
                str(athlete_id(r["batter"])) if opt(r["batter"]) is not None else None,
                int(r["at_bat_number"]), int(r["pitch_number"]), idx,
                (started + pd.Timedelta(seconds=int(20 * idx))).to_pydatetime(),
                int(r["balls"]), int(r["strikes"]), int(r["outs_when_up"]),
                int(r["inning"]), opt(r.get("inning_topbot")),
                r["stand"], r["p_throws"],
                opt(r["on_1b"]) is not None,
                opt(r["on_2b"]) is not None,
                opt(r["on_3b"]) is not None,
                opt_int(r.get("bat_score")), opt_int(r.get("fld_score")),
                opt_int(r.get("n_thruorder_pitcher")),
                None if r["pitch_type"] == "UNKNOWN" else str(r["pitch_type"]),
                opt(r.get("pitch_name")),
                # Ground truth. Written here and nowhere else; no feature
                # builder can reach this column.
                str(r[labels.TARGET]),
            ))

            meas_rows.append((
                pid, str(device_id),
                opt_float(r["release_speed"]), opt_float(r["release_spin_rate"]),
                opt_float(r["spin_axis"]),
                opt_float(r["release_pos_x"]), opt_float(r["release_pos_z"]),
                opt_float(r["release_extension"]),
                opt_float(r["pfx_x"]), opt_float(r["pfx_z"]),
                opt_float(r["plate_x"]), opt_float(r["plate_z"]),
                opt_float(r["sz_top"]), opt_float(r["sz_bot"]),
                opt_float(r.get("arm_angle")),
                opt_float(r["pfx_x_arm"]), opt_float(r["release_pos_x_arm"]),
                opt_float(r["plate_x_bat"]), opt_float(r["zone_height_norm"]),
                None if opt(r["in_zone"]) is None else bool(r["in_zone"]),
            ))

    cur.executemany("""
        INSERT INTO pitches (
            id, external_pitch_uid, session_id, pitcher_athlete_id, batter_athlete_id,
            at_bat_number, pitch_number, session_pitch_index, thrown_at,
            balls, strikes, outs_when_up, inning, inning_topbot,
            stand, p_throws, on_1b, on_2b, on_3b,
            bat_score, fld_score, n_thruorder_pitcher,
            pitch_type, pitch_name, actual_outcome, prediction_status
        ) VALUES (
            %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
            %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, 'PENDING'
        )
        ON CONFLICT (external_pitch_uid) DO NOTHING
    """, pitch_rows)

    cur.executemany("""
        INSERT INTO pitch_measurements (
            pitch_id, device_id,
            release_speed, release_spin_rate, spin_axis,
            release_pos_x, release_pos_z, release_extension,
            pfx_x, pfx_z, plate_x, plate_z, sz_top, sz_bot, arm_angle,
            pfx_x_arm, release_pos_x_arm, plate_x_bat, zone_height_norm, in_zone,
            normalization_version, measurement_quality
        ) VALUES (
            %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
            %s, %s, %s, %s, %s, 1, 'OK'
        )
        ON CONFLICT (pitch_id) DO NOTHING
    """, meas_rows)

    return len(pitch_rows)


if __name__ == "__main__":
    raise SystemExit(main())
