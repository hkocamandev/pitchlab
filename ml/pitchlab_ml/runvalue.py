"""Run-value table, expected run value, and the presentation score.

This module is what turns a probability vector into something a coach can
read. The chain is:

    P(class | features)  ->  xRV (runs)  ->  PitchScore (mean 100, SD 10)

Every step is estimated from data. There are no hand-chosen weights.

A note on `delta_run_exp`, which is on the leakage blacklist: it is banned as
a *feature* because it is a function of the pitch's own outcome. Averaging it
over (class x count) across the training split produces 60 population
constants -- the answer to "league-wide, what is a whiff worth in an 0-2
count" -- which is a lookup table, not per-pitch information. The test is
whether scoring a new pitch requires knowing that pitch's delta_run_exp: it
does not. Only the count and the model's probabilities are needed.

The table is built from the training split alone, so validation and test stay
untouched.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

import numpy as np
import pandas as pd

from . import config
from .labels import TARGET

log = logging.getLogger(__name__)

COUNT_STATES: tuple[str, ...] = tuple(
    f"{b}-{s}" for b in range(4) for s in range(3)
)

# Sign sanity: from the batting team's perspective a whiff must cost runs and
# a ball in play must gain them. If this inverts, something upstream changed.
EXPECTED_SIGNS: dict[str, int] = {
    config.CLASS_SWINGING_STRIKE: -1,
    config.CLASS_CALLED_STRIKE: -1,
    config.CLASS_BALL: +1,
    config.CLASS_IN_PLAY: +1,
}


@dataclass(frozen=True)
class RunValueTable:
    """5 classes x 12 count states of mean run-expectancy change."""

    values: pd.DataFrame  # index = class, columns = count_state
    fallback: pd.Series  # per-class mean, for unseen count states

    def to_dict(self) -> dict:
        return {
            "values": self.values.to_dict(orient="index"),
            "fallback": self.fallback.to_dict(),
        }

    @classmethod
    def from_dict(cls, payload: dict) -> "RunValueTable":
        return cls(
            values=pd.DataFrame.from_dict(payload["values"], orient="index"),
            fallback=pd.Series(payload["fallback"]),
        )

    def lookup(self, count_states: pd.Series) -> pd.DataFrame:
        """Return an (n_rows x n_classes) matrix of run values."""
        states = count_states.astype(str)
        table = self.values.T  # index = count_state, columns = class
        out = table.reindex(states).reset_index(drop=True)
        # Unknown count state -> per-class league mean.
        missing = out.isna().any(axis=1)
        if missing.any():
            log.warning("run value: %d rows had an unknown count state",
                        int(missing.sum()))
            for cls_name in out.columns:
                out.loc[missing, cls_name] = self.fallback[cls_name]
        return out[list(config.CLASS_LABELS)]


def build_run_value_table(train: pd.DataFrame, *, min_rows: int = 200) -> RunValueTable:
    """Empirical mean delta_run_exp per (class, count). Training split only."""
    needed = {TARGET, "count_state", "delta_run_exp"}
    missing = needed - set(train.columns)
    if missing:
        raise KeyError(f"run value table needs columns: {sorted(missing)}")

    df = train.loc[train["delta_run_exp"].notna(),
                   [TARGET, "count_state", "delta_run_exp"]].copy()
    df[TARGET] = df[TARGET].astype(str)
    df["count_state"] = df["count_state"].astype(str)

    grouped = df.groupby([TARGET, "count_state"])["delta_run_exp"]
    means = grouped.mean().unstack("count_state")
    counts = grouped.size().unstack("count_state")

    fallback = df.groupby(TARGET)["delta_run_exp"].mean()

    # Thin cells are noise; fall back to the class mean rather than trust them.
    thin = counts < min_rows
    if thin.to_numpy().any():
        log.info("run value: %d cell(s) below %d rows, using class mean",
                 int(thin.to_numpy().sum()), min_rows)
        means = means.where(~thin, other=np.nan)

    means = means.reindex(index=list(config.CLASS_LABELS), columns=list(COUNT_STATES))
    for cls_name in means.index:
        means.loc[cls_name] = means.loc[cls_name].fillna(fallback.get(cls_name, 0.0))

    table = RunValueTable(values=means, fallback=fallback.reindex(config.CLASS_LABELS))
    _check_signs(table)
    return table


def _check_signs(table: RunValueTable) -> None:
    for cls_name, expected in EXPECTED_SIGNS.items():
        observed = table.fallback.get(cls_name)
        if observed is None or np.isnan(observed):
            continue
        if np.sign(observed) != expected:
            raise AssertionError(
                f"run value sign check failed for {cls_name}: expected "
                f"{'positive' if expected > 0 else 'negative'}, got {observed:+.4f}. "
                "delta_run_exp is defined from the batting team's perspective; "
                "if this fires, that convention changed."
            )
    log.info("run value sign check passed")


def expected_run_value(
    probabilities: np.ndarray, count_states: pd.Series, table: RunValueTable
) -> np.ndarray:
    """xRV = sum over classes of P(class) * RV[class, count].

    Returned from the pitcher's perspective is *not* done here: the value keeps
    delta_run_exp's batting-team sign, so lower is better for the pitcher.
    """
    rv = table.lookup(count_states).to_numpy()
    if probabilities.shape != rv.shape:
        raise ValueError(
            f"shape mismatch: probabilities {probabilities.shape} vs run "
            f"values {rv.shape}"
        )
    return np.einsum("ij,ij->i", probabilities, rv)


@dataclass(frozen=True)
class ScoreScaler:
    """Maps xRV onto the conventional mean-100 / SD-10 presentation scale.

    Higher is better for the pitcher, so the sign is inverted: a pitch that
    lowers the batting team's run expectancy scores above 100.

    mu and sigma are frozen from the training split into the model artifact.
    Recomputing them at serving time would let scores drift silently as the
    population of scored pitches changes.
    """

    mu: float
    sigma: float

    def to_score(self, xrv: np.ndarray) -> np.ndarray:
        return 100.0 + 10.0 * ((self.mu - xrv) / self.sigma)

    def to_xrv(self, score: np.ndarray) -> np.ndarray:
        """Inverse, so a displayed score can always be traced back."""
        return self.mu - (np.asarray(score) - 100.0) * self.sigma / 10.0

    def to_dict(self) -> dict:
        return {"mu": float(self.mu), "sigma": float(self.sigma)}

    @classmethod
    def from_dict(cls, payload: dict) -> "ScoreScaler":
        return cls(mu=float(payload["mu"]), sigma=float(payload["sigma"]))


def fit_score_scaler(xrv_train: np.ndarray) -> ScoreScaler:
    sigma = float(np.std(xrv_train))
    if sigma <= 0:
        raise ValueError("xRV has zero variance; cannot build a score scale")
    return ScoreScaler(mu=float(np.mean(xrv_train)), sigma=sigma)
