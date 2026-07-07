# pg-sprite

> Working name — see the naming task in the research build tracker.

An online schema-change engine for **Aurora PostgreSQL** (and RDS/community
PostgreSQL 14+): a decoupled **planner → router → executor** design where the
planner classifies each change, the router picks a strategy, and
interchangeable executors carry it out — the cheap native PostgreSQL idiom
when one exists (`CONCURRENTLY`, `NOT VALID` + `VALIDATE`, fast default,
`USING INDEX`), and a log-based, checksum-gated, resumable copy-and-swap when
a genuine table rewrite is unavoidable.

**Status: Phase 0 (scaffold + test harness).** All subcommands are stubs. The
design docs and the phased build plan live in [docs/](docs/) — start with
[docs/README.md](docs/README.md).

The codebase is partitioned into a small safety-critical core and a
periphery — **[SAFETY.md](SAFETY.md)** says which packages are which and the
rules that apply inside the core. Read it before changing anything under
`pkg/`.

## Development

```sh
make build       # build ./... and the bin/pg-sprite binary
make test        # full suite; integration tests need Docker
make test-unit   # unit tests only (SKIP_INTEGRATION=1)
make lint        # golangci-lint
```

Integration tests run against a real PostgreSQL via testcontainers. `PG_VERSION`
selects the major (default 16); CI runs the matrix 14 → 18.
