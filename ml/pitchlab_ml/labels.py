"""Label construction: Statcast `description` -> 5 outcome classes.

Kept separate from features.py so the label path and the feature path never
share code. Nothing in this module may be imported by feature construction.
"""

from __future__ import annotations

import logging

import pandas as pd

from . import config

log = logging.getLogger(__name__)

TARGET = "outcome_class"


def attach_labels(df: pd.DataFrame, *, strict: bool = True) -> pd.DataFrame:
    """Map `description` to the 5-class target and drop non-competitive rows.

    Rows whose description is in EXCLUDED_DESCRIPTIONS are removed. If any
    description is neither mapped nor explicitly excluded, that is a contract
    violation: Statcast has introduced a value we have not classified, and
    silently dropping it would quietly bias the training set.
    """
    if "description" not in df.columns:
        raise KeyError("`description` column is required to build labels")

    mapped = df["description"].map(config.DESCRIPTION_TO_CLASS)
    excluded = df["description"].isin(config.EXCLUDED_DESCRIPTIONS)
    unknown = mapped.isna() & ~excluded

    if unknown.any():
        counts = df.loc[unknown, "description"].value_counts().to_dict()
        msg = f"unmapped `description` values: {counts}"
        if strict:
            raise ValueError(
                msg + " -- add them to DESCRIPTION_TO_CLASS or "
                "EXCLUDED_DESCRIPTIONS in config.py"
            )
        log.warning("%s -- dropping", msg)

    out = df.loc[mapped.notna()].copy()
    out[TARGET] = pd.Categorical(
        mapped.loc[mapped.notna()], categories=list(config.CLASS_LABELS)
    )

    log.info(
        "labelled %s rows (excluded %s, unknown %s)",
        f"{len(out):,}", f"{int(excluded.sum()):,}", f"{int(unknown.sum()):,}",
    )
    return out.reset_index(drop=True)


def class_distribution(df: pd.DataFrame) -> pd.DataFrame:
    counts = df[TARGET].value_counts().reindex(config.CLASS_LABELS)
    return pd.DataFrame({"count": counts, "share": counts / counts.sum()})
