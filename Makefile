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

MIGRATE_IMAGE := migrate/migrate:v4.18.1
MIGRATE := docker run --rm --network host \
	-v "$(PWD)/migrations:/migrations" $(MIGRATE_IMAGE) \
	-path=/migrations -database "$(DATABASE_URL)"

VENV := .venv/bin/python

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

.PHONY: seed
seed: ## Register the replay device and the trained model versions
	go run ./cmd/seed

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
		go test -race -tags=integration -p 1 ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

# --- ml --------------------------------------------------------------------

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
ml-load: ## Load real pitches into PostgreSQL (temporary, until phase 6)
	$(VENV) ml/scripts/load_to_postgres.py --sessions 20

# --- combined --------------------------------------------------------------

.PHONY: check
check: vet test ml-test ## Everything that must pass before a commit
