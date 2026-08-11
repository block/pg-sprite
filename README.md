# pg-sprite

> [!WARNING]
> **Work in progress — not ready for any use.** This project is under active
> early-stage development. There are no releases, no stability guarantees, and
> no support. Interfaces, behavior, on-disk/database artifacts, and the CLI
> surface may all change without notice. Do **not** run this against any
> database you care about.

> Working name — see the naming task in the research build tracker.

An online schema-change engine for **PostgreSQL** (community, RDS, and
Aurora; 14+): a decoupled **planner → router → executor** design where the
planner classifies each change, the router picks a strategy, and
interchangeable executors carry it out — the cheap native PostgreSQL idiom
when one exists (`CONCURRENTLY`, `NOT VALID` + `VALIDATE`, fast default,
`USING INDEX`), while a log-based, checksum-gated, resumable copy-and-swap for
genuine table rewrites lands in a later phase.

The planner is PostgreSQL's missing `ALGORITHM=` / `LOCK=` declaration: MySQL
lets authors assert a cost bracket and a concurrency impact and fails closed
when either can't be honored — PostgreSQL silently runs whichever cost
applies. pg-sprite proves both dimensions before execution, routes each
change to the safest sequence that exists, and refuses with a structured
verdict when it can't prove one (see
[docs/postgres-online-ddl-reference.md](docs/postgres-online-ddl-reference.md)).

**Status: Phases 1 and 2.1–2.5.** The parse boundary, declarative diff,
classifier, router seam, versioned dry-run plan report, offline linter, and
advisory `suggest` command are implemented. `pg-sprite migrate --alter '…'` runs a bounded optimistic
native attempt; routed execution beyond that attempt lands in Phase 3.
Changes without an available backend get a structured refusal (exit code 2).
The design docs and the phased
build plan live in [docs/](docs/) — start with
[docs/README.md](docs/README.md); the vision — what pg-sprite is and is not —
is [docs/vision.md](docs/vision.md).

The codebase is partitioned into a small safety-critical core and a
periphery — **[SAFETY.md](SAFETY.md)** says which packages are which and the
rules that apply inside the core. Read it before changing anything under
`pkg/`.

## Install

Release archives for linux/darwin on amd64/arm64, with `checksums.txt`, are
published on the [releases page](https://github.com/block/pg-sprite/releases)
once tags exist. The binary is pure Go (the SQL parser is Wasm), so on any
other platform — or without waiting for a release — `go install` works with
no C toolchain:

```sh
go install github.com/block/pg-sprite/cmd/pg-sprite@latest
```

## Development

```sh
make setup       # one-time: configure git hooks (.githooks)
make build       # build ./... and the bin/pg-sprite binary
make test        # full suite; integration tests need Docker
make test-unit   # unit tests only (SKIP_INTEGRATION=1)
make lint        # golangci-lint
```

Integration tests run against a real PostgreSQL via testcontainers. `PG_VERSION`
selects the major (default 16); CI runs the matrix 14 → 18. To iterate against a
long-lived local database instead of per-test containers:

```sh
make db-up PG_VERSION=14   # start PostgreSQL 14 on localhost via compose
make test-db               # run the suite against it (PG_DSN)
make db-down               # stop and discard it
```

`make test-supported-postgres` runs the full suite against every supported
major (14 → 18) — the local mirror of the CI matrix. See
[docs/testing.md](docs/testing.md) for the test-suite layout, what each build
phase owes, and the vanilla-PostgreSQL-vs-real-Aurora validation boundary.

## Contributing

Not yet — see [CONTRIBUTING](CONTRIBUTING.md). Safety-relevant issue
reports are welcome even at this stage.

## License

[Apache 2.0](LICENSE)
