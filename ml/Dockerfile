# Python 3.12 rather than the 3.14 used for local development.
#
# Every dependency ships cp314 wheels, so 3.14 works; 3.12 is chosen here for
# the container because it is the version with the longest remaining support
# window, and an image that needs to build a wheel from source on a slow
# architecture is a bad image. pandas stays pinned below 3.0 either way:
# pybaseball declares an unbounded pandas>=1.0.3 but predates that API.
FROM python:3.12-slim AS base

# LightGBM links against OpenMP at runtime and the wheel does not vendor it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 curl \
 && rm -rf /var/lib/apt/lists/*

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

# Requirements are copied first so the dependency layer survives source edits.
COPY ml/requirements.txt ./ml/requirements.txt
RUN pip install --no-cache-dir -r ml/requirements.txt

COPY ml/ ./ml/

# Run as a non-root user. The service reads model files and answers HTTP; it
# has no reason to hold root in the container.
RUN useradd --create-home --uid 10001 pitchlab \
 && chown -R pitchlab:pitchlab /app
USER pitchlab

ENV PITCHLAB_MODELS_DIR=/models \
    PYTHONPATH=/app:/app/ml

EXPOSE 8000

# Liveness checks nothing but the process, for the same reason the Go service
# does: a dependency-aware liveness probe turns a slow dependency into a
# restart storm.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 \
    CMD curl -fsS http://localhost:8000/healthz || exit 1

CMD ["python", "-m", "uvicorn", "ml.service.app:app", \
     "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
