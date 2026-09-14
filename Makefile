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
