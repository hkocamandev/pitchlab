"""Request and response contracts for the inference service."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field, model_validator

from pitchlab_ml import config

Variant = Literal["pitching", "stuff"]


class PitchFeatures(BaseModel):
    """One pitch's feature vector.

    Deliberately permissive about *which* keys arrive and strict about which
    are allowed: the caller sends a flat map, the service checks it against
    the loaded model's contract, and anything unrecognised is rejected rather
    than silently ignored. A typo in a feature name would otherwise become a
    missing value, and the pitch would be scored on incomplete input with no
    error anywhere.
    """

    pitch_id: str | None = Field(
        default=None,
        description="Echoed back so a batch response can be matched to its request",
    )
    count_state: str | None = Field(
        default=None,
        description="Needed to look up run values; required by the outcome model",
    )
    features: dict[str, Any] = Field(default_factory=dict)

    @model_validator(mode="after")
    def _reject_empty(self) -> "PitchFeatures":
        if not self.features:
            raise ValueError("features must not be empty")
        return self


class PredictRequest(BaseModel):
    variant: Variant = "pitching"
    pitches: list[PitchFeatures] = Field(min_length=1, max_length=1000)
    # SHAP is off by default. It costs materially more than a prediction, and
    # the live path needs the prediction only; explanations are pulled on
    # demand when a user opens a pitch.
    explain: bool = False
    top_n: int = Field(default=5, ge=1, le=20)


class ClassProbabilities(BaseModel):
    BALL: float
    CALLED_STRIKE: float
    SWINGING_STRIKE: float
    FOUL: float
    IN_PLAY: float


class FeatureContribution(BaseModel):
    feature: str
    value: float | None
    shap: float


class Explanation(BaseModel):
    """SHAP attribution for one prediction.

    Contributions are in the model's margin (log-odds) space, because that is
    where SHAP is additive: base_value plus every contribution equals
    predicted_value exactly. Softmax is not linear, so the same guarantee does
    not survive a conversion to probability -- reporting these as "probability
    points" would be a convenient lie.

    predicted_probability is carried alongside so a UI can show the familiar
    number without the explanation pretending to decompose it.

    Only the top N contributions are listed, so other_contribution carries the
    sum of everything left out. Without it the listed values would not add up
    and there would be no way to tell truncation from a bug.
    """

    target_class: str
    space: Literal["log_odds"] = "log_odds"
    base_value: float
    predicted_value: float
    predicted_probability: float
    contributions: list[FeatureContribution]
    other_contribution: float = 0.0
    features_shown: int = 0
    features_total: int = 0


class PitchPrediction(BaseModel):
    pitch_id: str | None = None

    # The 5-class model populates probabilities and predicted_class; the
    # stuff model populates whiff_probability. Never both.
    probabilities: ClassProbabilities | None = None
    predicted_class: str | None = None
    whiff_probability: float | None = None

    # The raw model-derived number the score comes from: expected run value
    # for the outcome model, negated whiff probability for the stuff model.
    # Stored so a displayed score can always be traced back.
    scored_quantity: float
    score: float

    explanation: Explanation | None = None


class PredictResponse(BaseModel):
    model_version: str
    variant: str
    target: str
    predictions: list[PitchPrediction]
    inference_ms: float


class ModelInfo(BaseModel):
    version: str
    variant: str
    target: str
    algorithm: str
    feature_columns: list[str]
    feature_count: int
    class_labels: list[str] | None = None
    score_mu: float
    score_sigma: float
    best_iteration: int
    metrics: dict[str, Any]
    git_sha: str | None = None
    created_at: str


class ModelsResponse(BaseModel):
    models: list[ModelInfo]


class HealthResponse(BaseModel):
    status: str
    version: str


class ReadyResponse(BaseModel):
    status: str
    version: str
    models_loaded: list[str]
    checks: dict[str, str]


class Problem(BaseModel):
    """Mirrors the Go service's error shape so a client writes one error path."""

    type: str
    title: str
    status: int
    detail: str | None = None
    instance: str | None = None
    request_id: str | None = None
    errors: list[dict[str, str]] | None = None


CLASS_LABELS = list(config.CLASS_LABELS)
