# Changelog

All notable changes to this project, one entry per phase. The project was
built in thirteen phases, and each one produced something demonstrable on its
own — no phase leaves an intermediate artifact that only makes sense next
time.

The format follows [Keep a Changelog](https://keepachangelog.com/1.1.0/).

## [0.12.0] — Demo and documentation

### Added
- `make demo`: the whole system from nothing in one command — infrastructure,
  topics, schema, images, services, then traffic, in that order
- A single image for all six Go binaries, chosen at start time rather than
  build time so the API and the processor cannot drift apart
- Twelve architecture decision records, two of them marked *changed* because
  measurement disproved them
- A ten-minute demo script, twenty interview questions with answers, and a
  list of the questions that cannot honestly be answered
- An audit of every difference between the phase 0 design and what was built

### Fixed
- `make demo` never returned: a shell-backgrounded replay held the recipe's
  stdout open, so piping it anywhere waited for an EOF that only arrived when
  the replay finished
- The dashboard's health check used `localhost`, which resolves to `::1`
  inside the container while nginx listens on IPv4

## [0.11.0] — Hardening

### Added
- Five chaos scenarios that stop real containers under live traffic
- `cmd/loadtest`: REST percentiles from every sample, and hundreds of held
  WebSocket connections
- CI: four jobs across Go, the dashboard, Python and container images
- `docs/hardening-report.md`, including eleven findings deliberately left open

### Fixed
- **A transient Kafka dial failure killed the processor.** A failed poll
  returned an error that reached `main` and exited. Found by a load test
  exhausting file descriptors; in production it is a rolling broker upgrade
- The Redis pool was ten connections against fifty concurrent workers: correct
  behaviour, but the cache was quietly doing less than it appeared to. Raising
  it took the maximum latency from 2.6 s to 110 ms
- Successful cache *writes* were counted as hits, inflating the hit ratio
- The dashboard container started as root
- Three dependency advisories closed: `golang.org/x/text`, `react-router`,
  `vitest`

### Measured
- 1,484 rps with p99 at 78 ms and no failures; 500 of 500 WebSocket
  connections; 86.7 pitches/s produced; 28.4 ms to process one pitch

## [0.10.0] — Observability

### Added
- Twenty-one application metrics plus runtime metrics, declared in one file so
  the cardinality rule has one place to be enforced
- Five Grafana dashboards, provisioned from the repository
- `make trace`: one correlation id returns a pitch's whole journey
- Two drift indicators, and a demonstration that 2026 data shifts the
  predicted class mix by +3.5pp toward `BALL`

### Fixed
- Successfully processed messages were labelled `transient`, because that is
  the error enum's zero value
- Consumer lag reported −1: the client library cannot report it inside a
  consumer group, so it is now computed from committed offsets against log ends
- Three dashboard panels rendered nothing — dividing a labelled vector by an
  unlabelled aggregate is valid PromQL that returns empty
- The opening WebSocket snapshot was not counted as a sent message

## [0.9.0] — React dashboard

### Added
- Three pages: athlete overview, outing timeline, live view
- TypeScript types generated from the Go response structs, with a test that
  fails the build when they are stale
- `GET /api/v1/pitches/{id}/explanation`, which the design specified and
  nobody had built

### Fixed
- The stream hook reconnected on every render, because its option callbacks
  were effect dependencies and an inline lambda is a new identity each time
- The dashboard container could not start without the API: nginx resolves a
  literal upstream once, at load, and exits if it cannot
- Explanations timed out — the two-second deadline is right for scoring and
  wrong for attribution, which also builds the explainer on first use

## [0.8.0] — Redis

### Added
- Live session counters (`HINCRBY`) and a cached athlete rollup
- A nil cache that is a valid cache: no method returns an error for a Redis
  failure, so no code path turns one into a failed request

### Changed
- Invalidation is a generation counter, not a key deletion. Deleting one key
  is correct only while the endpoint has one shape, and it has several
- Redis Pub/Sub fan-out was **rejected**: phase 7 already solved it

### Verified
- With Redis stopped, thirteen REST endpoints answer 200, `/readyz` returns
  200 with `degraded: ["redis"]`, and the WebSocket snapshot rebuilds from the
  rows to values identical to the database

## [0.7.0] — WebSocket

### Added
- A lock-free hub: one goroutine owns the registry, one reader and one writer
  per connection
- Sequence numbers assigned on enqueue, so a dropped message leaves a visible
  gap
- Origin validation before the upgrade, even though v1 has no authentication

### Fixed
- Every upgrade returned 500: the access-log wrapper did not forward `Hijack`,
  and the upgrade library type-asserts `http.Hijacker` directly

## [0.6.0] — Replay simulator

### Added
- A device-shaped NDJSON tape and a simulator that publishes to Kafka and
  nothing else, enforced by inspecting the dependency graph
- Synthetic pitch times from game state, seeded per session

### Fixed
- The processor attached `pitch_number` to every variant, and the stuff model
  is trained on pitch shape alone — the feature contract rejected all 344 calls

## [0.5.0] — Kafka event architecture

### Added
- Three work topics and two dead-letter queues, with auto-creation disabled
- One envelope carrying correlation, causation, idempotency and schema version
- At-least-once delivery absorbed by the consumer; errors classified before
  they are handled

### Fixed
- The dead-letter queue silently dropped malformed messages, which are exactly
  the messages it exists to capture
- The client library's package-level transport caches topic metadata
  process-wide

## [0.4.0] — Inference service

### Added
- FastAPI service with a feature-contract check that rejects unknown features
- A Go client with a circuit breaker

## [0.3.0] — REST API

### Added
- Versioned surface under `/api/v1`, RFC 7807 problems, keyset pagination

## [0.2.0] — Domain model and PostgreSQL

### Added
- Eight tables, twelve indexes, thirty-eight check constraints, `sqlc`
- Index-usage proofs via `EXPLAIN (ANALYZE, BUFFERS)`

## [0.1.0] — Data engineering and ML prototype

### Added
- LightGBM models for the 5-class outcome and whiff-given-swing
- A leakage whitelist enforced by tests, and a temporal split that refuses to
  load the test set without an explicit environment variable

### Changed
- The `stuff` model was re-pointed: physical features reach only 0.554 AUC on
  the swing decision

## [0.0.0] — Discovery and design

### Added
- Nine design documents covering the domain, the data, the ML problem, the
  architecture, the API, the event model, the schema and a thirteen-phase plan
