"""Contract tests for the data pipeline.

These cover the invariants the project's validity rests on: the label mapping
is total, the feature whitelist never touches the leakage blacklist, the
transformation rules do what they claim, and the splits are strictly temporal.
"""

from __future__ import annotations

import numpy as np
import pandas as pd
import pytest

from pitchlab_ml import config, features, labels, split


# --------------------------------------------------------------------------
# Labels
# --------------------------------------------------------------------------

def test_label_mapping_covers_every_class():
    assert set(config.DESCRIPTION_TO_CLASS.values()) == set(config.CLASS_LABELS)


def test_mapped_and_excluded_are_disjoint():
    assert not (set(config.DESCRIPTION_TO_CLASS) & config.EXCLUDED_DESCRIPTIONS)


@pytest.mark.parametrize(
    "description,expected",
    [
        ("ball", config.CLASS_BALL),
        ("blocked_ball", config.CLASS_BALL),
        ("called_strike", config.CLASS_CALLED_STRIKE),
        ("swinging_strike", config.CLASS_SWINGING_STRIKE),
        ("swinging_strike_blocked", config.CLASS_SWINGING_STRIKE),
        ("foul", config.CLASS_FOUL),
        ("foul_tip", config.CLASS_FOUL),
        ("hit_into_play", config.CLASS_IN_PLAY),
    ],
)
def test_description_maps_to_expected_class(description, expected):
    df = pd.DataFrame({"description": [description]})
    assert labels.attach_labels(df)[labels.TARGET].iloc[0] == expected


def test_excluded_descriptions_are_dropped():
    df = pd.DataFrame({"description": ["ball", "hit_by_pitch", "automatic_ball"]})
    out = labels.attach_labels(df)
    assert len(out) == 1
    assert out[labels.TARGET].iloc[0] == config.CLASS_BALL


def test_unknown_description_raises():
    """A new Statcast value must fail loudly, not be silently discarded."""
    df = pd.DataFrame({"description": ["ball", "some_new_2027_value"]})
    with pytest.raises(ValueError, match="unmapped"):
        labels.attach_labels(df)


# --------------------------------------------------------------------------
# Leakage
# --------------------------------------------------------------------------

def test_no_variant_contains_a_leaky_column():
    for variant, cols in features.VARIANTS.items():
        overlap = set(cols) & config.LEAKY_COLUMNS
        assert not overlap, f"{variant} leaks: {sorted(overlap)}"


def test_known_leaky_fields_are_blacklisted():
    for col in ("launch_speed", "launch_angle", "delta_run_exp",
                "estimated_woba_using_speedangle", "bat_speed",
                "pitcher_days_until_next_game", "description", "type", "events"):
        assert col in config.LEAKY_COLUMNS, f"{col} must be blacklisted"


def test_past_and_future_game_gap_fields_are_treated_differently():
    """One word apart; one is history, the other cannot exist at pitch time."""
    assert "pitcher_days_since_prev_game" in features.CONTEXT_FEATURES
    assert "pitcher_days_until_next_game" in config.LEAKY_COLUMNS


def test_select_matrix_rejects_leaky_columns(monkeypatch):
    monkeypatch.setitem(features.VARIANTS, "bad", ("release_speed", "launch_speed"))
    df = pd.DataFrame({"release_speed": [95.0], "launch_speed": [101.2]})
    with pytest.raises(AssertionError, match="leaky"):
        features.select_matrix(df, "bad")


def test_stuff_variant_is_physical_only():
    """StuffScore must describe the pitch, not where it went or the situation."""
    stuff = set(features.VARIANTS["stuff"])
    assert stuff == set(features.PHYSICAL_FEATURES)
    assert not stuff & set(features.LOCATION_FEATURES)
    assert not stuff & set(features.CONTEXT_FEATURES)


def test_pitching_variant_is_the_union():
    assert set(features.VARIANTS["pitching"]) == (
        set(features.PHYSICAL_FEATURES)
        | set(features.CONTEXT_FEATURES)
        | set(features.LOCATION_FEATURES)
    )


def test_feature_groups_are_disjoint():
    groups = (features.PHYSICAL_FEATURES, features.CONTEXT_FEATURES,
              features.LOCATION_FEATURES)
    for i, a in enumerate(groups):
        for b in groups[i + 1:]:
            assert not set(a) & set(b)


def test_swing_classes_are_exactly_the_contact_and_miss_outcomes():
    assert set(features.SWING_CLASSES) == {
        config.CLASS_SWINGING_STRIKE, config.CLASS_FOUL, config.CLASS_IN_PLAY
    }
    assert config.CLASS_BALL not in features.SWING_CLASSES
    assert config.CLASS_CALLED_STRIKE not in features.SWING_CLASSES


# --------------------------------------------------------------------------
# Transformations
# --------------------------------------------------------------------------

def _frame(**overrides) -> pd.DataFrame:
    base = {
        "p_throws": "R", "stand": "R", "pitch_type": "FF",
        "release_speed": 95.0, "release_spin_rate": 2300.0, "spin_axis": 200.0,
        "pfx_x": 0.8, "pfx_z": 1.4, "plate_x": 0.3, "plate_z": 2.5,
        "sz_top": 3.4, "sz_bot": 1.6, "release_pos_x": -1.9,
        "release_pos_z": 5.8, "release_extension": 6.4, "arm_angle": 42.0,
        "balls": 1, "strikes": 2, "outs_when_up": 1, "inning": 5,
        "on_1b": np.nan, "on_2b": np.nan, "on_3b": np.nan,
        "bat_score": 2, "fld_score": 3, "n_thruorder_pitcher": 2,
        "pitch_number": 3, "pitcher_days_since_prev_game": 5.0,
    }
    base.update(overrides)
    return pd.DataFrame([base])


def test_d2_horizontal_values_are_consistent_across_handedness():
    """Mirror-image pitches must produce identical arm-frame values."""
    rhp = features.build_features(_frame(p_throws="R", pfx_x=-0.63, release_pos_x=-1.83))
    lhp = features.build_features(_frame(p_throws="L", pfx_x=0.63, release_pos_x=1.83))

    assert rhp["pfx_x_arm"].iloc[0] == pytest.approx(lhp["pfx_x_arm"].iloc[0])
    assert rhp["release_pos_x_arm"].iloc[0] == pytest.approx(
        lhp["release_pos_x_arm"].iloc[0]
    )


def test_d2_arm_side_is_positive():
    """Pin the direction, not just the consistency.

    Consistency alone passes with the sign inverted, which is how the original
    implementation shipped backwards. These values are league means for
    four-seam fastballs over the 2023-2025 snapshot, where the run is by
    definition to the pitcher's arm side.
    """
    rhp = features.build_features(_frame(p_throws="R", pfx_x=-0.63, release_pos_x=-1.83))
    lhp = features.build_features(_frame(p_throws="L", pfx_x=0.66, release_pos_x=1.94))

    assert rhp["pfx_x_arm"].iloc[0] > 0, "RHP arm-side run must be positive"
    assert lhp["pfx_x_arm"].iloc[0] > 0, "LHP arm-side run must be positive"
    assert rhp["release_pos_x_arm"].iloc[0] > 0
    assert lhp["release_pos_x_arm"].iloc[0] > 0


def test_plate_x_bat_inside_is_positive():
    """Verified from hit-by-pitch locations: RHB side is negative plate_x."""
    rhb_inside = features.build_features(_frame(stand="R", plate_x=-0.6))
    lhb_inside = features.build_features(_frame(stand="L", plate_x=0.6))
    rhb_outside = features.build_features(_frame(stand="R", plate_x=0.6))

    assert rhb_inside["plate_x_bat"].iloc[0] > 0
    assert lhb_inside["plate_x_bat"].iloc[0] > 0
    assert rhb_outside["plate_x_bat"].iloc[0] < 0


def test_d1_spin_axis_is_encoded_circularly():
    """1 degree and 359 degrees are neighbours, not 358 units apart."""
    a = features.build_features(_frame(spin_axis=1.0))
    b = features.build_features(_frame(spin_axis=359.0))

    def vec(df):
        return np.array([df["spin_axis_sin"].iloc[0], df["spin_axis_cos"].iloc[0]])

    assert np.linalg.norm(vec(a) - vec(b)) < 0.05

    # Encoding is on the unit circle.
    assert np.hypot(*vec(a)) == pytest.approx(1.0)

    # Pure backspin (180) and pure topspin (0) must be maximally far apart.
    back = vec(features.build_features(_frame(spin_axis=180.0)))
    top = vec(features.build_features(_frame(spin_axis=0.0)))
    assert np.linalg.norm(back - top) == pytest.approx(2.0, abs=1e-6)


def test_d1_spin_axis_mirrors_for_left_handers():
    rhp = features.build_features(_frame(p_throws="R", spin_axis=120.0))
    lhp = features.build_features(_frame(p_throws="L", spin_axis=240.0))
    assert rhp["spin_axis_sin"].iloc[0] == pytest.approx(lhp["spin_axis_sin"].iloc[0])
    assert rhp["spin_axis_cos"].iloc[0] == pytest.approx(lhp["spin_axis_cos"].iloc[0])


def test_d4_zone_height_is_normalized():
    bot = features.build_features(_frame(plate_z=1.6, sz_bot=1.6, sz_top=3.4))
    top = features.build_features(_frame(plate_z=3.4, sz_bot=1.6, sz_top=3.4))
    mid = features.build_features(_frame(plate_z=2.5, sz_bot=1.6, sz_top=3.4))

    assert bot["zone_height_norm"].iloc[0] == pytest.approx(0.0)
    assert top["zone_height_norm"].iloc[0] == pytest.approx(1.0)
    assert mid["zone_height_norm"].iloc[0] == pytest.approx(0.5)

    # A short and a tall batter with the same relative height agree.
    short = features.build_features(_frame(plate_z=2.2, sz_bot=1.4, sz_top=3.0))
    tall = features.build_features(_frame(plate_z=2.8, sz_bot=1.8, sz_top=3.8))
    assert short["zone_height_norm"].iloc[0] == pytest.approx(
        tall["zone_height_norm"].iloc[0], abs=1e-6
    )


def test_plate_x_bat_mirrors_for_left_handed_batters():
    rhb = features.build_features(_frame(stand="R", plate_x=0.5))
    lhb = features.build_features(_frame(stand="L", plate_x=-0.5))
    assert rhb["plate_x_bat"].iloc[0] == pytest.approx(lhb["plate_x_bat"].iloc[0])


def test_in_zone_respects_plate_width_and_batter_zone():
    inside = features.build_features(_frame(plate_x=0.0, plate_z=2.5))
    wide = features.build_features(_frame(plate_x=1.2, plate_z=2.5))
    high = features.build_features(_frame(plate_x=0.0, plate_z=4.0))
    assert inside["in_zone"].iloc[0] == 1
    assert wide["in_zone"].iloc[0] == 0
    assert high["in_zone"].iloc[0] == 0


def test_measurement_bounds_null_impossible_values():
    out = features.build_features(_frame(release_speed=250.0))
    assert pd.isna(out["release_speed"].iloc[0])
    # the rest of the row survives
    assert out["release_spin_rate"].iloc[0] == pytest.approx(2300.0)


def test_unknown_pitch_type_becomes_a_category_not_a_dropped_row():
    out = features.build_features(_frame(pitch_type=None))
    assert len(out) == 1
    assert out["pitch_type"].iloc[0] == "UNKNOWN"
    assert out["pitch_group"].iloc[0] == "OTHER"


def test_context_encodings():
    out = features.build_features(
        _frame(balls=3, strikes=1, on_1b=12345, on_3b=67890,
               stand="L", p_throws="L", bat_score=5, fld_score=2)
    )
    assert out["count_state"].iloc[0] == "3-1"
    assert out["runners_state"].iloc[0] == "1_3"
    assert out["same_handed"].iloc[0] == 1
    assert out["score_diff"].iloc[0] == 3


# --------------------------------------------------------------------------
# Arsenal features
# --------------------------------------------------------------------------

def test_arsenal_reference_uses_only_the_previous_season():
    df = features.build_features(
        pd.concat([
            _frame(release_speed=95.0).assign(pitcher=1, game_year=2023),
            _frame(release_speed=90.0).assign(pitcher=1, game_year=2024),
        ], ignore_index=True)
    )
    ref = pd.DataFrame([
        {"pitcher": 1, "game_year": 2023, "fb_speed": 95.0, "fb_pfx_x_arm": 0.8,
         "fb_pfx_z": 1.4, "fb_rel_x_arm": -1.9, "fb_rel_z": 5.8, "fb_n": 500},
    ])
    out = features.attach_arsenal_features(df, ref)

    # 2023 pitch has no prior season -> NaN, never its own aggregate.
    assert pd.isna(out.loc[out["game_year"] == 2023, "velo_diff_vs_fb"].iloc[0])
    # 2024 pitch is compared against the 2023 baseline.
    assert out.loc[out["game_year"] == 2024, "velo_diff_vs_fb"].iloc[0] == pytest.approx(-5.0)


def test_arsenal_reference_ignores_thin_samples():
    df = pd.DataFrame([
        {"pitcher": 1, "game_year": 2023, "pitch_type": "FF",
         "release_speed": 95.0, "pfx_x_arm": 0.8, "pfx_z": 1.4,
         "release_pos_x_arm": -1.9, "release_pos_z": 5.8},
    ])
    ref = features.build_arsenal_reference(df)
    assert pd.isna(ref["fb_speed"].iloc[0]), "1 fastball is not a baseline"


# --------------------------------------------------------------------------
# Splits
# --------------------------------------------------------------------------

def _split_frame() -> pd.DataFrame:
    dates = (
        ["2023-05-01"] * 5 + ["2024-05-01"] * 5
        + ["2025-04-01"] * 5 + ["2025-08-01"] * 5
    )
    return pd.DataFrame({
        "game_date": pd.to_datetime(dates),
        "game_pk": range(len(dates)),
        "pitcher": [1] * len(dates),
    })


def test_split_is_strictly_temporal():
    s = split.make_split(_split_frame())
    assert len(s.train) == 10 and len(s.val) == 5 and len(s.test) == 5
    assert s.train["game_date"].max() < s.val["game_date"].min()
    assert s.val["game_date"].max() < s.test["game_date"].min()


def test_split_excludes_out_of_regime_seasons():
    df = pd.concat([
        _split_frame(),
        pd.DataFrame({"game_date": pd.to_datetime(["2026-05-01"] * 5),
                      "game_pk": range(100, 105), "pitcher": [1] * 5}),
    ], ignore_index=True)
    s = split.make_split(df)
    for part in (s.train, s.val, s.test):
        assert not (pd.to_datetime(part["game_date"]).dt.year == 2026).any()


def test_no_game_straddles_two_splits():
    s = split.make_split(_split_frame())
    assert not set(s.train["game_pk"]) & set(s.val["game_pk"])
    assert not set(s.val["game_pk"]) & set(s.test["game_pk"])


def test_test_set_requires_explicit_opt_in(monkeypatch):
    s = split.make_split(_split_frame())
    monkeypatch.delenv(split.TEST_ACCESS_ENV, raising=False)
    with pytest.raises(PermissionError):
        split.load_test_set(s)

    monkeypatch.setenv(split.TEST_ACCESS_ENV, "1")
    assert len(split.load_test_set(s)) == 5


# --------------------------------------------------------------------------
# Metrics
# --------------------------------------------------------------------------

def test_log_loss_respects_our_class_column_order():
    """Regression: sklearn's log_loss re-sorts labels and mispairs columns.

    CLASS_LABELS is not alphabetical. A metric that assumes alphabetical order
    scores a confident correct prediction as a confident wrong one.
    """
    from pitchlab_ml import evaluate

    y = pd.Series([config.CLASS_SWINGING_STRIKE])
    idx = list(config.CLASS_LABELS).index(config.CLASS_SWINGING_STRIKE)
    proba = np.full((1, 5), 0.01)
    proba[0, idx] = 0.96

    assert evaluate.multiclass_log_loss(y, proba) == pytest.approx(
        -np.log(0.96), abs=1e-6
    )


def test_log_loss_matches_hand_computation_on_several_rows():
    from pitchlab_ml import evaluate

    labels_list = list(config.CLASS_LABELS)
    y = pd.Series([labels_list[0], labels_list[2], labels_list[4]])
    proba = np.array([
        [0.60, 0.10, 0.10, 0.10, 0.10],
        [0.10, 0.10, 0.50, 0.20, 0.10],
        [0.05, 0.05, 0.05, 0.05, 0.80],
    ])
    expected = -np.mean(np.log([0.60, 0.50, 0.80]))
    assert evaluate.multiclass_log_loss(y, proba) == pytest.approx(expected)


def test_log_loss_is_minimized_by_the_truth():
    from pitchlab_ml import evaluate

    y = pd.Series(list(config.CLASS_LABELS))
    perfect = np.eye(5) * 0.99 + 0.0025
    uniform = np.full((5, 5), 0.2)
    assert evaluate.multiclass_log_loss(y, perfect) < evaluate.multiclass_log_loss(
        y, uniform
    )


def test_suspicious_auc_is_flagged():
    from pitchlab_ml import evaluate

    m = evaluate.Metrics(
        log_loss=0.1, brier_per_class={}, ece=0.0, accuracy=1.0, n_rows=1,
        auc_ovr={"BALL": 0.99, "FOUL": 0.70},
    )
    assert m.suspicious_classes == ["BALL"]
