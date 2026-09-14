# Local dev helpers. Go targets need Go 1.24+ installed; the docker-* targets
# do not (they run the toolchain in a container).

SHELL := /bin/bash
COMPOSE := docker compose

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Resolve dependencies and write go.sum
	go mod tidy

.PHONY: docker-tidy
docker-tidy: ## Same, but without a local Go toolchain
	docker run --rm -v "$(PWD)":/src -w /src golang:1.25-alpine go mod tidy

.PHONY: build
build: ## Build both binaries into ./bin
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker
	go build -o bin/token ./cmd/token

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race ./...

.PHONY: vet
vet: ## go vet + gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

# ---------------------------------------------------------------------------
# Native dev loop (Go on the host, Postgres in Docker).
# Closest thing this project has to `npm run start:dev`.
# All of these source .env first -- the app itself does not read .env files.
# ---------------------------------------------------------------------------

.PHONY: dev
dev: ## ONE COMMAND: Postgres + API + worker, both hot-reloading (Ctrl-C stops all)
	@docker compose up -d postgres
	@docker compose stop api worker >/dev/null 2>&1 || true
	@printf 'waiting for postgres'; \
	  until docker compose exec -T postgres pg_isready -U gotaskqueue -d gotaskqueue >/dev/null 2>&1; do \
	    printf '.'; sleep 1; \
	  done; echo ' ready'
	@set -a && source .env && set +a && \
	  trap 'kill 0' EXIT INT TERM; \
	  air -c .air.api.toml & \
	  air -c .air.worker.toml & \
	  wait

.PHONY: dev-db
dev-db: ## Start ONLY Postgres (published on localhost:5433) for native dev
	$(COMPOSE) up -d postgres

.PHONY: run-api
run-api: ## go run the API once, no reload (needs: make dev-db)
	set -a && source .env && set +a && go run ./cmd/api

.PHONY: run-worker
run-worker: ## go run the worker once, no reload (needs: make dev-db)
	set -a && source .env && set +a && go run ./cmd/worker

.PHONY: dev-api
dev-api: ## API with hot reload on file save (needs: make dev-db)
	set -a && source .env && set +a && air -c .air.api.toml

.PHONY: dev-worker
dev-worker: ## Worker with hot reload on file save (needs: make dev-db)
	set -a && source .env && set +a && air -c .air.worker.toml

.PHONY: dev-token
dev-token: ## Mint a dev JWT using the native toolchain (make dev-token SUB=alice)
	@set -a && source .env && set +a && go run ./cmd/token -sub $${SUB:-local-dev}

.PHONY: up
up: ## Start Postgres + api + worker
	$(COMPOSE) up --build

.PHONY: down
down: ## Stop the stack and delete the database volume
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail worker logs
	$(COMPOSE) logs -f worker

.PHONY: token
token: ## Mint a dev JWT (make token SUB=alice)
	@$(COMPOSE) run --rm token -sub $${SUB:-local-dev}

.PHONY: psql
psql: ## Open a psql shell against the local database
	$(COMPOSE) exec postgres psql -U gotaskqueue -d gotaskqueue
