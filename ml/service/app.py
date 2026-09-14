"""FastAPI inference service.

This is the one genuinely separate service in the system, and it passes the
test the others fail: a different runtime (LightGBM and SHAP are a Python
ecosystem), a different release cadence (models retrain on their own
schedule), and a different scaling shape (stateless, CPU-bound). It holds no
database connection and knows no domain rules -- given a feature vector it
returns a number, and that is all.

Run:
    .venv/bin/python -m uvicorn ml.service.app:app --port 8000
"""

from __future__ import annotations

import logging
import os
import sys
import time
import uuid
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import numpy as np
import pandas as pd
from fastapi import FastAPI, Request, Response
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, runvalue

from ml.service import artifacts, schemas
from ml.service import metrics as obs

logging.basicConfig(
    level=os.getenv("PITCHLAB_LOG_LEVEL", "INFO").upper(),
    format='{"ts":"%(asctime)s","level":"%(levelname)s",'
           '"service":"pitchlab-ml","msg":"%(message)s"}',
)
log = logging.getLogger("pitchlab-ml")

SERVICE_VERSION = os.getenv("PITCHLAB_VERSION", "dev")
PROBLEM_BASE = "https://pitchlab.dev/errors/"

# Correlation ids arrive from the Go processor and are carried into this
# service's logs, so one pitch's journey stays greppable across both.
HEADER_CORRELATION_ID = "X-Correlation-ID"
HEADER_REQUEST_ID = "X-Request-ID"

STATE: dict[str, Any] = {"models": {}, "explainers": {}, "null_trackers": {}}


@asynccontextmanager
async def lifespan(app: FastAPI):
    models_dir = Path(os.getenv("PITCHLAB_MODELS_DIR", str(artifacts.default_models_dir())))
    # Loading at startup rather than lazily: a missing or mismatched artifact
    # should stop the process here, where it is obvious, not on the first
    # prediction of a live session.
    STATE["models"] = artifacts.load_all(models_dir)
    loaded_at = time.time()
    for variant, model in STATE["models"].items():
        # Recorded so a dashboard can answer "which build is serving, and
        # since when" without reading a log. A model that quietly failed to
        # reload after a deploy is otherwise invisible.
        obs.MODEL_LOAD_TIMESTAMP.labels(
            model_variant=variant, model_version=model.version
        ).set(loaded_at)
        STATE["null_trackers"][variant] = obs.NullRatioTracker()
    log.info("loaded %d model variants from %s", len(STATE["models"]), models_dir)
    yield
    STATE["models"].clear()
    STATE["explainers"].clear()
    STATE["null_trackers"].clear()


app = FastAPI(
    title="PitchLab Inference",
    version=SERVICE_VERSION,
    lifespan=lifespan,
    docs_url="/docs",
)


def problem(status: int, slug: str, title: str, detail: str,
            request: Request, errors: list[dict[str, str]] | None = None) -> JSONResponse:
    return JSONResponse(
        status_code=status,
        media_type="application/problem+json",
        content=schemas.Problem(
            type=PROBLEM_BASE + slug, title=title, status=status, detail=detail,
            instance=str(request.url.path),
            request_id=getattr(request.state, "request_id", None),
            errors=errors,
        ).model_dump(exclude_none=True),
    )


@app.middleware("http")
async def request_context(request: Request, call_next):
    request_id = request.headers.get(HEADER_REQUEST_ID) or str(uuid.uuid4())
    correlation_id = request.headers.get(HEADER_CORRELATION_ID)
    request.state.request_id = request_id

    started = time.perf_counter()
    response: Response = await call_next(request)
    duration_ms = (time.perf_counter() - started) * 1000

    response.headers[HEADER_REQUEST_ID] = request_id
    if correlation_id:
        response.headers[HEADER_CORRELATION_ID] = correlation_id

    if request.url.path not in ("/healthz", "/metrics"):
        log.info(
            "%s %s %d %.2fms request_id=%s correlation_id=%s",
            request.method, request.url.path, response.status_code,
            duration_ms, request_id, correlation_id or "-",
        )
    return response


@app.exception_handler(RequestValidationError)
async def validation_error(request: Request, exc: RequestValidationError):
    errors = [
        {"field": ".".join(str(p) for p in e.get("loc", [])), "message": e.get("msg", "")}
        for e in exc.errors()
    ]
    return problem(422, "validation-failed", "Validation failed",
                   "the request body is not valid", request, errors)


@app.exception_handler(Exception)
async def unhandled_error(request: Request, exc: Exception):
    # The cause is logged, never returned: internal error text leaks schema
    # and file layout.
    log.exception("unhandled error: %s", exc)
    return problem(500, "internal", "Internal server error",
                   "an unexpected error occurred", request)


# --------------------------------------------------------------------------
# Health
# --------------------------------------------------------------------------

@app.get("/healthz", response_model=schemas.HealthResponse)
async def healthz() -> schemas.HealthResponse:
    """Liveness. Checks nothing, on purpose -- see the API service."""
    return schemas.HealthResponse(status="ok", version=SERVICE_VERSION)


@app.get("/readyz", response_model=schemas.ReadyResponse)
async def readyz(response: Response) -> schemas.ReadyResponse:
    """Readiness. A service with no models loaded cannot answer anything."""
    models = STATE["models"]
    ready = bool(models)
    if not ready:
        response.status_code = 503
    return schemas.ReadyResponse(
        status="ok" if ready else "unavailable",
        version=SERVICE_VERSION,
        models_loaded=sorted(models.keys()),
        checks={"models": "ok" if ready else "none loaded"},
    )


@app.get("/metrics")
async def prometheus_metrics() -> Response:
    """Scrape endpoint.

    Deliberately outside /v1: metrics are infrastructure, not product surface,
    and must not move when the API is versioned.
    """
    # The sliding window publishes on its own schedule, which means a low-
    # traffic service could go a long time with a stale gauge. Flushing on
    # scrape costs nothing and makes the number current.
    for variant, tracker in STATE["null_trackers"].items():
        tracker.flush(variant)

    payload, content_type = obs.render()
    return Response(content=payload, media_type=content_type)


@app.get("/v1/models", response_model=schemas.ModelsResponse)
async def list_models() -> schemas.ModelsResponse:
    out = []
    for model in STATE["models"].values():
        out.append(schemas.ModelInfo(
            version=model.version, variant=model.variant, target=model.target,
            algorithm=model.algorithm,
            feature_columns=list(model.feature_columns),
            feature_count=len(model.feature_columns),
            class_labels=(schemas.CLASS_LABELS
                          if model.target == artifacts.TARGET_OUTCOME else None),
            score_mu=model.scaler.mu, score_sigma=model.scaler.sigma,
            best_iteration=model.best_iteration, metrics=model.metrics,
            git_sha=model.git_sha, created_at=model.created_at,
        ))
    return schemas.ModelsResponse(models=out)


# --------------------------------------------------------------------------
# Prediction
# --------------------------------------------------------------------------

def validate_feature_contract(model, pitches) -> list[dict[str, str]]:
    """Reject a payload that does not match the model's feature contract.

    This is the single most valuable check in the service. Training and
    serving computing features differently is the classic way a model goes
    quietly wrong in production: nothing errors, the numbers just stop
    meaning anything. Unknown keys are rejected rather than ignored, because
    a misspelled feature would otherwise arrive as a missing value.
    """
    expected = set(model.feature_columns)
    errors: list[dict[str, str]] = []

    for i, pitch in enumerate(pitches):
        provided = set(pitch.features)

        unknown = sorted(provided - expected)
        if unknown:
            errors.append({
                "field": f"pitches[{i}].features",
                "message": f"unknown feature(s) for variant {model.variant}: "
                           f"{', '.join(unknown)}",
            })

        # Missing keys are allowed -- a device can fail to read a value and
        # the model handles that. What is not allowed is a key the model has
        # never seen.
        if model.target == artifacts.TARGET_OUTCOME and not pitch.count_state:
            errors.append({
                "field": f"pitches[{i}].count_state",
                "message": "required: run values are looked up per count state",
            })

    return errors


@app.post("/v1/predict", response_model=schemas.PredictResponse)
async def predict(req: schemas.PredictRequest, request: Request):
    models = STATE["models"]
    model = models.get(req.variant)
    if model is None:
        obs.ERRORS_TOTAL.labels(endpoint="predict", reason="unknown_variant").inc()
        return problem(404, "unknown-variant", "Unknown model variant",
                       f"variant {req.variant!r} is not loaded; "
                       f"available: {sorted(models)}", request)

    errors = validate_feature_contract(model, req.pitches)
    if errors:
        # Counted separately from a server failure. A contract violation is
        # the caller sending the wrong shape, and it is the signal that a
        # deploy put a processor and a model out of step.
        obs.ERRORS_TOTAL.labels(endpoint="predict", reason="feature_contract").inc()
        return problem(400, "feature-contract", "Feature contract violation",
                       "the payload does not match the model's feature contract",
                       request, errors)

    started = time.perf_counter()

    frame = model.build_frame([p.features for p in req.pitches])
    raw = model.predict(frame)

    if model.target == artifacts.TARGET_OUTCOME:
        predictions = _outcome_predictions(model, req, raw)
    else:
        predictions = _whiff_predictions(model, req, raw)

    if req.explain:
        explain_started = time.perf_counter()
        _attach_explanations(model, frame, raw, predictions, req.top_n)
        obs.EXPLAIN_DURATION.labels(model_variant=model.variant).observe(
            time.perf_counter() - explain_started)

    _record_prediction_metrics(model, req, predictions, time.perf_counter() - started)

    return schemas.PredictResponse(
        model_version=model.version, variant=model.variant, target=model.target,
        predictions=predictions,
        inference_ms=(time.perf_counter() - started) * 1000,
    )


def _record_prediction_metrics(model, req, predictions, elapsed: float) -> None:
    """Record volume, latency and the two drift indicators."""
    n = len(req.pitches)

    obs.PREDICT_DURATION.labels(
        model_variant=model.variant, batch_size_bucket=obs.batch_bucket(n)
    ).observe(elapsed)
    obs.PREDICTIONS_TOTAL.labels(
        model_variant=model.variant, model_version=model.version
    ).inc(n)

    for p in predictions:
        if p.predicted_class:
            obs.PREDICTED_CLASS_TOTAL.labels(
                model_variant=model.variant, predicted_class=p.predicted_class
            ).inc()

    tracker = STATE["null_trackers"].get(model.variant)
    if tracker is not None:
        tracker.observe(model.variant, [p.features for p in req.pitches])


def _outcome_predictions(model, req, proba: np.ndarray) -> list[schemas.PitchPrediction]:
    counts = pd.Series([p.count_state for p in req.pitches], dtype="object")
    xrv = runvalue.expected_run_value(proba, counts, model.rv_table)
    scores = model.scaler.to_score(xrv)

    out = []
    for i, pitch in enumerate(req.pitches):
        probs = {label: float(proba[i][j])
                 for j, label in enumerate(config.CLASS_LABELS)}
        out.append(schemas.PitchPrediction(
            pitch_id=pitch.pitch_id,
            probabilities=schemas.ClassProbabilities(**probs),
            predicted_class=config.CLASS_LABELS[int(proba[i].argmax())],
            scored_quantity=float(xrv[i]),
            score=float(scores[i]),
        ))
    return out


def _whiff_predictions(model, req, proba: np.ndarray) -> list[schemas.PitchPrediction]:
    # Higher whiff probability must mean a higher score, so the sign is
    # flipped before the shared scaler, which is written for "lower is better".
    scored = -np.asarray(proba, dtype="float64")
    scores = model.scaler.to_score(scored)

    return [
        schemas.PitchPrediction(
            pitch_id=pitch.pitch_id,
            whiff_probability=float(proba[i]),
            scored_quantity=float(scored[i]),
            score=float(scores[i]),
        )
        for i, pitch in enumerate(req.pitches)
    ]


def _attach_explanations(model, frame, raw, predictions, top_n: int) -> None:
    """Attach SHAP contributions.

    Kept off the default path: TreeSHAP costs materially more than a
    prediction, and the live stream needs only the number. A user opening one
    pitch can afford to wait.
    """
    import shap

    explainer = STATE["explainers"].get(model.variant)
    if explainer is None:
        explainer = shap.TreeExplainer(model.booster)
        STATE["explainers"][model.variant] = explainer

    values = explainer.shap_values(frame)
    expected = explainer.expected_value

    # SHAP explains the margin, not the probability, so the margin is what the
    # explanation reports. Asking the booster for raw scores is the only way
    # base_value + contributions can be checked to equal predicted_value.
    margins = model.booster.predict(
        frame, num_iteration=model.best_iteration, raw_score=True)

    for i, pred in enumerate(predictions):
        if model.target == artifacts.TARGET_OUTCOME:
            target_idx = int(np.asarray(raw[i]).argmax())
            target_class = config.CLASS_LABELS[target_idx]
            row = np.asarray(values)[i, :, target_idx] if np.asarray(values).ndim == 3 \
                else np.asarray(values[target_idx])[i]
            base = float(np.atleast_1d(expected)[target_idx])
            predicted = float(np.asarray(margins[i])[target_idx])
            probability = float(np.asarray(raw[i])[target_idx])
        else:
            target_class = "WHIFF"
            row = np.asarray(values)[i]
            base = float(np.atleast_1d(expected)[0])
            predicted = float(np.atleast_1d(margins)[i])
            probability = float(np.atleast_1d(raw)[i])

        ranked = np.argsort(np.abs(row))[::-1]
        order = ranked[:top_n]
        # Everything outside the top N still moved the prediction; reporting
        # the remainder keeps the decomposition complete.
        other = float(row[ranked[top_n:]].sum()) if len(ranked) > top_n else 0.0

        contributions = []
        for j in order:
            name = model.feature_columns[int(j)]
            value = frame.iloc[i, int(j)]
            contributions.append(schemas.FeatureContribution(
                feature=name,
                value=None if pd.isna(value) else float(value)
                if not isinstance(value, str) else None,
                shap=float(row[int(j)]),
            ))

        pred.explanation = schemas.Explanation(
            target_class=target_class, base_value=base,
            predicted_value=predicted, predicted_probability=probability,
            contributions=contributions, other_contribution=other,
            features_shown=len(contributions), features_total=len(row),
        )
