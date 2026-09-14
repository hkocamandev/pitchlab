"""Export a replay tape: historical pitches shaped as device readings.

The design had the Go simulator read Parquet directly. This exports an
NDJSON tape instead, and the reasoning is worth stating: the simulator's job
is to imitate a tracking device, and a tracking device does not read Parquet.
Keeping the columnar format on the Python side leaves the ETL doing data
engineering and the simulator doing event emission, which is a cleaner split
than making the Go tool a Parquet reader -- and it keeps the simulator's
dependency list short enough that its compile-time constraints stay easy to
enforce.

What the tape deliberately does NOT contain:

  * any normalization. Arm-frame mirroring, circular spin encoding and zone
    normalization are domain rules and belong in the processor, where they
    are tested. A real device knows nothing about handedness conventions.

  * any outcome field beyond the ground_truth block. A camera cannot know a
    pitch's exit velocity or its change in run expectancy, so the tape has
    nowhere to put one. Imitating the device faithfully is what makes the
    leakage defense structural rather than a matter of discipline.

Usage:
    .venv/bin/python ml/scripts/export_replay_tape.py --sessions 10
"""

from __future__ import annotations

import argparse
import json
import logging
import math
import sys
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, labels

logging.basicConfig(level=logging.INFO, format="%(levelname)-5s %(message)s")
log = logging.getLogger("tape")

# Raw device fields only. Every one of these is something a tracking system
# measures at or shortly after release.
MEASUREMENT_FIELDS = [
    "release_speed", "release_spin_rate", "spin_axis",
    "release_pos_x", "release_pos_y", "release_pos_z", "release_extension",
    "pfx_x", "pfx_z", "plate_x", "plate_z", "sz_top", "sz_bot",
    "effective_speed", "arm_angle",
]

CONTEXT_FIELDS = [
    "balls", "strikes", "outs_when_up", "inning", "inning_topbot",
    "on_1b", "on_2b", "on_3b", "bat_score", "fld_score", "n_thruorder_pitcher",
]

REQUIRED = [
    "game_pk", "game_date", "game_year", "at_bat_number", "pitch_number",
    "pitcher", "batter", "p_throws", "stand", "pitch_type", "pitch_name",
    "home_team", "away_team", "inning_topbot",
    # `description` is read here and nowhere else: it is the source of the
    # ground-truth block, and it is dropped from the frame before anything is
    # written. It never travels as a measurement.
    "description",
    *MEASUREMENT_FIELDS,
    *CONTEXT_FIELDS,
]


def clean(v):
    if v is None or (isinstance(v, float) and math.isnan(v)) or pd.isna(v):
        return None
    return v


def num(v):
    v = clean(v)
    return None if v is None else float(v)


def integer(v):
    v = clean(v)
    return None if v is None else int(v)


def text(v):
    v = clean(v)
    return None if v is None else str(v)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--snapshot", type=Path, default=None)
    ap.add_argument("--out", type=Path, default=None)
    ap.add_argument("--season", type=int, default=2025,
                    help="2026 is a different measurement regime; see the README")
    ap.add_argument("--sessions", type=int, default=10,
                    help="how many pitcher outings to include")
    ap.add_argument("--min-pitches", type=int, default=40,
                    help="skip outings shorter than this")
    args = ap.parse_args()

    snapshot = args.snapshot or sorted(config.RAW_DIR.glob("statcast_*.parquet"))[-1]
    out_path = args.out or (config.PROCESSED_DIR /
                            f"replay_tape_{args.season}_{args.sessions}.ndjson")

    log.info("reading %s", snapshot.name)
    df = pd.read_parquet(snapshot, columns=sorted(set(REQUIRED)))
    df = df[df["game_year"] == args.season]

    # Only competitive pitches. The excluded ones -- pitch-timer violations,
    # intentional walks, bunts -- either have no pitch behind them or are a
    # different event entirely, and a device would not report them as pitches.
    df = labels.attach_labels(df)

    sizes = df.groupby(["game_pk", "pitcher"]).size()
    sizes = sizes[sizes >= args.min_pitches].sort_values(ascending=False)
    chosen = set(sizes.head(args.sessions).index)
    if not chosen:
        log.error("no outing had at least %d pitches", args.min_pitches)
        return 1

    df = df[df.set_index(["game_pk", "pitcher"]).index.isin(chosen)].copy()
    df = df.sort_values(["game_date", "game_pk", "pitcher",
                         "at_bat_number", "pitch_number"])

    log.info("selected %d outings, %s pitches", len(chosen), f"{len(df):,}")

    written = 0
    with out_path.open("w", encoding="utf-8") as fh:
        for (game_pk, pitcher), group in df.groupby(["game_pk", "pitcher"], sort=False):
            group = group.sort_values(["at_bat_number", "pitch_number"])
            first = group.iloc[0]

            session_uid = f"{int(game_pk)}:{int(pitcher)}"
            opponent = text(first.get("away_team")) or ""
            if text(first.get("inning_topbot")) == "Bot":
                opponent = text(first.get("home_team")) or opponent

            for _, r in group.iterrows():
                record = {
                    "external_game_ref": str(int(r["game_pk"])),
                    "at_bat_number": integer(r["at_bat_number"]),
                    "pitch_number": integer(r["pitch_number"]),
                    "source_game_date": str(pd.to_datetime(r["game_date"]).date()),
                    "session": {
                        "session_uid": session_uid,
                        "pitcher_mlbam_id": int(pitcher),
                        "p_throws": text(r["p_throws"]) or "R",
                        "opponent_team": opponent,
                    },
                    "batter": {
                        "mlbam_id": integer(r["batter"]) or 0,
                        "stand": text(r["stand"]) or "R",
                    },
                    "context": {
                        "balls": integer(r["balls"]) or 0,
                        "strikes": integer(r["strikes"]) or 0,
                        "outs_when_up": integer(r["outs_when_up"]) or 0,
                        "inning": integer(r["inning"]) or 1,
                        "inning_topbot": text(r["inning_topbot"]),
                        # Runner columns hold a player id or nothing; the tape
                        # carries only whether the base was occupied, which is
                        # all the pre-pitch state the model uses.
                        "on_1b": clean(r["on_1b"]) is not None,
                        "on_2b": clean(r["on_2b"]) is not None,
                        "on_3b": clean(r["on_3b"]) is not None,
                        "bat_score": integer(r["bat_score"]),
                        "fld_score": integer(r["fld_score"]),
                        "n_thruorder_pitcher": integer(r["n_thruorder_pitcher"]),
                    },
                    "measurement": {
                        "pitch_type": text(r["pitch_type"]),
                        "pitch_name": text(r["pitch_name"]),
                        **{f: num(r[f]) for f in MEASUREMENT_FIELDS},
                    },
                    # Kept apart from the measurement so it reaches exactly one
                    # place: the column used to show predictions next to what
                    # happened. A real device integration leaves this out.
                    "ground_truth": {"outcome": str(r[labels.TARGET])},
                }
                fh.write(json.dumps(record, separators=(",", ":")) + "\n")
                written += 1

    size_mb = out_path.stat().st_size / 1e6
    log.info("wrote %s", out_path)
    log.info("  %s pitches across %d outings, %.1f MB",
             f"{written:,}", len(chosen), size_mb)
    log.info("replay with: go run ./cmd/replay --tape %s --speed 30", out_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
