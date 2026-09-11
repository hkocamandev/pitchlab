"""Statcast ingestion via pybaseball.

Data belongs to MLB Advanced Media and is licensed for individual,
non-commercial, non-bulk use. This module therefore:
  * enables pybaseball's on-disk cache so a range is fetched at most once,
  * fetches in bounded chunks rather than as one bulk request,
  * writes date-stamped local snapshots so results stay reproducible even
    though Statcast is revised retroactively.

Nothing here writes to the repository tree outside of data/, which is
gitignored.
"""

from __future__ import annotations

import logging
from datetime import date, timedelta

import pandas as pd

from . import config

log = logging.getLogger(__name__)


def enable_cache() -> None:
    """Point pybaseball's cache at our data directory and turn it on."""
    import pybaseball

    try:
        pybaseball.cache.config.cache_directory = str(config.CACHE_DIR)
        pybaseball.cache.config.save()
    except Exception:  # pragma: no cover - older pybaseball layouts
        log.debug("could not relocate pybaseball cache; using its default")
    pybaseball.cache.enable()


def fetch_range(start: str, end: str, *, verbose: bool = False) -> pd.DataFrame:
    """Fetch pitch-level Statcast rows for an inclusive date range."""
    import pybaseball

    enable_cache()
    log.info("fetching statcast %s..%s", start, end)
    df = pybaseball.statcast(start_dt=start, end_dt=end, verbose=verbose)

    if df is None or df.empty:
        return pd.DataFrame()

    # pybaseball returns the index as a leftover range; normalize it.
    df = df.reset_index(drop=True)

    if config.REGULAR_SEASON_ONLY and "game_type" in df.columns:
        df = df[df["game_type"] == "R"].reset_index(drop=True)

    return df


def month_chunks(start: date, end: date) -> list[tuple[str, str]]:
    """Split a date range into calendar-month chunks (inclusive bounds)."""
    chunks: list[tuple[str, str]] = []
    cursor = start
    while cursor <= end:
        if cursor.month == 12:
            next_month = date(cursor.year + 1, 1, 1)
        else:
            next_month = date(cursor.year, cursor.month + 1, 1)
        chunk_end = min(next_month - timedelta(days=1), end)
        chunks.append((cursor.isoformat(), chunk_end.isoformat()))
        cursor = chunk_end + timedelta(days=1)
    return chunks
