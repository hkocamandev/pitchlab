"""Check whether the physical ('stuff') features carry signal on the
sub-problem where they should: whiff given that the batter swung.

The 5-class outcome is dominated by location, because whether a batter swings
at all is mostly a question of where the pitch is. Conditioning on a swing
removes that, leaving the question public Stuff+ models actually ask.

Usage:
    .venv/bin/python ml/scripts/probe_stuff_subproblem.py
"""

from __future__ import annotations

import logging
import sys
from pathlib import Path

import lightgbm as lgb
import pandas as pd
from sklearn.metrics import roc_auc_score

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, features, labels, split, train  # noqa: E402

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)-5s %(message)s")
log = logging.getLogger("probe")

SWING_CLASSES = (
    config.CLASS_SWINGING_STRIKE,
    config.CLASS_FOUL,
    config.CLASS_IN_PLAY,
)

# Context features are shared by every variant; excluding them isolates the
# purely physical signal.
CONTEXT_FEATURES = frozenset({
    "balls", "strikes", "count_state", "outs_when_up", "runners_state",
    "inning", "score_diff", "n_thruorder_pitcher", "pitch_number",
    "pitcher_days_since_prev_game",
})


def auc_for(tr: pd.DataFrame, va: pd.DataFrame, cols: tuple[str, ...],
            label: str) -> float:
    X_tr = features.select_matrix(tr, "pitching").loc[:, list(cols)]
    X_va = features.select_matrix(va, "pitching").loc[:, list(cols)]
    cat = [c for c in cols if c in features.CATEGORICAL_FEATURES]

    params = {k: v for k, v in train.LGB_PARAMS.items() if k != "num_class"}
    params.update(objective="binary", metric="binary_logloss")

    booster = lgb.train(
        params,
        lgb.Dataset(X_tr, label=tr[label].to_numpy(), categorical_feature=cat),
        num_boost_round=2000,
        valid_sets=[lgb.Dataset(X_va, label=va[label].to_numpy(),
                                categorical_feature=cat)],
        callbacks=[lgb.early_stopping(100, verbose=False)],
    )
    return float(roc_auc_score(
        va[label], booster.predict(X_va, num_iteration=booster.best_iteration)
    ))


def main() -> int:
    snapshot = sorted(config.RAW_DIR.glob("statcast_*.parquet"))[-1]
    parts = split.make_split(train.prepare(snapshot))
    tr, va = parts.train.copy(), parts.val.copy()

    for part in (tr, va):
        part["is_swing"] = (
            part[labels.TARGET].astype(str).isin(SWING_CLASSES).astype(int)
        )

    swing_tr = tr[tr["is_swing"] == 1].copy()
    swing_va = va[va["is_swing"] == 1].copy()
    for part in (swing_tr, swing_va):
        part["is_whiff"] = (
            part[labels.TARGET].astype(str) == config.CLASS_SWINGING_STRIKE
        ).astype(int)

    log.info("swings: train %s, val %s (%.1f%% of pitches)",
             f"{len(swing_tr):,}", f"{len(swing_va):,}",
             100 * len(swing_tr) / len(tr))
    log.info("whiff rate among swings: train %.3f, val %.3f",
             swing_tr["is_whiff"].mean(), swing_va["is_whiff"].mean())

    stuff_only = tuple(
        c for c in features.STUFF_FEATURES if c not in CONTEXT_FEATURES
    )
    both = stuff_only + tuple(features.LOCATION_FEATURES)

    print()
    print("=" * 72)
    print("A. SWING DECISION   P(swing | pitch)")
    print("=" * 72)
    a_loc = auc_for(tr, va, features.LOCATION_FEATURES, "is_swing")
    a_stuff = auc_for(tr, va, stuff_only, "is_swing")
    print(f"  location only : AUC {a_loc:.4f}")
    print(f"  stuff only    : AUC {a_stuff:.4f}")

    print()
    print("=" * 72)
    print("B. WHIFF GIVEN SWING   P(whiff | swing)   -- the Stuff+ question")
    print("=" * 72)
    b_loc = auc_for(swing_tr, swing_va, features.LOCATION_FEATURES, "is_whiff")
    b_stuff = auc_for(swing_tr, swing_va, stuff_only, "is_whiff")
    b_both = auc_for(swing_tr, swing_va, both, "is_whiff")
    print(f"  location only : AUC {b_loc:.4f}")
    print(f"  stuff only    : AUC {b_stuff:.4f}")
    print(f"  both          : AUC {b_both:.4f}")

    print()
    print("=" * 72)
    print("VERDICT")
    print("=" * 72)
    print(f"  stuff on swing decision    : {a_stuff:.4f}")
    print(f"  stuff on whiff-given-swing : {b_stuff:.4f}")
    if b_stuff > a_stuff + 0.05:
        print("  => Stuff is informative, but only once the swing decision is")
        print("     conditioned away. Scoring stuff on the full 5-class outcome")
        print("     buries it under geometry.")
    else:
        print("  => Stuff adds little even on the conditioned sub-problem.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
