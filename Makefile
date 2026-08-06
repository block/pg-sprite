GO ?= go
COMPOSE ?= docker compose

# PostgreSQL major under test (see docs/postgresql-version-support.md).
PG_VERSION ?= 16
PG_PORT ?= 5432
# Local dev database credentials (compose/compose.yml); test-only defaults.
PG_USER ?= pgsprite
PG_PASSWORD ?= pgsprite
PG_DATABASE ?= pgsprite
# Localhost-only test credentials, parameterized above — not a real secret.
PG_DSN_LOCAL = postgres://$(PG_USER):$(PG_PASSWORD)@localhost:$(PG_PORT)/$(PG_DATABASE)?sslmode=disable# sadscan:disable np.postgres.1

.PHONY: build test test-unit test-db test-supported-postgres test-aws-boundary lint setup db-up db-down clean

build:
	$(GO) build -o bin/pg-sprite ./cmd/pg-sprite
	$(GO) build ./...

test:
	$(GO) test -race ./...

# Unit tests only (no Docker required).
test-unit:
	SKIP_INTEGRATION=1 $(GO) test -race ./...

# Integration suite against the long-lived compose database (no per-test
# containers). Start it first: make db-up [PG_VERSION=14]
test-db:
	PG_DSN="$(PG_DSN_LOCAL)" $(GO) test -race -count=1 ./...

# Full suite against every supported PostgreSQL major (14 -> 18) via
# testcontainers — the local mirror of the CI matrix.
test-supported-postgres:
	@for v in 14 15 16 17 18; do \
		echo "=== PostgreSQL $$v ==="; \
		PG_VERSION=$$v $(GO) test -race -count=1 ./... || exit 1; \
	done

# AWS-boundary tests against Ministack's RDS/Aurora control plane
# (docs/testing.md). Needs Docker only — Ministack is MIT-licensed and
# tokenless. PG_VERSION selects the major of the provisioned database.
test-aws-boundary:
	$(GO) test -race -count=1 -run 'AuroraControlPlane' -v ./internal/testutil/

lint:
	golangci-lint run

# Configure git hooks (relative path so worktrees work too).
setup:
	git config core.hooksPath .githooks

# Start / stop the local development database (compose/compose.yml).
COMPOSE_ENV = PG_VERSION=$(PG_VERSION) PG_PORT=$(PG_PORT) PG_USER=$(PG_USER) PG_PASSWORD=$(PG_PASSWORD) PG_DATABASE=$(PG_DATABASE)

db-up:
	$(COMPOSE_ENV) $(COMPOSE) -f compose/compose.yml up --wait -d

db-down:
	$(COMPOSE_ENV) $(COMPOSE) -f compose/compose.yml down -v

clean:
	rm -rf bin
