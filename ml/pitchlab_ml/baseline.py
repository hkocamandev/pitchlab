"""Prior-based baselines.

The model has to beat these to be worth deploying, and by a believable margin.
This problem is irreducibly noisy -- whether a batter swings is substantially
unpredictable -- so the realistic bar is a single-digit to low-double-digit
percentage improvement in log loss. A very large jump is evidence of leakage,
not of skill, and the evaluation treats it that way.

  B0  global class prior
  B1  prior conditioned on the count
  B2  prior conditioned on count x pitch group
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field

import numpy as np
import pandas as pd

from . import config
from .labels import TARGET

log = logging.getLogger(__name__)


@dataclass
class PriorBaseline:
    """Empirical class distribution, optionally conditioned on some columns.

    Cells with too few observations fall back to the global prior rather than
    trusting a noisy estimate.
    """

    by: tuple[str, ...] = ()
    min_rows: int = 100
    name: str = "baseline"
    _table: pd.DataFrame | None = field(default=None, repr=False)
    _global: np.ndarray | None = field(default=None, repr=False)

    def fit(self, df: pd.DataFrame) -> PriorBaseline:
        target = df[TARGET].astype(str)
        counts = target.value_counts().reindex(config.CLASS_LABELS).fillna(0.0)
        self._global = (counts / counts.sum()).to_numpy(dtype=float)

        if self.by:
            keys = [df[c].astype(str) for c in self.by]
            grouped = (
                pd.crosstab(index=keys, columns=target, normalize="index")
                .reindex(columns=list(config.CLASS_LABELS))
                .fillna(0.0)
            )
            sizes = pd.crosstab(index=keys, columns=target).sum(axis=1)
            keep = sizes[sizes >= self.min_rows].index
            self._table = grouped.loc[grouped.index.isin(keep)]
            log.info("%s: %d conditioned cells kept of %d",
                     self.name, len(self._table), len(grouped))
        return self

    def predict_proba(self, df: pd.DataFrame) -> np.ndarray:
        if self._global is None:
            raise RuntimeError("baseline is not fitted")

        n = len(df)
        out = np.tile(self._global, (n, 1))
        if not self.by or self._table is None or self._table.empty:
            return out

        keys = [df[c].astype(str) for c in self.by]
        index = keys[0] if len(keys) == 1 else pd.MultiIndex.from_arrays(keys)
        matched = self._table.reindex(index)
        found = matched.notna().all(axis=1).to_numpy()
        out[found] = matched.to_numpy(dtype=float)[found]
        return out


def build_baselines() -> dict[str, PriorBaseline]:
    return {
        "B0_global": PriorBaseline(by=(), name="B0_global"),
        "B1_count": PriorBaseline(by=("count_state",), name="B1_count"),
        "B2_count_group": PriorBaseline(
            by=("count_state", "pitch_group"), name="B2_count_group"
        ),
    }
