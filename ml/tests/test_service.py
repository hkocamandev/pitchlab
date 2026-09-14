"""Contract tests for the inference service.

The single most valuable thing these check is that the service and the
offline training code compute the same number from the same input. Training
and serving diverging is the classic way a model goes quietly wrong in
production: nothing errors, the scores just stop meaning anything.
"""

from __future__ import annotations

import sys
from pathlib import Path

import numpy as np
import pandas as pd
import pytest
from fastapi.testclient import TestClient

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / "ml"))

from pitchlab_ml import config, features, labels, runvalue  # noqa: E402

from ml.service import artifacts  # noqa: E402
from ml.service.app import app  # noqa: E402

pytestmark = pytest.mark.skipif(
    not (config.MODELS_DIR / "pitch-outcome-v1.0.0-pitching").exists(),
    reason="trained model artifacts are not present; run `make ml-train`",
)


@pytest.fixture(scope="module")
def client():
    with TestClient(app) as c:
        yield c


@pytest.fixture(scope="module")
def sample_frame() -> pd.DataFrame:
    """A handful of real pitches, prepared exactly as training prepares them."""
    snapshots = sorted(config.RAW_DIR.glob("statcast_*.parquet"))
    if not snapshots:
        pytest.skip("no data snapshot available")

    raw = pd.read_parquet(snapshots[-1], columns=features.required_raw_columns())
    raw = raw[raw["game_year"] == 2025].head(5000)
    df = features.build_features(labels.attach_labels(raw))
    return features.attach_arsenal_features(df, features.build_arsenal_reference(df))


def to_payload(frame: pd.DataFrame, variant: str, n: int = 8) -> dict:
    X = features.select_matrix(frame.head(n), variant)
    pitches = []
    for i in range(len(X)):
        row = X.iloc[i]
        feats = {}
        for col in X.columns:
            value = row[col]
            if pd.isna(value):
                continue
            feats[col] = (str(value) if col in features.CATEGORICAL_FEATURES
                          else float(value))
        pitches.append({
            "pitch_id": f"p{i}",
            "count_state": str(frame["count_state"].iloc[i]),
            "features": feats,
        })
    return {"variant": variant, "pitches": pitches}


# --- health ---------------------------------------------------------------

def test_healthz_checks_nothing(client):
    assert client.get("/healthz").status_code == 200


def test_readyz_requires_loaded_models(client):
    body = client.get("/readyz").json()
    assert body["status"] == "ok"
    assert set(body["models_loaded"]) == {"pitching", "stuff"}


def test_models_endpoint_publishes_the_contract(client):
    models = {m["variant"]: m for m in client.get("/v1/models").json()["models"]}

    pitching = models["pitching"]
    assert pitching["target"] == "outcome_5class"
    assert pitching["feature_count"] == len(pitching["feature_columns"])
    assert pitching["class_labels"] == list(config.CLASS_LABELS)
    # Publishing sigma is what makes a displayed score reversible.
    assert pitching["score_sigma"] > 0

    stuff = models["stuff"]
    assert stuff["target"] == "whiff_given_swing"
    assert stuff["class_labels"] is None


# --- T4.1 training/serving parity -----------------------------------------

def test_service_reproduces_offline_prediction_exactly(client, sample_frame):
    """The reason this test exists: a divergence here is silent in production."""
    model = artifacts.load_model(config.MODELS_DIR / "pitch-outcome-v1.0.0-pitching")

    subset = sample_frame.head(8)
    X = features.select_matrix(subset, "pitching")
    offline_proba = model.predict(X)
    offline_xrv = runvalue.expected_run_value(
        offline_proba, subset["count_state"], model.rv_table)
    offline_score = model.scaler.to_score(offline_xrv)

    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()

    for i, pred in enumerate(body["predictions"]):
        for j, label in enumerate(config.CLASS_LABELS):
            assert pred["probabilities"][label] == pytest.approx(
                offline_proba[i][j], abs=1e-12), f"probability drift on {label}"
        assert pred["scored_quantity"] == pytest.approx(offline_xrv[i], abs=1e-12)
        assert pred["score"] == pytest.approx(offline_score[i], abs=1e-9)


def test_stuff_variant_reproduces_offline_prediction(client, sample_frame):
    model = artifacts.load_model(config.MODELS_DIR / "pitch-outcome-v1.0.0-stuff")
    X = features.select_matrix(sample_frame.head(8), "stuff")
    offline = model.predict(X)

    body = client.post("/v1/predict", json=to_payload(sample_frame, "stuff")).json()
    for i, pred in enumerate(body["predictions"]):
        assert pred["whiff_probability"] == pytest.approx(offline[i], abs=1e-12)
        assert pred["probabilities"] is None, "the binary model has no class vector"


# --- T4.2 feature contract ------------------------------------------------

def test_unknown_feature_is_rejected_not_ignored(client, sample_frame):
    """A misspelled feature would otherwise arrive as a missing value.

    The pitch would then be scored on incomplete input, with nothing anywhere
    reporting a problem. Rejecting the payload is the whole point.
    """
    payload = to_payload(sample_frame, "pitching", n=1)
    payload["pitches"][0]["features"]["realease_speed"] = 95.0  # typo

    resp = client.post("/v1/predict", json=payload)
    assert resp.status_code == 400
    body = resp.json()
    assert body["title"] == "Feature contract violation"
    assert "realease_speed" in body["errors"][0]["message"]


def test_location_feature_rejected_by_the_stuff_variant(client, sample_frame):
    """StuffScore must describe the pitch, not where it went."""
    payload = to_payload(sample_frame, "stuff", n=1)
    payload["pitches"][0]["features"]["plate_x_bat"] = 0.3

    resp = client.post("/v1/predict", json=payload)
    assert resp.status_code == 400
    assert "plate_x_bat" in resp.json()["errors"][0]["message"]


def test_missing_feature_is_allowed(client, sample_frame):
    """A device can fail to read a value; the model handles missing natively.

    Imputing a mean here would invent a measurement that was never taken.
    """
    payload = to_payload(sample_frame, "pitching", n=1)
    payload["pitches"][0]["features"].pop("release_spin_rate", None)

    resp = client.post("/v1/predict", json=payload)
    assert resp.status_code == 200


def test_outcome_model_requires_count_state(client, sample_frame):
    """Run values are looked up per count; without it the score is undefined."""
    payload = to_payload(sample_frame, "pitching", n=1)
    payload["pitches"][0]["count_state"] = None

    resp = client.post("/v1/predict", json=payload)
    assert resp.status_code == 400
    assert "count_state" in resp.json()["errors"][0]["field"]


def test_unknown_variant_is_404(client, sample_frame):
    payload = to_payload(sample_frame, "pitching", n=1)
    payload["variant"] = "nonsense"
    assert client.post("/v1/predict", json=payload).status_code == 422


def test_empty_batch_is_rejected(client):
    resp = client.post("/v1/predict", json={"variant": "pitching", "pitches": []})
    assert resp.status_code == 422


# --- T4.3 batch consistency -----------------------------------------------

def test_batching_does_not_change_results(client, sample_frame):
    batch = client.post("/v1/predict", json=to_payload(sample_frame, "pitching", n=8)).json()

    singles = []
    full = to_payload(sample_frame, "pitching", n=8)
    for pitch in full["pitches"]:
        one = client.post("/v1/predict",
                          json={"variant": "pitching", "pitches": [pitch]}).json()
        singles.append(one["predictions"][0])

    for b, s in zip(batch["predictions"], singles, strict=True):
        assert b["score"] == pytest.approx(s["score"], abs=1e-9)
        assert b["predicted_class"] == s["predicted_class"]


def test_pitch_ids_are_echoed_in_order(client, sample_frame):
    payload = to_payload(sample_frame, "pitching", n=5)
    body = client.post("/v1/predict", json=payload).json()

    sent = [p["pitch_id"] for p in payload["pitches"]]
    got = [p["pitch_id"] for p in body["predictions"]]
    assert got == sent, "a batch response must be matchable to its request"


# --- T4.4 score derivation ------------------------------------------------

def test_probabilities_sum_to_one(client, sample_frame):
    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()
    for pred in body["predictions"]:
        assert sum(pred["probabilities"].values()) == pytest.approx(1.0, abs=1e-9)


def test_predicted_class_is_the_argmax(client, sample_frame):
    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()
    for pred in body["predictions"]:
        expected = max(pred["probabilities"], key=pred["probabilities"].get)
        assert pred["predicted_class"] == expected


def test_score_is_reversible(client, sample_frame):
    """A displayed score must be traceable back to the quantity behind it.

    This is what makes the presentation score defensible rather than a number
    with a nice range: it is a transformation of xRV, not a judgement.
    """
    model = artifacts.load_model(config.MODELS_DIR / "pitch-outcome-v1.0.0-pitching")
    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()

    for pred in body["predictions"]:
        back = model.scaler.to_xrv(np.array([pred["score"]]))[0]
        assert back == pytest.approx(pred["scored_quantity"], abs=1e-6)


def test_lower_expected_runs_scores_higher(client, sample_frame):
    """Score direction: a pitch that costs the batting team runs is a good one."""
    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()
    preds = sorted(body["predictions"], key=lambda p: p["scored_quantity"])
    scores = [p["score"] for p in preds]
    assert scores == sorted(scores, reverse=True)


def test_higher_whiff_probability_scores_higher(client, sample_frame):
    body = client.post("/v1/predict", json=to_payload(sample_frame, "stuff")).json()
    preds = sorted(body["predictions"], key=lambda p: p["whiff_probability"])
    scores = [p["score"] for p in preds]
    assert scores == sorted(scores)


# --- T4.6 explanations ----------------------------------------------------

def test_explanations_are_off_by_default(client, sample_frame):
    """SHAP costs materially more than a prediction; the live path skips it."""
    body = client.post("/v1/predict", json=to_payload(sample_frame, "pitching")).json()
    assert all(p["explanation"] is None for p in body["predictions"])


def test_shap_contributions_reconstruct_the_prediction(client, sample_frame):
    payload = to_payload(sample_frame, "pitching", n=2)
    payload["explain"] = True
    payload["top_n"] = 20

    body = client.post("/v1/predict", json=payload).json()
    for pred in body["predictions"]:
        exp = pred["explanation"]
        assert exp is not None
        assert exp["target_class"] == pred["predicted_class"]
        assert exp["space"] == "log_odds"

        # Additivity is what makes SHAP an explanation rather than a ranking:
        # the base value plus every contribution equals the prediction. It
        # holds in margin space, not after softmax, which is why the
        # explanation reports the margin and carries the probability
        # separately rather than pretending to decompose it.
        total = (exp["base_value"]
                 + sum(c["shap"] for c in exp["contributions"])
                 + exp["other_contribution"])
        assert total == pytest.approx(exp["predicted_value"], abs=1e-6)
        assert exp["features_shown"] <= exp["features_total"]

        assert 0.0 <= exp["predicted_probability"] <= 1.0
        assert exp["predicted_probability"] == pytest.approx(
            max(pred["probabilities"].values()), abs=1e-9)


# --- artifact integrity ---------------------------------------------------

def test_loader_rejects_a_card_that_disagrees_with_its_booster(tmp_path):
    """A mismatched pair would score every pitch on the wrong columns."""
    import json
    import shutil

    src = config.MODELS_DIR / "pitch-outcome-v1.0.0-pitching"
    dst = tmp_path / "broken"
    shutil.copytree(src, dst)

    card = json.loads((dst / "model_card.json").read_text())
    card["feature_columns"] = list(reversed(card["feature_columns"]))
    (dst / "model_card.json").write_text(json.dumps(card))

    with pytest.raises(ValueError, match="disagree on features"):
        artifacts.load_model(dst)


def test_outcome_model_without_a_run_value_table_is_rejected(tmp_path):
    import json
    import shutil

    src = config.MODELS_DIR / "pitch-outcome-v1.0.0-pitching"
    dst = tmp_path / "no-rv"
    shutil.copytree(src, dst)

    card = json.loads((dst / "model_card.json").read_text())
    card.pop("run_value_table", None)
    (dst / "model_card.json").write_text(json.dumps(card))

    with pytest.raises(ValueError, match="run-value table"):
        artifacts.load_model(dst)


def test_correlation_id_is_carried_through(client, sample_frame):
    """One pitch's journey must stay greppable across both services."""
    resp = client.post("/v1/predict",
                       json=to_payload(sample_frame, "pitching", n=1),
                       headers={"X-Correlation-ID": "01JX7KTEST"})
    assert resp.headers["X-Correlation-ID"] == "01JX7KTEST"
    assert resp.headers["X-Request-ID"]
