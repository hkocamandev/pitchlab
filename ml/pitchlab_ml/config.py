"""Central configuration: paths, date windows, and data contracts.

Everything that other modules must agree on lives here. In particular the
label mapping and the feature whitelist are single-sourced so that training
and inference cannot drift apart.
"""

from __future__ import annotations

from pathlib import Path

# --------------------------------------------------------------------------
# Paths
# --------------------------------------------------------------------------

REPO_ROOT = Path(__file__).resolve().parents[2]

DATA_DIR = REPO_ROOT / "data"
RAW_DIR = DATA_DIR / "raw"
SAMPLE_DIR = RAW_DIR / "samples"
PROCESSED_DIR = DATA_DIR / "processed"
CACHE_DIR = DATA_DIR / "cache"

MODELS_DIR = REPO_ROOT / "models"

# Generated analysis reports are project documentation, so they live under
# docs/ (Turkish, local-only) rather than in the committed source tree.
REPORTS_DIR = REPO_ROOT / "docs" / "reports"

for _d in (RAW_DIR, SAMPLE_DIR, PROCESSED_DIR, CACHE_DIR, MODELS_DIR, REPORTS_DIR):
    _d.mkdir(parents=True, exist_ok=True)


# --------------------------------------------------------------------------
# Temporal windows
#
# Rationale lives in docs/dataset-analysis.md. Summary of the regime breaks
# that bound this window:
#   2020        Hawk-Eye replaces TrackMan; spin axis becomes directly measured
#   2021-06-21  foreign-substance enforcement; league spin drops ~80 RPM
#   2023        pitch clock, shift ban, larger bases
#   2026        ABS: plate_x/plate_z move front-of-plate -> middle-of-plate,
#               sz_top/sz_bot become ABS-defined rather than operator-marked
# --------------------------------------------------------------------------

TRAIN_SEASONS = (2023, 2024)
VAL_SEASON = 2025
TEST_SEASON = 2025

# 2025 is split in half: first half validates, second half is the held-out test.
VAL_END = "2025-06-30"

# Excluded from training and evaluation; used only for drift demonstration.
OUT_OF_REGIME_SEASONS = (2026,)

REGULAR_SEASON_ONLY = True  # game_type == 'R'


# --------------------------------------------------------------------------
# Label contract: Statcast `description` -> 5 outcome classes
#
# Notes:
#   foul_tip    -> FOUL      the rulebook scores it a strike, but physically
#                            the bat touched the ball; the model should learn
#                            the physical event. The rule effect is captured
#                            in the run-value table instead.
#   blocked_ball-> BALL      blocking reflects location, not pitch quality.
# --------------------------------------------------------------------------

CLASS_BALL = "BALL"
CLASS_CALLED_STRIKE = "CALLED_STRIKE"
CLASS_SWINGING_STRIKE = "SWINGING_STRIKE"
CLASS_FOUL = "FOUL"
CLASS_IN_PLAY = "IN_PLAY"

CLASS_LABELS = (
    CLASS_BALL,
    CLASS_CALLED_STRIKE,
    CLASS_SWINGING_STRIKE,
    CLASS_FOUL,
    CLASS_IN_PLAY,
)

DESCRIPTION_TO_CLASS: dict[str, str] = {
    "ball": CLASS_BALL,
    "blocked_ball": CLASS_BALL,
    "called_strike": CLASS_CALLED_STRIKE,
    "swinging_strike": CLASS_SWINGING_STRIKE,
    "swinging_strike_blocked": CLASS_SWINGING_STRIKE,
    "foul": CLASS_FOUL,
    "foul_tip": CLASS_FOUL,
    "hit_into_play": CLASS_IN_PLAY,
}

# Non-competitive pitches and bunt attempts are dropped rather than mapped.
# Together these are roughly 1% of pitches.
#
# automatic_ball / automatic_strike were added after empirical inspection in
# phase 1: these rows are pitch-timer violations and intentional walks where
# *no pitch was thrown at all*. Verified on a 53k-pitch sample -- plate_x,
# plate_z and pitch_type are null on 100% of them. They carry an outcome but
# no pitch to attribute it to, so they cannot be training examples.
EXCLUDED_DESCRIPTIONS: frozenset[str] = frozenset(
    {
        "hit_by_pitch",
        "pitchout",
        "foul_bunt",
        "missed_bunt",
        "bunt_foul_tip",
        "foul_pitchout",
        "swinging_pitchout",
        "automatic_ball",
        "automatic_strike",
        "automatic_ball_pitcher_pitch_timer",
        "automatic_strike_batter_pitch_timer",
    }
)


# --------------------------------------------------------------------------
# Leakage blacklist
#
# This is NOT the primary defense -- the feature whitelist in features.py is.
# This list exists so that a test can assert the whitelist never intersects it,
# and so the reasoning is discoverable from the code.
# --------------------------------------------------------------------------

LEAKY_COLUMNS: frozenset[str] = frozenset(
    {
        # the label itself and its coarsenings
        "description", "type", "events", "des",
        # post-contact measurements
        "launch_speed", "launch_angle", "hit_distance", "hit_distance_sc",
        "bb_type", "hc_x", "hc_y", "launch_speed_angle", "hyper_speed",
        "hit_location",
        # outcome-derived metrics
        "estimated_ba_using_speedangle", "estimated_woba_using_speedangle",
        "estimated_slg_using_speedangle", "woba_value", "woba_denom",
        "babip_value", "iso_value", "delta_run_exp", "delta_pitcher_run_exp",
        "delta_home_win_exp", "post_home_score", "post_away_score",
        "post_bat_score", "post_fld_score", "home_win_exp", "bat_win_exp",
        # bat tracking: only populated when a swing occurred
        "bat_speed", "swing_length", "attack_angle", "attack_direction",
        "swing_path_tilt", "intercept_ball_minus_batter_pos_x_inches",
        "intercept_ball_minus_batter_pos_y_inches",
        # future information -- cannot exist at pitch time
        "pitcher_days_until_next_game", "batter_days_until_next_game",
        # identity: memorizing the pitcher is not modeling the pitch
        "pitcher", "batter", "player_name", "pitcher_1", "fielder_2",
        "game_pk", "game_date", "game_year", "at_bat_number", "pitch_number",
        "sv_id", "home_team", "away_team",
    }
)


# --------------------------------------------------------------------------
# Physical plausibility bounds. Mirrors the CHECK constraints planned for
# pitch_measurements so that bad device readings are caught identically in
# both the offline and online paths.
# --------------------------------------------------------------------------

MEASUREMENT_BOUNDS: dict[str, tuple[float, float]] = {
    "release_speed": (40.0, 110.0),
    "release_spin_rate": (0.0, 4000.0),
    "spin_axis": (0.0, 360.0),
    "release_extension": (3.0, 9.0),
    "release_pos_z": (0.0, 8.0),
    "release_pos_x": (-6.0, 6.0),
    "pfx_x": (-3.0, 3.0),
    "pfx_z": (-3.0, 3.0),
    "plate_x": (-4.0, 4.0),
    "plate_z": (-3.0, 8.0),
    "sz_top": (2.0, 5.0),
    "sz_bot": (0.5, 3.0),
}

PITCH_GROUPS: dict[str, str] = {
    "FF": "FASTBALL", "SI": "FASTBALL", "FC": "FASTBALL", "FA": "FASTBALL",
    "SL": "BREAKING", "ST": "BREAKING", "SV": "BREAKING",
    "CU": "BREAKING", "KC": "BREAKING", "CS": "BREAKING", "SC": "BREAKING",
    "CH": "OFFSPEED", "FS": "OFFSPEED", "FO": "OFFSPEED",
    "KN": "OTHER", "EP": "OTHER", "PO": "OTHER", "IN": "OTHER",
}
FASTBALL_TYPES = frozenset({"FF", "SI", "FC", "FA"})

RANDOM_SEED = 42
