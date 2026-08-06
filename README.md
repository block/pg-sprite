# pg-sprite

> [!WARNING]
> **Work in progress — not ready for any use.** This project is under active
> early-stage development. There are no releases, no stability guarantees, and
> no support. Interfaces, behavior, on-disk/database artifacts, and the CLI
> surface may all change without notice. Do **not** run this against any
> database you care about.

> Working name — see the naming task in the research build tracker.

An online schema-change engine for **Aurora PostgreSQL** (and RDS/community
PostgreSQL 14+): a decoupled **planner → router → executor** design where the
planner classifies each change, the router picks a strategy, and
interchangeable executors carry it out — the cheap native PostgreSQL idiom
when one exists (`CONCURRENTLY`, `NOT VALID` + `VALIDATE`, fast default,
`USING INDEX`), and a log-based, checksum-gated, resumable copy-and-swap when
a genuine table rewrite is unavoidable.

**Status: Phase 1 (optimistic front door).** `pg-sprite migrate --alter '…'`
runs easy `ALTER TABLE` changes directly under tight lock/statement budgets
and refuses everything else with a structured verdict (exit code 2): index
maintenance gets a pointer to the `CONCURRENTLY` idiom, and changes that need
a table rewrite — caught by the size guard or a cancelled bounded attempt —
get an explicit **not native-safe** verdict. `diff`, `fmt`, and `lint` are
still stubs. The design docs and the phased build plan live in
[docs/](docs/) — start with [docs/README.md](docs/README.md).

The codebase is partitioned into a small safety-critical core and a
periphery — **[SAFETY.md](SAFETY.md)** says which packages are which and the
rules that apply inside the core. Read it before changing anything under
`pkg/`.

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
