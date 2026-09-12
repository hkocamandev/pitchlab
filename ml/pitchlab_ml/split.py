"""Temporal train / validation / test splitting.

Random splitting is not merely suboptimal here, it is wrong:

  * consecutive pitches within a plate appearance are strongly dependent, so
    a random split puts pitch 1 in train and pitch 3 in test;
  * arsenal-relative features are pitcher-season aggregates, so the same
    pitcher-season appearing on both sides leaks the aggregate;
  * rule and measurement regimes change over time, so a random split lets the
    model see the future;
  * production always trains on the past and predicts the future -- the
    evaluation should imitate that.

The test split is additionally guarded: reading it requires an explicit opt-in,
because looking at the test score and then changing the model converts it into
a validation set and invalidates the number reported.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass

import pandas as pd

from . import config

log = logging.getLogger(__name__)

TEST_ACCESS_ENV = "PITCHLAB_ALLOW_TEST_SET"


@dataclass(frozen=True)
class Split:
    train: pd.DataFrame
    val: pd.DataFrame
    test: pd.DataFrame

    def summary(self) -> pd.DataFrame:
        rows = []
        for name, part in (("train", self.train), ("val", self.val),
                           ("test", self.test)):
            rows.append({
                "split": name,
                "rows": len(part),
                "start": part["game_date"].min(),
                "end": part["game_date"].max(),
                "pitchers": part["pitcher"].nunique(),
            })
        return pd.DataFrame(rows)


def _as_date(series: pd.Series) -> pd.Series:
    return pd.to_datetime(series).dt.normalize()


def make_split(df: pd.DataFrame) -> Split:
    """Split by game_date. Boundaries fall between days, never inside a game."""
    dates = _as_date(df["game_date"])
    year = dates.dt.year

    train_mask = year.isin(config.TRAIN_SEASONS)
    val_cut = pd.Timestamp(config.VAL_END)
    val_mask = year.eq(config.VAL_SEASON) & dates.le(val_cut)
    test_mask = year.eq(config.TEST_SEASON) & dates.gt(val_cut)

    dropped = int((~(train_mask | val_mask | test_mask)).sum())
    if dropped:
        log.info("split: %s rows outside the configured regime were dropped",
                 f"{dropped:,}")

    split = Split(
        train=df.loc[train_mask].reset_index(drop=True),
        val=df.loc[val_mask].reset_index(drop=True),
        test=df.loc[test_mask].reset_index(drop=True),
    )
    assert_temporal_integrity(split)
    return split


def assert_temporal_integrity(split: Split) -> None:
    """Fail loudly if any split boundary is violated."""
    tr, va, te = split.train, split.val, split.test
    for name, part in (("train", tr), ("val", va), ("test", te)):
        if part.empty:
            raise ValueError(f"{name} split is empty")

    tr_end = _as_date(tr["game_date"]).max()
    va_start = _as_date(va["game_date"]).min()
    va_end = _as_date(va["game_date"]).max()
    te_start = _as_date(te["game_date"]).min()

    if not tr_end < va_start:
        raise AssertionError(f"train ends {tr_end} but val starts {va_start}")
    if not va_end < te_start:
        raise AssertionError(f"val ends {va_end} but test starts {te_start}")

    # A game must never straddle two splits.
    for a_name, a, b_name, b in (("train", tr, "val", va), ("val", va, "test", te)):
        shared = set(a["game_pk"]) & set(b["game_pk"])
        if shared:
            raise AssertionError(
                f"{len(shared)} game_pk values appear in both {a_name} and {b_name}"
            )

    log.info("temporal integrity ok: train<=%s | val %s..%s | test>=%s",
             tr_end.date(), va_start.date(), va_end.date(), te_start.date())


def load_test_set(split: Split) -> pd.DataFrame:
    """Return the test split, but only with an explicit opt-in.

    Set PITCHLAB_ALLOW_TEST_SET=1 in the environment. The friction is the
    point: the test set is read once, after the model is frozen.
    """
    if os.environ.get(TEST_ACCESS_ENV) != "1":
        raise PermissionError(
            "test set access is disabled. This split is read exactly once, "
            "after model selection is complete. If that is what you are doing, "
            f"set {TEST_ACCESS_ENV}=1."
        )
    log.warning("test set accessed -- the reported score is only valid if the "
                "model was frozen before this point")
    return split.test


def rolling_origin_folds(
    df: pd.DataFrame, *, n_folds: int = 6, freq: str = "MS"
) -> list[tuple[pd.Index, pd.Index]]:
    """Expanding-window folds for stability checking.

    Fold k trains on everything before month k and tests on month k. Only ever
    applied to train+validation; the test split is untouched.
    """
    dates = _as_date(df["game_date"])
    months = sorted(dates.dt.to_period("M").unique())
    if len(months) <= n_folds:
        raise ValueError(f"need more than {n_folds} months, have {len(months)}")

    folds: list[tuple[pd.Index, pd.Index]] = []
    for month in months[-n_folds:]:
        period = dates.dt.to_period("M")
        tr_idx = df.index[period < month]
        te_idx = df.index[period == month]
        if len(tr_idx) and len(te_idx):
            folds.append((tr_idx, te_idx))
    return folds
