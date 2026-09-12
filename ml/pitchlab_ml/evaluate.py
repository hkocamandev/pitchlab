"""Evaluation metrics.

Log loss is the primary metric because this system shows probabilities to
users rather than a single predicted class; accuracy would reward confident
wrong answers. Calibration is reported alongside it for the same reason: an
uncalibrated 40% is misinformation, not a prediction.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
import pandas as pd
from sklearn.metrics import roc_auc_score

from . import config

# Probabilities are clipped before taking logs so a confident miss costs a
# large but finite amount rather than infinity.
_EPS = 1e-15

# If discrimination is this good, suspect leakage before celebrating.
SUSPICIOUS_AUC = 0.95


@dataclass
class Metrics:
    log_loss: float
    brier_per_class: dict[str, float]
    auc_ovr: dict[str, float]
    ece: float
    accuracy: float
    n_rows: int

    def to_dict(self) -> dict:
        return {
            "log_loss": self.log_loss,
            "brier_per_class": self.brier_per_class,
            "auc_ovr": self.auc_ovr,
            "ece": self.ece,
            "accuracy": self.accuracy,
            "n_rows": self.n_rows,
        }

    @property
    def suspicious_classes(self) -> list[str]:
        return sorted(k for k, v in self.auc_ovr.items() if v > SUSPICIOUS_AUC)


def _one_hot(y: pd.Series) -> np.ndarray:
    codes = pd.Categorical(y.astype(str), categories=list(config.CLASS_LABELS)).codes
    out = np.zeros((len(y), len(config.CLASS_LABELS)))
    valid = codes >= 0
    out[np.arange(len(y))[valid], codes[valid]] = 1.0
    return out


def multiclass_log_loss(y_true: pd.Series, proba: np.ndarray) -> float:
    """Cross-entropy against CLASS_LABELS column order.

    Deliberately not sklearn.metrics.log_loss: that function re-sorts the
    `labels` argument lexicographically and then assumes the probability
    columns follow that sorted order. Our column order is CLASS_LABELS, which
    is not alphabetical, so sklearn silently pairs each class with the wrong
    column. On a single row where the model puts 0.96 on the true class it
    reports 4.6052 instead of 0.0408.
    """
    onehot = _one_hot(y_true)
    if proba.shape != onehot.shape:
        raise ValueError(
            f"shape mismatch: proba {proba.shape} vs labels {onehot.shape}"
        )
    p = np.clip(proba, _EPS, 1.0)
    p = p / p.sum(axis=1, keepdims=True)
    return float(-np.mean(np.sum(onehot * np.log(p), axis=1)))


def expected_calibration_error(
    y_true: pd.Series, proba: np.ndarray, *, n_bins: int = 20
) -> float:
    """Confidence-weighted gap between predicted and observed frequency.

    Computed on the predicted (max-probability) class, which is the number a
    user actually sees emphasised in the UI.
    """
    codes = pd.Categorical(
        y_true.astype(str), categories=list(config.CLASS_LABELS)
    ).codes
    confidence = proba.max(axis=1)
    predicted = proba.argmax(axis=1)
    correct = (predicted == codes).astype(float)

    edges = np.linspace(0.0, 1.0, n_bins + 1)
    ece = 0.0
    for lo, hi in zip(edges[:-1], edges[1:]):
        in_bin = (confidence > lo) & (confidence <= hi)
        if not in_bin.any():
            continue
        ece += in_bin.mean() * abs(correct[in_bin].mean() - confidence[in_bin].mean())
    return float(ece)


def reliability_table(
    y_true: pd.Series, proba: np.ndarray, class_name: str, *, n_bins: int = 10
) -> pd.DataFrame:
    """Per-bin predicted vs observed frequency for one class."""
    idx = list(config.CLASS_LABELS).index(class_name)
    p = proba[:, idx]
    actual = (y_true.astype(str) == class_name).to_numpy(dtype=float)

    bins = np.clip(np.digitize(p, np.linspace(0, 1, n_bins + 1)[1:-1]), 0, n_bins - 1)
    rows = []
    for b in range(n_bins):
        m = bins == b
        if not m.any():
            continue
        rows.append({
            "bin": f"{b / n_bins:.1f}-{(b + 1) / n_bins:.1f}",
            "n": int(m.sum()),
            "predicted": float(p[m].mean()),
            "observed": float(actual[m].mean()),
            "gap": float(actual[m].mean() - p[m].mean()),
        })
    return pd.DataFrame(rows)


def evaluate(y_true: pd.Series, proba: np.ndarray) -> Metrics:
    labels = list(config.CLASS_LABELS)
    y = y_true.astype(str)
    onehot = _one_hot(y)

    brier = {
        name: float(np.mean((proba[:, i] - onehot[:, i]) ** 2))
        for i, name in enumerate(labels)
    }

    auc: dict[str, float] = {}
    for i, name in enumerate(labels):
        actual = onehot[:, i]
        # A class absent from this slice has no meaningful AUC.
        auc[name] = (
            float(roc_auc_score(actual, proba[:, i]))
            if 0 < actual.sum() < len(actual)
            else float("nan")
        )

    return Metrics(
        log_loss=multiclass_log_loss(y, proba),
        brier_per_class=brier,
        auc_ovr=auc,
        ece=expected_calibration_error(y, proba),
        accuracy=float((proba.argmax(axis=1) == onehot.argmax(axis=1)).mean()),
        n_rows=len(y),
    )


def comparison_table(results: dict[str, Metrics], reference: str) -> pd.DataFrame:
    """Rank models against a reference baseline's log loss."""
    ref = results[reference].log_loss
    rows = []
    for name, m in results.items():
        rows.append({
            "model": name,
            "log_loss": m.log_loss,
            "vs_reference_%": (ref - m.log_loss) / ref * 100.0,
            "ece": m.ece,
            "accuracy": m.accuracy,
        })
    return pd.DataFrame(rows).sort_values("log_loss").reset_index(drop=True)
