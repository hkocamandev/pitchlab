#!/usr/bin/env bash
#
# Create the project's topics with explicit settings.
#
# Auto-creation is disabled on the broker on purpose. A topic created on
# demand gets the broker default of one partition, which silently removes the
# only thing guaranteeing that a session's pitches are processed in order.
# Better to fail on a missing topic than to succeed with a broken guarantee.
#
# Partition counts are chosen with room to spare because they can be raised
# but never lowered, and raising them changes hash(key) % n -- so every key's
# partition assignment moves and ordering breaks for keys already in flight.
#
# Idempotent: re-running leaves existing topics alone.

set -euo pipefail

BOOTSTRAP="${KAFKA_BOOTSTRAP:-localhost:9092}"
CONTAINER="${KAFKA_CONTAINER:-pitchlab-kafka}"
REPLICATION="${KAFKA_REPLICATION:-1}"

HOUR=3600000
DAY=$((HOUR * 24))

kafka() {
  docker exec -i "$CONTAINER" /opt/kafka/bin/kafka-topics.sh \
    --bootstrap-server "$BOOTSTRAP" "$@"
}

create_topic() {
  local name="$1" partitions="$2" retention_ms="$3" purpose="$4"

  if kafka --list 2>/dev/null | grep -qx "$name"; then
    echo "  = $name (exists)"
    return
  fi

  kafka --create \
    --topic "$name" \
    --partitions "$partitions" \
    --replication-factor "$REPLICATION" \
    --config "retention.ms=$retention_ms" \
    --config "compression.type=snappy" \
    >/dev/null

  printf "  + %-38s %d partitions  %-8s  %s\n" \
    "$name" "$partitions" "$(( retention_ms / DAY ))d" "$purpose"
}

echo "creating topics on $BOOTSTRAP"

# Device readings. Keyed by session, so one outing lands on one partition and
# is therefore consumed in order by a single goroutine.
create_topic "pitchlab.pitch.raw.v1" 6 $((DAY * 7)) \
  "device readings, keyed by session"

# Normalized, scored, persisted pitches. Two independent consumer groups read
# this -- WebSocket fan-out and anomaly evaluation -- which is what earns it a
# topic rather than being an internal function call.
#
# Short retention: this feeds the live view, and the durable record is already
# in PostgreSQL. Keeping a week of it would store the same facts twice.
create_topic "pitchlab.pitch.analyzed.v1" 6 $((DAY * 1)) \
  "scored pitches, keyed by session"

# Anomalies. Keyed by athlete rather than session, because a pitcher's alerts
# are meaningful in sequence and sessions are not.
create_topic "pitchlab.anomaly.detected.v1" 3 $((DAY * 7)) \
  "baseline deviations, keyed by athlete"

# Dead letters. Longer retention than the topics they shadow: a human has to
# look at these, and that can wait until Monday.
create_topic "pitchlab.pitch.raw.v1.dlq" 1 $((DAY * 30)) \
  "unprocessable raw pitches"
create_topic "pitchlab.pitch.analyzed.v1.dlq" 1 $((DAY * 30)) \
  "unprocessable analyzed pitches"

echo
echo "topics:"
kafka --list | sed 's/^/  /'
