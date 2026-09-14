"""Prometheus metrics for the inference service.

Two of these exist specifically to make a data problem visible in production
rather than in a post-mortem: the predicted-class distribution and the
per-feature null ratio. The 2026 rule change found during data analysis is the
worked example -- plate coordinates moved to a different reference point and
zone boundaries became rule-defined rather than measured. A model trained
before that keeps answering confidently while its inputs mean something else.

Nothing in the pipeline would have raised an error. What shifts is the *shape*
of the traffic: the class mix moves, or a feature that was always present
starts arriving null. These two metrics are where that shows up.
"""

from __future__ import annotations

from typing import Any

from prometheus_client import (
    CONTENT_TYPE_LATEST,
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

# A private registry, matching the Go services. The default registry is global
# state any imported library can write to, and a test that asserts what this
# service exports should not depend on what else happens to be installed.
REGISTRY = CollectorRegistry()

# Batch size is bucketed rather than used directly: the Go processor sends one
# pitch at a time on the live path and larger batches on backfills, and the
# exact count would be an unbounded label.
PREDICT_DURATION = Histogram(
    "ml_predict_duration_seconds",
    "Time to score one request, measured inside the service.",
    ["model_variant", "batch_size_bucket"],
    buckets=(0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5),
    registry=REGISTRY,
)

PREDICTIONS_TOTAL = Counter(
    "ml_predictions_total",
    "Pitches scored.",
    ["model_variant", "model_version"],
    registry=REGISTRY,
)

# Drift indicator 1. The class mix is stable for a given population; a step
# change means the population changed, the inputs changed, or the model is
# being asked something it was not trained for.
PREDICTED_CLASS_TOTAL = Counter(
    "ml_predicted_class_total",
    "Predicted classes, for distribution drift.",
    ["model_variant", "predicted_class"],
    registry=REGISTRY,
)

# Drift indicator 2. A feature that was always present suddenly arriving null
# is an upstream change -- a renamed column, a device firmware update, a
# regime change in the source data.
FEATURE_NULL_RATIO = Gauge(
    "ml_feature_null_ratio",
    "Share of recent requests in which a feature was absent.",
    ["model_variant", "feature_name"],
    registry=REGISTRY,
)

MODEL_LOAD_TIMESTAMP = Gauge(
    "ml_model_load_timestamp_seconds",
    "When a model artifact was loaded.",
    ["model_variant", "model_version"],
    registry=REGISTRY,
)

EXPLAIN_DURATION = Histogram(
    "ml_explain_duration_seconds",
    "Time to attribute one prediction.",
    ["model_variant"],
    buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
    registry=REGISTRY,
)

ERRORS_TOTAL = Counter(
    "ml_request_errors_total",
    "Rejected or failed requests.",
    ["endpoint", "reason"],
    registry=REGISTRY,
)


def batch_bucket(n: int) -> str:
    """Reduce a batch size to a bounded label."""
    if n <= 1:
        return "1"
    if n <= 10:
        return "2-10"
    if n <= 100:
        return "11-100"
    return "100+"


class NullRatioTracker:
    """Tracks how often each feature is absent, over a sliding count.

    A ratio over a fixed window rather than a cumulative one. A cumulative
    ratio is dominated by history: a feature that has been present for a
    million pitches and is null for the last thousand barely moves, which is
    exactly the case the metric exists to catch. The window forgets, so a step
    change shows up as a step.
    """

    def __init__(self, window: int = 1000) -> None:
        self._window = window
        self._seen: dict[str, int] = {}
        self._missing: dict[str, int] = {}
        self._count = 0

    def observe(self, variant: str, features: list[dict[str, Any]]) -> None:
        if not features:
            return

        # The union of keys across the batch, so a feature omitted by every
        # pitch in the batch still counts as missing rather than vanishing
        # from the denominator.
        known = set(self._seen)
        for row in features:
            known.update(row.keys())

        for name in known:
            present = sum(
                1 for row in features if row.get(name) is not None
            )
            self._seen[name] = self._seen.get(name, 0) + len(features)
            self._missing[name] = self._missing.get(name, 0) + (len(features) - present)

        self._count += len(features)
        if self._count >= self._window:
            self._publish(variant)
            self._decay()

    def _publish(self, variant: str) -> None:
        for name, seen in self._seen.items():
            if seen <= 0:
                continue
            ratio = self._missing.get(name, 0) / seen
            FEATURE_NULL_RATIO.labels(
                model_variant=variant, feature_name=name
            ).set(ratio)

    def _decay(self) -> None:
        # Halved rather than cleared, so the gauge does not jump around on the
        # first few requests of each new window.
        self._seen = {k: v // 2 for k, v in self._seen.items()}
        self._missing = {k: v // 2 for k, v in self._missing.items()}
        self._count //= 2

    def flush(self, variant: str) -> None:
        """Publish immediately, without waiting for the window to fill."""
        self._publish(variant)


def render() -> tuple[bytes, str]:
    """Render the registry for a scrape."""
    return generate_latest(REGISTRY), CONTENT_TYPE_LATEST
