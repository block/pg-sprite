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

.PHONY: build test test-unit test-db test-supported-postgres test-aws-boundary lint setup db-up db-down demos clean demo demo-seed demo-check

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
	$(GO) test -tags ministack -race -count=1 -run 'AuroraControlPlane' -v ./internal/testutil/

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

# Re-render the README/docs demo GIFs from their VHS tapes (docs/demos).
# Needs vhs (brew install vhs); the binary and compose database are built
# and started as prerequisites — every tape puts bin/ on PATH, so rendering
# without a built binary would record "command not found" into the GIFs.
demos: build db-up
	cd docs/demos && for t in *.tape; do vhs $$t || exit 1; done

clean:
	rm -rf bin

# Runnable product tour: build the binary, start the compose database,
# reseed the demo tables, and walk the CLI through every planner route,
# the declarative diff, the offline commands, and real executions
# (demo/tour.sh). Rerunnable — each run starts from the same seed. The
# database is left running; stop it with make db-down.
demo: build db-up demo-seed
	PGS="$(CURDIR)/bin/pg-sprite" PG_DSN="$(PG_DSN_LOCAL)" demo/tour.sh

# Reseed the demo tables to the baseline (psql runs inside the compose
# container, so no local client is required). Depends on db-up so it works
# standalone and never races the database under parallel make.
demo-seed: db-up
	$(COMPOSE_ENV) $(COMPOSE) -f compose/compose.yml exec -T postgres \
		psql -v ON_ERROR_STOP=1 -U $(PG_USER) -d $(PG_DATABASE) < demo/seed.sql

# The same tour as a packaged-binary smoke test (CI runs this): asserts on
# --json fields and exit codes only. Needs jq.
demo-check: build db-up demo-seed
	CHECK=1 PGS="$(CURDIR)/bin/pg-sprite" PG_DSN="$(PG_DSN_LOCAL)" demo/tour.sh
