# PitchLab developer entry points.
# GNU Make 3.81 on macOS, so nothing newer than that is used here.

SHELL := /bin/bash

DB_USER ?= pitchlab
DB_PASS ?= pitchlab
DB_NAME ?= pitchlab
DB_HOST ?= localhost
DB_PORT ?= 5432
DATABASE_URL ?= postgres://$(DB_USER):$(DB_PASS)@$(DB_HOST):$(DB_PORT)/$(DB_NAME)?sslmode=disable

KAFKA_BROKERS ?= localhost:9094
REDIS_URL     ?= redis://localhost:6379/0
PROMETHEUS_PORT ?= 9090
GRAFANA_PORT    ?= 3000
LOG_DIR         ?= .

# Demo defaults. The seed is fixed, so a rehearsed demo replays identically.
DEMO_SESSIONS ?= 4
DEMO_SPEED    ?= 25
DEMO_TAPE     ?= data/processed/replay_tape_$(SEASON)_$(DEMO_SESSIONS).ndjson
PITCHLAB_API_PORT ?= 8081
PITCHLAB_WEB_PORT ?= 5174

MIGRATE_IMAGE := migrate/migrate:v4.18.1
MIGRATE := docker run --rm --network host \
	-v "$(PWD)/migrations:/migrations" $(MIGRATE_IMAGE) \
	-path=/migrations -database "$(DATABASE_URL)"

VENV := .venv/bin/python

# Replay defaults. Override on the command line:
#   make replay SPEED=5 TAPE=data/processed/replay_tape_2025_3.ndjson
SEASON   ?= 2025
SESSIONS ?= 8
SPEED    ?= 30
TAPE     ?= data/processed/replay_tape_$(SEASON)_$(SESSIONS).ndjson

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- infrastructure --------------------------------------------------------

.PHONY: up
up: ## Start Postgres, Redis and Kafka
	docker compose up -d postgres redis kafka
	@$(MAKE) --no-print-directory wait-db
	@$(MAKE) --no-print-directory wait-kafka

.PHONY: wait-kafka
wait-kafka:
	@until docker compose exec -T kafka \
		/opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list \
		>/dev/null 2>&1; do sleep 2; done
	@echo "kafka ready"

.PHONY: down
down: ## Stop containers, keep volumes
	docker compose down

.PHONY: nuke
nuke: ## Stop containers and delete volumes
	docker compose down -v

.PHONY: wait-db
wait-db:
	@until docker compose exec -T postgres pg_isready -U $(DB_USER) -d $(DB_NAME) \
		>/dev/null 2>&1; do sleep 1; done
	@echo "postgres ready"

.PHONY: kafka-topics
kafka-topics: ## Create Kafka topics with explicit partition and retention settings
	./scripts/create-topics.sh

.PHONY: kafka-ui
kafka-ui: ## Start the Kafka web UI on :8080
	docker compose --profile tools up -d kafka-ui

.PHONY: psql
psql: ## Open a psql shell
	docker compose exec -it postgres psql -U $(DB_USER) -d $(DB_NAME)

# --- migrations ------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Apply all migrations
	$(MIGRATE) up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	$(MIGRATE) down 1

.PHONY: migrate-reset
migrate-reset: ## Roll back everything, then re-apply
	$(MIGRATE) down -all
	$(MIGRATE) up

.PHONY: migrate-version
migrate-version: ## Show current schema version
	$(MIGRATE) version

.PHONY: migrate-new
migrate-new: ## Create a migration pair: make migrate-new NAME=add_widgets
	@test -n "$(NAME)" || (echo "NAME is required" && exit 1)
	$(MIGRATE) create -ext sql -dir /migrations -seq $(NAME)

# --- go --------------------------------------------------------------------

.PHONY: processor
processor: ## Run the stream processor
	go run ./cmd/processor

.PHONY: dlq
dlq: ## Show what is sitting in the raw-pitch dead-letter queue
	go run ./cmd/dlq-replay --dlq pitchlab.pitch.raw.v1.dlq --dry-run --timeout 5s

.PHONY: demo
demo: ## The whole system, from nothing, in one command
	@# Ordered by what has to exist before what: infrastructure, then the
	@# schema, then the models the processor needs to score anything, then the
	@# application, and only then the traffic. Running the replay before the
	@# processor is listening would publish into a topic nobody is reading and
	@# look like the pipeline was broken.
	@echo "==> 1/6  infrastructure (postgres, redis, kafka)   ~30s on a cold start"
	@docker compose up -d postgres redis kafka
	@$(MAKE) --no-print-directory wait-db
	@$(MAKE) --no-print-directory wait-kafka
	@echo "==> 2/6  topics"
	@$(MAKE) --no-print-directory kafka-topics
	@echo "==> 3/6  schema"
	@$(MAKE) --no-print-directory migrate-up
	@echo "==> 4/6  building images                            ~2min on a cold start"
	@docker compose --profile app build
	@echo "==> 5/6  services (inference, processor, api, dashboard)"
	@docker compose --profile app up -d
	@$(MAKE) --no-print-directory wait-api
	@$(MAKE) --no-print-directory seed
	@echo "==> 6/6  replaying $(DEMO_SESSIONS) outings at $(DEMO_SPEED)x"
	@$(MAKE) --no-print-directory demo-replay
	@echo ""
	@echo "  dashboard   http://localhost:$(PITCHLAB_WEB_PORT)"
	@echo "  api         http://localhost:$(PITCHLAB_API_PORT)/api/v1/athletes"
	@echo "  grafana     make observability, then http://localhost:$(GRAFANA_PORT)"
	@echo ""
	@echo "  the replay is running in the background; watch it with:"
	@echo "    docker compose logs -f processor"

.PHONY: demo-replay
demo-replay: ## Replay into the containerised stack
	@# The tape is built on the host, because it needs the dataset, which is
	@# deliberately not in any image.
	@test -f $(DEMO_TAPE) || $(VENV) ml/scripts/export_replay_tape.py 		--season $(SEASON) --sessions $(DEMO_SESSIONS)
	@docker compose run --rm --no-deps 		-v "$(PWD)/$(DEMO_TAPE):/tape.ndjson:ro" 		-e KAFKA_BROKERS=kafka:9092 		--entrypoint /usr/local/bin/replay 		api --tape /tape.ndjson --speed $(DEMO_SPEED) --sessions $(DEMO_SESSIONS) &

.PHONY: wait-api
wait-api:
	@echo -n "    waiting for the api"
	@for i in $$(seq 1 60); do 		if curl -sf http://localhost:$(PITCHLAB_API_PORT)/healthz >/dev/null 2>&1; then 			echo " ok"; exit 0; fi; 		echo -n "."; sleep 2; done; 	echo " timed out"; docker compose --profile app logs api | tail -20; exit 1

.PHONY: demo-down
demo-down: ## Stop everything the demo started, keeping the data
	docker compose --profile app --profile observability down

.PHONY: tape
tape: ## Export a device-shaped replay tape from historical pitches
	$(VENV) ml/scripts/export_replay_tape.py --season $(SEASON) --sessions $(SESSIONS)

.PHONY: replay
replay: ## Replay a tape into Kafka as a live device feed
	go run ./cmd/replay --tape $(TAPE) --speed $(SPEED)

.PHONY: watch
watch: ## Follow one session's live channel from the terminal
	go run ./cmd/wsclient --session $(SESSION)

.PHONY: ts-types
ts-types: ## Regenerate the dashboard's TypeScript types from the Go DTOs
	go run ./cmd/tsgen

.PHONY: web-install
web-install: ## Install dashboard dependencies
	cd web && npm ci

.PHONY: web
web: ## Run the dashboard dev server on :5173 (proxies the API)
	cd web && npm run dev

.PHONY: web-test
web-test: ## Run dashboard tests
	cd web && npm run test

.PHONY: web-build
web-build: ## Type-check and build the dashboard bundle
	cd web && npx tsc -b && npm run build

.PHONY: observability
observability: ## Start Prometheus and Grafana
	docker compose --profile observability up -d prometheus grafana
	@echo "prometheus  http://localhost:$(PROMETHEUS_PORT)"
	@echo "grafana     http://localhost:$(GRAFANA_PORT)  (five dashboards, no login)"

.PHONY: test-chaos
test-chaos: ## Break the system on purpose: stop dependencies under live traffic
	@# Minutes, not seconds, and it stops real containers. Behind its own tag
	@# so the unit suite stays fast and deterministic.
	PITCHLAB_TEST_DATABASE_URL="$(DATABASE_URL)" \
	PITCHLAB_TEST_KAFKA_BROKERS="$(KAFKA_BROKERS)" \
	PITCHLAB_TEST_REDIS_URL="$(REDIS_URL)" \
		go test -tags=chaos -count=1 -timeout=20m -v ./internal/chaos/

.PHONY: loadtest
loadtest: ## REST load test: make loadtest ATHLETE=<id> SESSION=<id>
	go run ./cmd/loadtest --rest --athlete $(ATHLETE) --session $(SESSION) \
		--duration 30s --concurrency 50

.PHONY: loadtest-ws
loadtest-ws: ## Open 500 concurrent WebSocket connections: make loadtest-ws SESSION=<id>
	go run ./cmd/loadtest --ws --session $(SESSION) --connections 500 --duration 30s

.PHONY: audit
audit: ## Dependency and secret scans across all three stacks
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	cd web && npm audit --omit=dev --audit-level=high
	$(VENV) -m pip_audit 2>/dev/null || .venv/bin/pip-audit
	$(VENV) -m ruff check ml/ 2>/dev/null || .venv/bin/ruff check ml/

.PHONY: check-dashboards
check-dashboards: ## Verify every dashboard panel returns data from a live Prometheus
	$(VENV) scripts/check-dashboards.py

.PHONY: trace
trace: ## Follow one pitch through every service: make trace CID=<correlation-id>
	scripts/trace-pitch.sh $(CID) $(LOG_DIR)

.PHONY: seed
seed: ## Register the replay device and the trained model versions
	go run ./cmd/seed

.PHONY: redis-cli
redis-cli: ## Open a redis-cli shell
	docker exec -it pitchlab-redis redis-cli

.PHONY: redis-keys
redis-keys: ## List the keys this project has written
	docker exec pitchlab-redis redis-cli --scan --pattern 'pl:*'

.PHONY: redis-down
redis-down: ## Stop Redis, to show that nothing breaks without it
	docker compose stop redis

.PHONY: redis-up
redis-up: ## Start Redis again
	docker compose start redis

.PHONY: db-reset
db-reset: migrate-reset seed ## Rebuild the schema from scratch and reseed

.PHONY: build
build: ## Build all binaries
	go build ./...

.PHONY: test
test: ## Run Go tests with the race detector
	go test -race ./...

.PHONY: test-integration
test-integration: ## Run tests that need a live database
	@# -p 1 is required: integration packages share one database and each
	@# truncates it between tests. Running packages in parallel makes them
	@# delete each other's fixtures, and concurrent TRUNCATEs deadlock.
	PITCHLAB_TEST_DATABASE_URL="$(DATABASE_URL)" \
	PITCHLAB_TEST_KAFKA_BROKERS="$(KAFKA_BROKERS)" \
	PITCHLAB_TEST_REDIS_URL="$(REDIS_URL)" \
		go test -race -tags=integration -p 1 ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

# --- ml --------------------------------------------------------------------

.PHONY: lint-py
lint-py: ## Lint the Python code
	.venv/bin/ruff check ml/

.PHONY: ml-test
ml-test: ## Run ML contract tests
	cd ml && ../$(VENV) -m pytest

.PHONY: ml-validate
ml-validate: ## Fetch sample data and verify data contracts
	$(VENV) ml/scripts/validate_data.py

.PHONY: ml-pull
ml-pull: ## Pull the full training window
	$(VENV) ml/scripts/pull_seasons.py --seasons 2022 2023 2024 2025 --label full

.PHONY: ml-train
ml-train: ## Train both model variants
	$(VENV) ml/scripts/train_model.py --version pitch-outcome-v1.0.0

.PHONY: ml-serve
ml-serve: ## Run the inference service locally
	$(VENV) -m uvicorn ml.service.app:app --port 8000 --reload

.PHONY: ml-load
ml-load: ## Load pitches straight into PostgreSQL, bypassing the pipeline
	$(VENV) ml/scripts/load_to_postgres.py --sessions 20

# --- combined --------------------------------------------------------------

.PHONY: check
check: vet test ml-test web-test lint-py ## Everything that must pass before a commit
