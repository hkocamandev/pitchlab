"""Training pipeline for the 5-class pitch outcome model.

Produces, per variant:
  * a LightGBM booster
  * a run-value table (training split only)
  * a score scaler (training split only)
  * a model card recording the data snapshot, splits, feature contract and
    metrics, so any stored prediction stays interpretable later
"""

from __future__ import annotations

import json
import logging
import subprocess
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import lightgbm as lgb
import numpy as np
import pandas as pd
import pyarrow.parquet as pq
from sklearn.metrics import roc_auc_score

from . import baseline, config, evaluate, features, labels, runvalue, split

log = logging.getLogger(__name__)

LGB_PARAMS: dict = {
    "objective": "multiclass",
    "num_class": len(config.CLASS_LABELS),
    "metric": "multi_logloss",
    "learning_rate": 0.05,
    "num_leaves": 96,
    "min_data_in_leaf": 400,
    "feature_fraction": 0.85,
    "bagging_fraction": 0.85,
    "bagging_freq": 1,
    "lambda_l2": 1.0,
    "max_cat_threshold": 32,
    "cat_smooth": 20.0,
    "num_threads": 0,
    "verbosity": -1,
    "seed": config.RANDOM_SEED,
}

NUM_BOOST_ROUND = 3000
EARLY_STOPPING_ROUNDS = 100


def _git_sha() -> str | None:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=config.REPO_ROOT, stderr=subprocess.DEVNULL, text=True,
        ).strip()
    except Exception:  # noqa: BLE001
        return None


@dataclass
class TrainedModel:
    variant: str
    version: str
    booster: lgb.Booster
    feature_columns: tuple[str, ...]
    categorical_columns: tuple[str, ...]
    scaler: runvalue.ScoreScaler
    metrics: dict
    target: str = "outcome_5class"
    rv_table: runvalue.RunValueTable | None = None

    @property
    def artifact_dir(self) -> Path:
        return config.MODELS_DIR / f"{self.version}-{self.variant}"

    def save(self) -> Path:
        d = self.artifact_dir
        d.mkdir(parents=True, exist_ok=True)
        self.booster.save_model(str(d / "model.txt"))
        (d / "model_card.json").write_text(
            json.dumps(self.card(), indent=2, default=str), encoding="utf-8"
        )
        log.info("saved %s", d)
        return d

    def card(self) -> dict:
        card = {
            "version": self.version,
            "variant": self.variant,
            "target": self.target,
            "algorithm": (
                "lightgbm-multiclass" if self.rv_table is not None
                else "lightgbm-binary"
            ),
            "feature_columns": list(self.feature_columns),
            "categorical_columns": list(self.categorical_columns),
            "params": LGB_PARAMS,
            "best_iteration": self.booster.best_iteration,
            "score_scaler": self.scaler.to_dict(),
            "metrics": self.metrics,
            "git_sha": _git_sha(),
            "created_at": datetime.now(timezone.utc).isoformat(),
        }
        if self.rv_table is not None:
            card["class_labels"] = list(config.CLASS_LABELS)
            card["run_value_table"] = self.rv_table.to_dict()
        return card


def prepare(snapshot: Path) -> pd.DataFrame:
    """Load a raw snapshot and produce a labelled, feature-complete frame."""
    log.info("loading %s", snapshot.name)
    wanted = features.required_raw_columns()
    available = set(pq.read_schema(snapshot).names)
    missing = [c for c in wanted if c not in available]
    if missing:
        raise KeyError(f"snapshot is missing required columns: {missing}")
    raw = pd.read_parquet(snapshot, columns=wanted)
    log.info("  %s raw rows, %d columns loaded", f"{len(raw):,}", raw.shape[1])

    labelled = labels.attach_labels(raw)
    built = features.build_features(labelled)

    reference = features.build_arsenal_reference(built)
    log.info("arsenal reference: %d pitcher-seasons", len(reference))
    built = features.attach_arsenal_features(built, reference)

    return built


def _dataset(df: pd.DataFrame, variant: str) -> tuple[pd.DataFrame, np.ndarray]:
    X = features.select_matrix(df, variant)
    y = pd.Categorical(
        df[labels.TARGET].astype(str), categories=list(config.CLASS_LABELS)
    ).codes
    return X, y


def swings_only(df: pd.DataFrame) -> pd.DataFrame:
    """Rows where the batter offered, with a binary whiff label attached."""
    out = df[df[labels.TARGET].astype(str).isin(features.SWING_CLASSES)].copy()
    out["is_whiff"] = (
        out[labels.TARGET].astype(str) == config.CLASS_SWINGING_STRIKE
    ).astype("int8")
    return out


def train_stuff_model(
    parts: split.Split, version: str
) -> tuple[TrainedModel, dict[str, float]]:
    """P(whiff | swing) from physical features alone -- the Stuff+ question.

    Trained on swings only. The resulting probability is normalized to the
    conventional mean-100 / SD-10 scale over the training population, so
    StuffScore answers "how hard is this pitch to hit when offered at",
    independent of where it was located or of the game situation.
    """
    log.info("=== training variant 'stuff' (whiff | swing) ===")
    tr, va = swings_only(parts.train), swings_only(parts.val)
    log.info("swings: train %s (%.1f%% of pitches), val %s",
             f"{len(tr):,}", 100 * len(tr) / len(parts.train), f"{len(va):,}")
    log.info("whiff rate: train %.4f, val %.4f",
             tr["is_whiff"].mean(), va["is_whiff"].mean())

    X_tr = features.select_matrix(tr, "stuff")
    X_va = features.select_matrix(va, "stuff")
    cat = tuple(c for c in X_tr.columns if c in features.CATEGORICAL_FEATURES)

    params = {k: v for k, v in LGB_PARAMS.items() if k != "num_class"}
    params.update(objective="binary", metric="binary_logloss")

    booster = lgb.train(
        params,
        lgb.Dataset(X_tr, label=tr["is_whiff"], categorical_feature=list(cat)),
        num_boost_round=NUM_BOOST_ROUND,
        valid_sets=[lgb.Dataset(X_va, label=va["is_whiff"],
                                categorical_feature=list(cat))],
        valid_names=["val"],
        callbacks=[
            lgb.early_stopping(EARLY_STOPPING_ROUNDS, verbose=False),
            lgb.log_evaluation(200),
        ],
    )
    log.info("stopped at iteration %d", booster.best_iteration)

    p_va = booster.predict(X_va, num_iteration=booster.best_iteration)
    p_tr = booster.predict(X_tr, num_iteration=booster.best_iteration)

    base_rate = float(tr["is_whiff"].mean())
    metrics = {
        "auc": float(roc_auc_score(va["is_whiff"], p_va)),
        "baseline_auc": 0.5,
        "log_loss": float(_binary_log_loss(va["is_whiff"].to_numpy(), p_va)),
        "baseline_log_loss": float(
            _binary_log_loss(va["is_whiff"].to_numpy(),
                             np.full(len(va), base_rate))
        ),
        "base_rate": base_rate,
        "n_train": len(tr),
        "n_val": len(va),
    }
    metrics["log_loss_improvement_pct"] = (
        (metrics["baseline_log_loss"] - metrics["log_loss"])
        / metrics["baseline_log_loss"] * 100.0
    )
    log.info("stuff: AUC %.4f, log loss %.4f (base %.4f, %+.2f%%)",
             metrics["auc"], metrics["log_loss"], metrics["baseline_log_loss"],
             metrics["log_loss_improvement_pct"])

    # Higher whiff probability must mean a higher score, so negate before
    # reusing the shared scaler (which is written for "lower is better").
    scaler = runvalue.fit_score_scaler(-p_tr)

    model = TrainedModel(
        variant="stuff",
        version=version,
        booster=booster,
        feature_columns=tuple(X_tr.columns),
        categorical_columns=cat,
        scaler=scaler,
        metrics=metrics,
        target="whiff_given_swing",
        rv_table=None,
    )
    return model, metrics


def _binary_log_loss(y: np.ndarray, p: np.ndarray) -> float:
    p = np.clip(p, 1e-15, 1 - 1e-15)
    return float(-np.mean(y * np.log(p) + (1 - y) * np.log(1 - p)))


def train_variant(
    parts: split.Split, variant: str, version: str
) -> tuple[TrainedModel, dict[str, evaluate.Metrics]]:
    log.info("=== training variant %r ===", variant)

    X_tr, y_tr = _dataset(parts.train, variant)
    X_va, y_va = _dataset(parts.val, variant)
    cat_cols = tuple(c for c in X_tr.columns if c in features.CATEGORICAL_FEATURES)

    log.info("train %s x %d | val %s", f"{len(X_tr):,}", X_tr.shape[1], f"{len(X_va):,}")

    booster = lgb.train(
        LGB_PARAMS,
        lgb.Dataset(X_tr, label=y_tr, categorical_feature=list(cat_cols)),
        num_boost_round=NUM_BOOST_ROUND,
        valid_sets=[lgb.Dataset(X_va, label=y_va, categorical_feature=list(cat_cols))],
        valid_names=["val"],
        callbacks=[
            lgb.early_stopping(EARLY_STOPPING_ROUNDS, verbose=False),
            lgb.log_evaluation(200),
        ],
    )
    log.info("stopped at iteration %d", booster.best_iteration)

    # --- baselines, fitted on train only --------------------------------
    results: dict[str, evaluate.Metrics] = {}
    for name, bl in baseline.build_baselines().items():
        bl.fit(parts.train)
        results[name] = evaluate.evaluate(
            parts.val[labels.TARGET], bl.predict_proba(parts.val)
        )

    proba_va = booster.predict(X_va, num_iteration=booster.best_iteration)
    results[f"model_{variant}"] = evaluate.evaluate(parts.val[labels.TARGET], proba_va)

    # --- run value and score scale, from train only ----------------------
    rv_table = runvalue.build_run_value_table(parts.train)
    proba_tr = booster.predict(X_tr, num_iteration=booster.best_iteration)
    xrv_tr = runvalue.expected_run_value(
        proba_tr, parts.train["count_state"], rv_table
    )
    scaler = runvalue.fit_score_scaler(xrv_tr)
    log.info("score scale: mu=%.5f sigma=%.5f", scaler.mu, scaler.sigma)

    model = TrainedModel(
        variant=variant,
        version=version,
        booster=booster,
        feature_columns=tuple(X_tr.columns),
        categorical_columns=cat_cols,
        rv_table=rv_table,
        scaler=scaler,
        metrics={name: m.to_dict() for name, m in results.items()},
    )
    return model, results


def score_pitches(model: TrainedModel, df: pd.DataFrame) -> pd.DataFrame:
    """Full inference chain: probabilities -> quantity -> presentation score."""
    X = features.select_matrix(df, model.variant)
    proba = model.booster.predict(X, num_iteration=model.booster.best_iteration)

    if model.rv_table is None:
        # Binary stuff model: the scored quantity is -P(whiff | swing), so
        # that "lower is better" holds and the shared scaler applies.
        out = pd.DataFrame({"whiff_prob": proba}, index=df.index)
        out["score"] = model.scaler.to_score(-proba)
        return out

    xrv = runvalue.expected_run_value(proba, df["count_state"], model.rv_table)
    out = pd.DataFrame(proba, columns=list(config.CLASS_LABELS), index=df.index)
    out["xrv"] = xrv
    out["score"] = model.scaler.to_score(xrv)
    out["predicted_class"] = [config.CLASS_LABELS[i] for i in proba.argmax(axis=1)]
    return out
