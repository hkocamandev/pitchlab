"""Loading and holding trained model artifacts.

A prediction is only interpretable alongside the model that produced it, so
everything needed to reproduce a score travels together: the booster, the
ordered feature contract, the run-value table and the score scaler.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass
from pathlib import Path

import lightgbm as lgb
import numpy as np
import pandas as pd
from pitchlab_ml import config, features, runvalue

log = logging.getLogger(__name__)

TARGET_OUTCOME = "outcome_5class"
TARGET_WHIFF = "whiff_given_swing"


@dataclass
class LoadedModel:
    """One trained variant, ready to serve."""

    version: str
    variant: str
    target: str
    algorithm: str
    booster: lgb.Booster
    # The ordered feature contract. Serving must present columns in exactly
    # this order; a mismatch is a silent wrong answer, so it is checked.
    feature_columns: tuple[str, ...]
    categorical_columns: frozenset[str]
    scaler: runvalue.ScoreScaler
    rv_table: runvalue.RunValueTable | None
    best_iteration: int
    metrics: dict
    git_sha: str | None
    created_at: str

    @property
    def key(self) -> str:
        return self.variant

    def build_frame(self, rows: list[dict]) -> pd.DataFrame:
        """Turn request payloads into the exact matrix the booster expects.

        Missing values stay missing. LightGBM handles NaN natively and the
        absence of a reading is itself information -- imputing a mean would
        invent a measurement the device never took.
        """
        df = pd.DataFrame(rows)

        for col in self.feature_columns:
            if col not in df.columns:
                df[col] = np.nan

        df = df.loc[:, list(self.feature_columns)]

        for col in df.columns:
            if col in self.categorical_columns:
                df[col] = df[col].astype("category")
            else:
                df[col] = pd.to_numeric(df[col], errors="coerce").astype("float64")

        return df

    def predict(self, frame: pd.DataFrame) -> np.ndarray:
        return self.booster.predict(frame, num_iteration=self.best_iteration)


def load_model(directory: Path) -> LoadedModel:
    card_path = directory / "model_card.json"
    model_path = directory / "model.txt"

    card = json.loads(card_path.read_text(encoding="utf-8"))
    booster = lgb.Booster(model_file=str(model_path))

    feature_columns = tuple(card["feature_columns"])

    # The booster carries its own feature names. If they disagree with the
    # card, the artifact pair is mismatched and every score it produces would
    # be quietly wrong -- better to refuse to start.
    booster_features = tuple(booster.feature_name())
    if booster_features != feature_columns:
        raise ValueError(
            f"{directory.name}: model card and booster disagree on features.\n"
            f"  card:    {feature_columns}\n"
            f"  booster: {booster_features}"
        )

    rv_table = None
    if card.get("run_value_table"):
        rv_table = runvalue.RunValueTable.from_dict(card["run_value_table"])

    target = card.get("target", TARGET_OUTCOME)
    if target == TARGET_OUTCOME and rv_table is None:
        raise ValueError(
            f"{directory.name}: the outcome model needs a run-value table to "
            "turn probabilities into runs"
        )

    return LoadedModel(
        version=card["version"],
        variant=card["variant"],
        target=target,
        algorithm=card.get("algorithm", "lightgbm"),
        booster=booster,
        feature_columns=feature_columns,
        categorical_columns=frozenset(card.get("categorical_columns", [])),
        scaler=runvalue.ScoreScaler.from_dict(card["score_scaler"]),
        rv_table=rv_table,
        best_iteration=int(card.get("best_iteration") or booster.best_iteration or 0),
        metrics=card.get("metrics", {}),
        git_sha=card.get("git_sha"),
        created_at=card.get("created_at", ""),
    )


def load_all(models_dir: Path) -> dict[str, LoadedModel]:
    """Load every model artifact directory, keyed by variant."""
    if not models_dir.exists():
        raise FileNotFoundError(f"models directory not found: {models_dir}")

    loaded: dict[str, LoadedModel] = {}
    for entry in sorted(models_dir.iterdir()):
        if not entry.is_dir() or not (entry / "model_card.json").exists():
            continue
        model = load_model(entry)
        if model.variant in loaded:
            raise ValueError(
                f"two artifacts claim variant {model.variant!r}: "
                f"{loaded[model.variant].version} and {model.version}"
            )
        loaded[model.variant] = model
        log.info("loaded %s/%s (%d features, target %s)",
                 model.version, model.variant, len(model.feature_columns),
                 model.target)

    if not loaded:
        raise FileNotFoundError(f"no model artifacts under {models_dir}")
    return loaded


def default_models_dir() -> Path:
    return config.MODELS_DIR


def known_feature_columns() -> frozenset[str]:
    """Every column any variant may legitimately ask for."""
    cols: set[str] = set()
    for variant_cols in features.VARIANTS.values():
        cols.update(variant_cols)
    return frozenset(cols)
