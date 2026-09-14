#!/usr/bin/env bash
#
# Follow one pitch through every part of the system, from one identifier.
#
#   scripts/trace-pitch.sh <correlation-id> [log-directory]
#
# The correlation id is minted by the replay simulator, carried unchanged
# through the Kafka envelope and its headers, sent to the inference service as
# a header, written to the pitch row, and echoed to the browser over the
# WebSocket. This script is what that design is for: one value, one command,
# the whole journey.
#
# The stream processor logs it only on warnings and errors, deliberately. An
# info line per pitch is fine at a game's pace and is tens of thousands of
# lines a minute under burst replay, which is the log volume that makes logs
# unusable exactly when they are needed. The durable trace does not depend on
# it: the database row, the Kafka message and the inference log all carry the
# same id.

set -euo pipefail

CORRELATION_ID="${1:-}"
LOG_DIR="${2:-.}"

if [[ -z "$CORRELATION_ID" ]]; then
  echo "usage: $0 <correlation-id> [log-directory]" >&2
  exit 1
fi

DATABASE_URL="${DATABASE_URL:-postgres://pitchlab:pitchlab@localhost:5432/pitchlab?sslmode=disable}"
PG_CONTAINER="${PG_CONTAINER:-pitchlab-postgres}"
KAFKA_CONTAINER="${KAFKA_CONTAINER:-pitchlab-kafka}"

rule() { printf '\n\033[1m── %s ─────────────────────────────────────\033[0m\n' "$1"; }

rule "1. the pitch as stored"
docker exec "$PG_CONTAINER" psql -U pitchlab -d pitchlab -x -c "
  SELECT p.external_pitch_uid,
         p.session_pitch_index,
         p.thrown_at,
         p.pitch_type,
         p.actual_outcome,
         p.prediction_status,
         s.external_session_uid,
         s.is_synthetic,
         a.full_name AS pitcher
  FROM pitches p
  JOIN sessions s ON s.id = p.session_id
  JOIN athletes a ON a.id = p.pitcher_athlete_id
  WHERE p.correlation_id = '${CORRELATION_ID}';" 2>/dev/null || echo "  (no row)"

rule "2. what the model said"
docker exec "$PG_CONTAINER" psql -U pitchlab -d pitchlab -c "
  SELECT mv.variant, mv.version, pr.score, pr.predicted_class, pr.scored_quantity
  FROM pitch_predictions pr
  JOIN pitches p ON p.id = pr.pitch_id
  JOIN model_versions mv ON mv.id = pr.model_version_id
  WHERE p.correlation_id = '${CORRELATION_ID}';" 2>/dev/null || echo "  (none)"

rule "3. inference service log"
grep -h "$CORRELATION_ID" "$LOG_DIR"/*.log 2>/dev/null | grep -i "pitchlab-ml" || echo "  (not in the logs provided)"

rule "4. every other service log"
grep -h "$CORRELATION_ID" "$LOG_DIR"/*.log 2>/dev/null | grep -iv "pitchlab-ml" || echo "  (nothing; the processor logs this id only on warnings)"

rule "5. the event on the wire"
# Read from the beginning and stop at the first match. The header carries the
# same id as the body, which is what lets console tooling filter without
# parsing JSON.
docker exec "$KAFKA_CONTAINER" /opt/kafka/bin/kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 \
  --topic pitchlab.pitch.analyzed.v1 \
  --from-beginning --timeout-ms 15000 \
  --property print.headers=true 2>/dev/null \
  | grep -m1 "$CORRELATION_ID" \
  | cut -c1-400 || echo "  (not found within the timeout)"

echo
