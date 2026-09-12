"""Feature construction and the feature whitelist.

The whitelist is the primary leakage defense. A column reaches the model only
by appearing in STUFF_FEATURES or LOCATION_FEATURES below; there is no code
path that passes a raw dataframe to the estimator. `assert_no_leakage` makes
that contract testable.

Transformation rules referenced as D1..D12 are documented in the domain
research; the short version is in the docstrings here.
"""

from __future__ import annotations

import logging

import numpy as np
import pandas as pd

from . import config

log = logging.getLogger(__name__)


# --------------------------------------------------------------------------
# Feature whitelist
# --------------------------------------------------------------------------

# Shape of the pitch itself: what it looks like coming out of the hand,
# independent of where it ends up or of the game situation. This is the
# feature set public Stuff+ models use.
PHYSICAL_FEATURES: tuple[str, ...] = (
    "release_speed",
    "release_spin_rate",
    "spin_axis_sin",
    "spin_axis_cos",
    "pfx_x_arm",
    "pfx_z",
    "release_extension",
    "release_pos_z",
    "release_pos_x_arm",
    "arm_angle",
    # arsenal-relative (computed from the pitcher's PREVIOUS season only)
    "velo_diff_vs_fb",
    "pfx_x_diff_vs_fb",
    "pfx_z_diff_vs_fb",
    "release_dist_from_fb",
    # the pitcher's intent, and the platoon matchup it is thrown into
    "pitch_type",
    "pitch_group",
    "stand",
    "p_throws",
    "same_handed",
)

# Game situation. Known before the pitch, but not a property of the pitch.
CONTEXT_FEATURES: tuple[str, ...] = (
    "balls",
    "strikes",
    "count_state",
    "outs_when_up",
    "runners_state",
    "inning",
    "n_thruorder_pitcher",
    "pitch_number",
    "score_diff",
    "pitcher_days_since_prev_game",
)

# Where the pitch crossed the plate.
LOCATION_FEATURES: tuple[str, ...] = (
    "plate_x_bat",
    "zone_height_norm",
    "dist_from_zone_center",
    "in_zone",
)

CATEGORICAL_FEATURES: frozenset[str] = frozenset(
    {"count_state", "runners_state", "stand", "p_throws", "pitch_type", "pitch_group"}
)

# Two models with two different targets, because phase 1 measurement showed a
# single target does not serve both questions:
#
#   pitching  5-class pitch outcome over every pitch. Feeds xRV and PitchScore.
#   stuff     P(whiff | swing) on physical features alone. Feeds StuffScore.
#
# Scoring "stuff" on the 5-class outcome was the original design and it does
# not work: physical features reach only 0.554 AUC on the swing decision, which
# dominates that target, versus 0.652 once a swing is conditioned on. A pitch's
# shape barely predicts whether a batter offers at it -- that is a question
# about location -- but it does predict whether he misses.
VARIANTS: dict[str, tuple[str, ...]] = {
    "stuff": PHYSICAL_FEATURES,
    "pitching": PHYSICAL_FEATURES + CONTEXT_FEATURES + LOCATION_FEATURES,
}

# Classes that imply the batter offered at the pitch.
SWING_CLASSES: tuple[str, ...] = (
    config.CLASS_SWINGING_STRIKE,
    config.CLASS_FOUL,
    config.CLASS_IN_PLAY,
)

# Columns needed to build features or to join/split, but never fed to a model.
SUPPORT_COLUMNS: tuple[str, ...] = (
    "pitcher", "batter", "game_pk", "game_date", "game_year",
    "at_bat_number", "description",
    # Used only to build the run-value lookup table, never as a feature.
    "delta_run_exp",
)

# Raw Statcast columns that build_features reads. Loading only these keeps a
# 2.9M-row snapshot manageable in memory; the full frame is ~119 columns wide.
RAW_INPUT_COLUMNS: tuple[str, ...] = (
    "release_speed", "release_spin_rate", "spin_axis",
    "release_pos_x", "release_pos_z", "release_extension",
    "pfx_x", "pfx_z", "plate_x", "plate_z", "sz_top", "sz_bot",
    "arm_angle", "pitch_type", "p_throws", "stand",
    "balls", "strikes", "outs_when_up", "inning",
    "on_1b", "on_2b", "on_3b", "bat_score", "fld_score",
    "n_thruorder_pitcher", "pitch_number", "pitcher_days_since_prev_game",
)


def required_raw_columns() -> list[str]:
    """Every raw column the pipeline reads, deduplicated and ordered."""
    seen: dict[str, None] = {}
    for col in (*RAW_INPUT_COLUMNS, *SUPPORT_COLUMNS):
        seen.setdefault(col, None)
    return list(seen)

PLATE_HALF_WIDTH_FT = 17.0 / 2.0 / 12.0  # home plate is 17 inches wide


def mirror_for_right(values, is_right) -> np.ndarray:
    """Mirror a catcher-relative horizontal quantity into a handedness-neutral frame.

    Statcast's horizontal axis is catcher-relative, so the same physical action
    has opposite signs for left- and right-handers. Negating the right-handed
    rows puts both groups in one frame where positive means "arm side" for a
    pitcher and "inside" for a batter.

    The direction was established from data, not from geometry -- see
    ml/scripts/verify_plate_sign.py. Over the 2023-2025 snapshot:
      * right-handed batters are hit by pitches at mean plate_x -1.94,
        left-handed batters at +1.98, so negative plate_x is the RHB's side;
      * four-seam fastballs average pfx_x -0.63 for RHP and +0.66 for LHP,
        so negative pfx_x is the right-hander's arm side.
    """
    return np.where(is_right, -np.asarray(values, dtype="float64"),
                    np.asarray(values, dtype="float64"))


def assert_no_leakage(columns) -> None:
    """Raise if any model feature is on the leakage blacklist."""
    overlap = sorted(set(columns) & config.LEAKY_COLUMNS)
    if overlap:
        raise AssertionError(f"leaky columns present in feature set: {overlap}")


# --------------------------------------------------------------------------
# Arsenal reference
# --------------------------------------------------------------------------

def build_arsenal_reference(df: pd.DataFrame) -> pd.DataFrame:
    """Per (pitcher, season) fastball baseline: speed, movement, release point.

    Used to express a pitch relative to the pitcher's own fastball, which
    public pitch-quality models find more predictive than absolute values.
    """
    fb = df[df["pitch_type"].isin(config.FASTBALL_TYPES)]
    if fb.empty:
        return pd.DataFrame(
            columns=["pitcher", "game_year", "fb_speed", "fb_pfx_x_arm",
                     "fb_pfx_z", "fb_rel_x_arm", "fb_rel_z", "fb_n"]
        )

    ref = (
        fb.groupby(["pitcher", "game_year"])
        .agg(
            fb_speed=("release_speed", "mean"),
            fb_pfx_x_arm=("pfx_x_arm", "mean"),
            fb_pfx_z=("pfx_z", "mean"),
            fb_rel_x_arm=("release_pos_x_arm", "mean"),
            fb_rel_z=("release_pos_z", "mean"),
            fb_n=("release_speed", "size"),
        )
        .reset_index()
    )
    # Too few fastballs is a noisy baseline; better to emit NaN than a bad number.
    ref.loc[ref["fb_n"] < 50, ref.columns.difference(["pitcher", "game_year", "fb_n"])] = np.nan
    return ref


def attach_arsenal_features(df: pd.DataFrame, reference: pd.DataFrame) -> pd.DataFrame:
    """Join the pitcher's PREVIOUS-season fastball baseline onto each pitch.

    Using the current season would leak: a season aggregate computed over all
    of 2025 would be visible to the first pitch of 2025. Shifting the
    reference forward by one year keeps every feature strictly historical.
    Pitchers with no prior season get NaN, which LightGBM handles natively.
    """
    if reference.empty:
        for c in ("velo_diff_vs_fb", "pfx_x_diff_vs_fb",
                  "pfx_z_diff_vs_fb", "release_dist_from_fb"):
            df[c] = np.nan
        return df

    ref = reference.copy()
    ref["game_year"] = ref["game_year"] + 1  # applies to the FOLLOWING season

    out = df.merge(
        ref.drop(columns=["fb_n"]), on=["pitcher", "game_year"], how="left"
    )

    out["velo_diff_vs_fb"] = out["release_speed"] - out["fb_speed"]
    out["pfx_x_diff_vs_fb"] = out["pfx_x_arm"] - out["fb_pfx_x_arm"]
    out["pfx_z_diff_vs_fb"] = out["pfx_z"] - out["fb_pfx_z"]
    out["release_dist_from_fb"] = np.sqrt(
        (out["release_pos_x_arm"] - out["fb_rel_x_arm"]) ** 2
        + (out["release_pos_z"] - out["fb_rel_z"]) ** 2
    )

    return out.drop(columns=["fb_speed", "fb_pfx_x_arm", "fb_pfx_z",
                             "fb_rel_x_arm", "fb_rel_z"])


# --------------------------------------------------------------------------
# Transformations
# --------------------------------------------------------------------------

def apply_measurement_bounds(df: pd.DataFrame) -> pd.DataFrame:
    """Null out physically impossible readings (D: trust no device).

    Values are set to NaN rather than dropped: one bad sensor field should not
    discard an otherwise usable pitch, and LightGBM handles missing natively.
    """
    out = df.copy()
    for col, (lo, hi) in config.MEASUREMENT_BOUNDS.items():
        if col not in out.columns:
            continue
        bad = out[col].notna() & ((out[col] < lo) | (out[col] > hi))
        n = int(bad.sum())
        if n:
            log.info("bounds: nulled %d out-of-range values in %s", n, col)
            out.loc[bad, col] = np.nan
    return out


def build_features(df: pd.DataFrame) -> pd.DataFrame:
    """Apply every transformation rule and return a frame with all features.

    D1  spin_axis is circular -> encode as sin/cos rather than raw degrees
    D2  horizontal values are catcher-relative -> mirror for LHP so that
        "arm side" means the same thing for both handedness groups
    D3  use pfx_z (induced vertical break), not gravity-inclusive break
    D4  normalize plate_z by the batter's own strike zone
    D10 keep pitch_type NULLs as an UNKNOWN category instead of dropping rows
    D11 model the platoon interaction explicitly
    """
    out = apply_measurement_bounds(df)

    is_rhp = out["p_throws"].eq("R")

    # D2: mirror horizontal quantities so that positive means arm side.
    out["pfx_x_arm"] = mirror_for_right(out["pfx_x"], is_rhp)
    out["release_pos_x_arm"] = mirror_for_right(out["release_pos_x"], is_rhp)

    # D1: circular encoding. Mirroring the axis flips sin and keeps cos, since
    #     sin(360 - t) = -sin(t) and cos(360 - t) = cos(t).
    axis_rad = np.deg2rad(out["spin_axis"].astype("float64"))
    out["spin_axis_sin"] = mirror_for_right(np.sin(axis_rad), is_rhp)
    out["spin_axis_cos"] = np.cos(axis_rad)

    # D4: express height as a fraction of this batter's zone.
    zone_height = out["sz_top"] - out["sz_bot"]
    zone_height = zone_height.where(zone_height > 0.5)  # guard bad markings
    out["zone_height_norm"] = (out["plate_z"] - out["sz_bot"]) / zone_height

    # Horizontal location relative to the batter: positive means inside.
    out["plate_x_bat"] = mirror_for_right(out["plate_x"], out["stand"].eq("R"))

    # pybaseball returns nullable extension dtypes, so comparisons propagate
    # pd.NA rather than producing False. Keep the missingness as NaN instead of
    # silently coercing it to "not in zone", which would be a quiet lie about
    # pitches the device failed to track.
    half_zone = PLATE_HALF_WIDTH_FT
    zone_known = (
        out["plate_x"].notna() & out["plate_z"].notna()
        & out["sz_top"].notna() & out["sz_bot"].notna()
    )
    in_zone = (
        (out["plate_x"].abs() <= half_zone)
        & (out["plate_z"] >= out["sz_bot"])
        & (out["plate_z"] <= out["sz_top"])
    )
    out["in_zone"] = np.where(
        zone_known, in_zone.fillna(False).astype("bool"), np.nan
    )

    # Distance from zone centre, with height scaled by zone size so that tall
    # and short batters are comparable.
    out["dist_from_zone_center"] = np.sqrt(
        (out["plate_x"] / half_zone) ** 2
        + ((out["zone_height_norm"] - 0.5) * 2.0) ** 2
    )

    # --- pre-pitch context -------------------------------------------------
    out["count_state"] = (
        out["balls"].astype("Int64").astype(str) + "-" +
        out["strikes"].astype("Int64").astype(str)
    )
    out["runners_state"] = (
        np.where(out["on_1b"].notna(), "1", "_")
        + np.where(out["on_2b"].notna(), "2", "_")
        + np.where(out["on_3b"].notna(), "3", "_")
    )
    hands_known = out["stand"].notna() & out["p_throws"].notna()
    out["same_handed"] = np.where(
        hands_known, out["stand"].eq(out["p_throws"]).fillna(False).astype("bool"),
        np.nan,
    )
    out["score_diff"] = out["bat_score"] - out["fld_score"]

    # D10: unknown pitch type is a category, not a reason to drop the row.
    out["pitch_type"] = out["pitch_type"].fillna("UNKNOWN")
    out["pitch_group"] = (
        out["pitch_type"].map(config.PITCH_GROUPS).fillna("OTHER")
    )

    for col in CATEGORICAL_FEATURES:
        out[col] = out[col].astype("category")

    return out


def select_matrix(df: pd.DataFrame, variant: str) -> pd.DataFrame:
    """Return exactly the feature columns for a variant, in a fixed order."""
    if variant not in VARIANTS:
        raise KeyError(f"unknown variant {variant!r}; expected {list(VARIANTS)}")
    cols = VARIANTS[variant]
    assert_no_leakage(cols)

    missing = [c for c in cols if c not in df.columns]
    if missing:
        raise KeyError(f"feature columns missing from frame: {missing}")

    out = df.loc[:, list(cols)].copy()

    # pybaseball hands back pandas nullable extension dtypes (Int64, Float64,
    # boolean). LightGBM wants plain numpy floats plus pandas categoricals, so
    # normalize here -- once, in the single place every model input flows
    # through -- rather than at each call site.
    for col in out.columns:
        if col in CATEGORICAL_FEATURES:
            if not isinstance(out[col].dtype, pd.CategoricalDtype):
                out[col] = out[col].astype("category")
        else:
            out[col] = pd.to_numeric(out[col], errors="coerce").astype("float64")

    return out
