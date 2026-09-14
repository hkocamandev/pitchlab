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
import random
import time
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


def _fetch_once(start: str, end: str, *, verbose: bool) -> pd.DataFrame:
    import pybaseball

    df = pybaseball.statcast(start_dt=start, end_dt=end, verbose=verbose)
    if df is None or df.empty:
        return pd.DataFrame()

    # pybaseball returns the index as a leftover range; normalize it.
    df = df.reset_index(drop=True)

    if config.REGULAR_SEASON_ONLY and "game_type" in df.columns:
        df = df[df["game_type"] == "R"].reset_index(drop=True)

    return df


def fetch_range(
    start: str,
    end: str,
    *,
    verbose: bool = False,
    attempts: int = 4,
    split_on_failure: bool = True,
) -> pd.DataFrame:
    """Fetch pitch-level Statcast rows for an inclusive date range.

    Savant intermittently answers with an error page instead of CSV, which
    surfaces as a pandas ParserError rather than an HTTP failure. That is
    transient and usually rate-limit related, so retry with exponential
    backoff. If a range still fails, fall back to fetching it one day at a
    time: pybaseball fetches day-by-day internally anyway, so a single bad
    day should not cost the whole month.
    """
    enable_cache()

    for attempt in range(1, attempts + 1):
        try:
            log.info("fetching statcast %s..%s (attempt %d)", start, end, attempt)
            return _fetch_once(start, end, verbose=verbose)
        except Exception as exc:
            if attempt == attempts:
                log.warning("range %s..%s failed after %d attempts: %s",
                            start, end, attempts, exc)
                break
            delay = 5 * 2 ** (attempt - 1) + random.uniform(0, 3)
            log.warning("fetch failed (%s); retrying in %.1fs", exc, delay)
            time.sleep(delay)

    if not split_on_failure:
        raise RuntimeError(f"could not fetch statcast range {start}..{end}")

    log.warning("falling back to day-by-day for %s..%s", start, end)
    frames: list[pd.DataFrame] = []
    skipped: list[str] = []
    day = date.fromisoformat(start)
    last = date.fromisoformat(end)
    while day <= last:
        iso = day.isoformat()
        try:
            frames.append(
                fetch_range(iso, iso, verbose=False, attempts=3,
                            split_on_failure=False)
            )
        except Exception as exc:
            log.error("skipping %s: %s", iso, exc)
            skipped.append(iso)
        day += timedelta(days=1)

    if skipped:
        log.error("SKIPPED %d day(s) in %s..%s: %s",
                  len(skipped), start, end, ", ".join(skipped))

    return (
        pd.concat(frames, ignore_index=True) if frames else pd.DataFrame()
    )


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
