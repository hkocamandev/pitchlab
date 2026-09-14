"""Fetch the 2023-2025 regular seasons and write a dated raw snapshot.

Fetches in calendar-month chunks, caching each chunk to its own parquet file
so an interrupted run resumes instead of restarting. Statcast is revised
retroactively, so the combined output is date-stamped and treated as an
immutable snapshot: models record which snapshot they were trained on.

Usage:
    .venv/bin/python ml/scripts/pull_seasons.py
    .venv/bin/python ml/scripts/pull_seasons.py --seasons 2026   # drift set
"""

from __future__ import annotations

import argparse
import logging
import sys
from datetime import date, datetime
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, ingest

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)-5s %(message)s"
)
log = logging.getLogger("pull")

# Generous bounds: the game_type == 'R' filter in ingest.fetch_range drops
# spring training and postseason, so these only need to contain the season.
# Mid-March is included because 2024 and 2025 opened with international series.
SEASON_BOUNDS: dict[int, tuple[date, date]] = {
    # 2022 is fetched for the arsenal reference only. Without it the 2023
    # pitches -- half the training set -- would have no previous-season
    # fastball baseline, leaving four features missing for that half and
    # present for the other, which is a distribution shift inside training.
    # A pitcher's own fastball velocity and movement is a personal baseline,
    # not a league-regime quantity, so the pre-2023 rule changes do not make
    # 2022 unusable for this narrow purpose. make_split drops these rows.
    2022: (date(2022, 4, 1), date(2022, 10, 5)),
    2023: (date(2023, 3, 15), date(2023, 10, 5)),
    2024: (date(2024, 3, 15), date(2024, 10, 5)),
    2025: (date(2025, 3, 15), date(2025, 10, 5)),
    2026: (date(2026, 3, 15), date(2026, 9, 10)),
}

CHUNK_DIR = config.RAW_DIR / "chunks"
CHUNK_DIR.mkdir(parents=True, exist_ok=True)


def pull_chunk(start: str, end: str) -> pd.DataFrame:
    path = CHUNK_DIR / f"statcast_{start}_{end}.parquet"
    if path.exists():
        df = pd.read_parquet(path)
        log.info("chunk %s..%s cached (%d rows)", start, end, len(df))
        return df

    df = ingest.fetch_range(start, end)
    df.to_parquet(path, compression="snappy", index=False)
    log.info("chunk %s..%s fetched (%d rows, %.1f MB)", start, end, len(df),
             path.stat().st_size / 1e6)
    return df


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--seasons", type=int, nargs="+",
                    default=[*config.TRAIN_SEASONS, config.VAL_SEASON])
    ap.add_argument("--label", default=None,
                    help="output filename label (default: derived from seasons)")
    args = ap.parse_args()

    seasons = sorted(set(args.seasons))
    unknown = [s for s in seasons if s not in SEASON_BOUNDS]
    if unknown:
        log.error("no date bounds configured for season(s): %s", unknown)
        return 1

    frames: list[pd.DataFrame] = []
    for season in seasons:
        start, end = SEASON_BOUNDS[season]
        log.info("=== season %d (%s .. %s) ===", season, start, end)
        for c_start, c_end in ingest.month_chunks(start, end):
            df = pull_chunk(c_start, c_end)
            if not df.empty:
                frames.append(df)

    if not frames:
        log.error("no data fetched")
        return 1

    combined = pd.concat(frames, ignore_index=True)

    # A game can straddle a chunk boundary only if the boundary splits a date,
    # which month chunks never do -- but dedupe defensively on the natural key.
    before = len(combined)
    combined = combined.drop_duplicates(
        subset=["game_pk", "at_bat_number", "pitch_number"], keep="first"
    ).reset_index(drop=True)
    if before != len(combined):
        log.warning("dropped %d duplicate pitch rows", before - len(combined))

    combined = combined.sort_values(
        ["game_date", "game_pk", "at_bat_number", "pitch_number"]
    ).reset_index(drop=True)

    label = args.label or "-".join(str(s) for s in seasons)
    stamp = datetime.now().strftime("%Y%m%d")
    out = config.RAW_DIR / f"statcast_{label}_{stamp}.parquet"
    combined.to_parquet(out, compression="snappy", index=False)

    log.info("wrote %s", out.name)
    log.info("  rows     : %s", f"{len(combined):,}")
    log.info("  columns  : %d", combined.shape[1])
    log.info("  size     : %.1f MB", out.stat().st_size / 1e6)
    log.info("  date span: %s .. %s",
             combined["game_date"].min(), combined["game_date"].max())
    log.info("  per season:")
    for season, n in combined["game_year"].value_counts().sort_index().items():
        log.info("    %s: %s pitches", season, f"{n:,}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
