<h1 align="center">pg-sprite</h1>

<p align="center"><b>Change the shape of live PostgreSQL tables while your application keeps reading and writing them</b></p>

<div align="center">

[![CI](https://github.com/block/pg-sprite/actions/workflows/ci.yml/badge.svg?branch=main&event=push)](https://github.com/block/pg-sprite/actions/workflows/ci.yml?query=branch%3Amain+event%3Apush)
[![Slot Release Proof](https://github.com/block/pg-sprite/actions/workflows/slot-release-proof.yml/badge.svg?branch=main&event=schedule)](https://github.com/block/pg-sprite/actions/workflows/slot-release-proof.yml?query=event%3Aschedule)
[![Vulnerability Scan](https://github.com/block/pg-sprite/actions/workflows/vulncheck.yml/badge.svg?branch=main)](https://github.com/block/pg-sprite/actions/workflows/vulncheck.yml?query=branch%3Amain)
[![Release](https://img.shields.io/github/v/release/block/pg-sprite?cacheSeconds=3600)](https://github.com/block/pg-sprite/releases/latest)
[![PostgreSQL tested: 14 | 15 | 16 | 17 | 18](https://img.shields.io/badge/PostgreSQL%20tested-14%20%7C%2015%20%7C%2016%20%7C%2017%20%7C%2018-336791?logo=postgresql&logoColor=white)](docs/postgresql-version-support.md)
[![Maturity: preview](https://img.shields.io/badge/maturity-preview-orange)](docs/capabilities.md)

</div>

<p align="center">
  <a href="./docs/vision.md">Vision</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="./docs/cli.md">CLI guide</a> ·
  <a href="#docs">Docs</a>
</p>

---

pg-sprite is an online schema-change engine for PostgreSQL: community, RDS, and
Aurora, tested on 14 through 18. Give it a DDL statement or a desired
`CREATE TABLE` file. It works out what the change would lock and rewrite, runs
the safest sequence that reaches the same end state, and refuses with a typed
verdict when it cannot prove one.

> [!IMPORTANT]
> **Preview: ready to try.** Everyday changes such as adding a column, an index,
> or a constraint run today with short locks. Rewriting a whole table, such as
> changing a column's type, is in preview and runs only with `--copy-and-swap`.
> We're not at 1.0 yet, so commands and output may change between releases.
> `pg-sprite capabilities --json` says which changes are `ga` and which are `preview`.

## How it works

A **planner** classifies each change, a **router** picks a strategy, and an
**executor** carries it out:

- **Native idioms** when PostgreSQL has one: `CREATE INDEX CONCURRENTLY`,
  `NOT VALID` + `VALIDATE`, fast defaults, `ADD CONSTRAINT … USING INDEX`.
- **Copy-and-swap** for genuine rewrites: a shadow table filled in chunks while
  writes stream through logical decoding, checksum-verified, then swapped in.
  It is resumable and runs a general `ALTER COLUMN TYPE` today.
- **A typed refusal** when neither is safe. Nothing runs, and the verdict
  carries a `reason` and `class` that automation can route on.

The planner is PostgreSQL's missing MySQL-style `ALGORITHM=` / `LOCK=`
declaration. It proves the cost and the lock impact before anything executes
([reference](docs/postgres-online-ddl-reference.md)). Anything that is not
changing a table's shape, such as data backfills or access control, is another
tool's job; [capabilities.md](docs/capabilities.md) names which.

## What it looks like

![pg-sprite replacing a blocking ADD CONSTRAINT with the safer online sequence: dry-run, real run, then the catalog proof](docs/demos/improve.gif)

- **Improve:** a blocking form is replaced with the safer online sequence (above;
  why it is safer is in [safer-sequences.md](docs/safer-sequences.md)).
- **Diff:** a desired `CREATE TABLE` file in, a classified plan out
  ([demo](docs/demos/diff-greenfield.gif)).
- **Refuse:** no safe path exists, so nothing runs and the command exits 2
  ([demo](docs/demos/refuse.gif)).
- **Lint:** blocking idioms flagged offline, no database needed
  ([demo](docs/demos/lint.gif)).

Every output shape, as text and JSON, is in
[cli-output-examples.md](docs/cli-output-examples.md).

## Install

```sh
brew install block/tap/pg-sprite
```

Release archives for linux and darwin on amd64 and arm64, with `checksums.txt`,
are on the [releases page](https://github.com/block/pg-sprite/releases). The
binary is pure Go, so `go install` works anywhere with no C toolchain:

```sh
go install github.com/block/pg-sprite/cmd/pg-sprite@latest
```

## Quick start

```sh
pg-sprite lint changes.sql                              # offline: flag blocking idioms

export PGSPRITE_URL='postgres://user:pass@primary:5432/app'
pg-sprite migrate --dry-run --alter 'ALTER TABLE app.orders ADD CONSTRAINT qty_positive CHECK (qty > 0)'
pg-sprite migrate --alter 'ALTER TABLE app.orders ADD CONSTRAINT qty_positive CHECK (qty > 0)'

pg-sprite pull --schema app --out schema                # one desired file per table
pg-sprite diff --schema app --desired schema/orders.sql # what would converge app.orders
```

Connect directly to the primary, not through a pooler
([why](docs/limitations.md#connections-and-poolers)). The
[CLI guide](docs/cli.md) walks through each task with captured output.

## Commands

| Command | Database | What it does |
|---|---|---|
| `migrate` | required | Plan, route, and execute one change under bounded lock and statement budgets ([copy-and-swap flags](docs/copy-and-swap.md)) |
| `migrate --dry-run` | required | The same plan against the real table; executes nothing |
| `diff` | required | Plan the changes that converge a live table onto a desired file; changes nothing |
| [`pull`](docs/pull.md) | required | Write one desired file per supported table in a schema |
| `status` | required | Show live pg-sprite sessions |
| `reap` | required | Drop orphan replication slots that stopped copy-and-swap runs left behind |
| `capabilities` | none | Print the support matrix (`--json` for automation) |
| `fmt` | none | Canonicalize a schema file |
| `lint` | none | Flag patterns the engine would refuse, rewrite, or gate |
| `suggest` | none | Show the safer form the engine would run for risky DDL |

Only `migrate` and `reap` ever write. If a multi-step change fails partway,
[execution-model.md](docs/execution-model.md) says what state the table is in.

## What pg-sprite does not do yet

Every boundary fails closed with a typed refusal, never a silently wrong or
incomplete result. The full list is in
[limitations.md](docs/limitations.md#boundaries-at-a-glance), and the support
matrix is [capabilities.md](docs/capabilities.md).

- **Copy-and-swap is in preview** and carries only `ALTER COLUMN TYPE`; other rewrites refuse ([running it](docs/copy-and-swap.md)).
- **No connection poolers** on pg-sprite's own connection, except Supabase's session endpoint ([poolers](docs/limitations.md#connections-and-poolers)).
- **Foreign keys** a desired file adds run online (`NOT VALID`, then `VALIDATE`, or partition by partition on a partitioned table before PostgreSQL 18, except the shapes [the build refuses](docs/partitioned-foreign-key-design.md#routing-from-a-desired-file); a referenced table or partition tree it does not take stops the whole plan before anything runs, while a conflict on the parent or a catalog change it finds under its lock stops it after the statements ahead of it commit); dropping one runs only through `migrate --alter`.
- **Renames are refused** unless `--force` names the table, since they break running application instances. An index a desired file declares under a new name is the exception: it is renamed in place and keeps serving.
- **Destructive changes** need `--allow-destructive SCHEMA.TABLE`, and desired plans also need `--expect-fingerprint`.
- **Index maintenance** (`DROP INDEX`, `REINDEX`) runs from `migrate --alter` only as an accepted blocking change ([passthrough](docs/lock-budgeted-passthrough.md)); a desired file drops an index concurrently under `--allow-destructive` and `--expect-fingerprint`, and a partitioned table's index only as an accepted blocking drop.
- **Non-table objects** such as views, functions, and triggers are outside the declarative model, apart from adding enum labels.
- **Tables are never dropped**; that stays with the owner of the whole schema.

## Exit codes

| Exit | Meaning |
|---|---|
| 0 | Executed through an online-safe path, or a dry run, `diff`, or `pull` found nothing to refuse |
| 1 | Failed: a PostgreSQL, operational, or usage error. A safer sequence that stopped partway keeps its committed steps, named in `executed_sql` |
| 2 | Refused: no online-safe path. The verdict names the typed `reason` and `class`. A `migrate --desired` plan refused partway keeps the statements before it |
| 3 | Committed through the accepted-blocking passthrough, without an online-safety guarantee |

Gate on non-zero. The full rules, including a lost `COMMIT` and library
callers, are in [cli-output-examples.md](docs/cli-output-examples.md#exit-codes).

## Docs

- [Vision](docs/vision.md): what pg-sprite is and is not
- [Capabilities](docs/capabilities.md) and [limitations](docs/limitations.md): what runs, what refuses, and why
- [CLI guide](docs/cli.md) and [output examples](docs/cli-output-examples.md)
- [Copy-and-swap](docs/copy-and-swap.md): running a table rewrite
- [Execution model](docs/execution-model.md): partial states and how failures are reported
- [Go library](docs/schemabot-integration.md): embedding pg-sprite in an orchestrator through `pkg/migrate` (`Run`, `RunDesired`); SchemaBot is the worked example
- [Safety](SAFETY.md) and [invariants](docs/invariants.md): the safety-critical core and its rules
- [Design docs](docs/README.md): everything else

## Development

```sh
make setup       # one-time: configure git hooks
make build       # build ./... and bin/pg-sprite
make test        # full suite; integration tests need Docker
make test-unit   # unit tests only
make demo        # runnable tour of the CLI against a local PostgreSQL
```

Integration tests run against real PostgreSQL through testcontainers.
[testing.md](docs/testing.md) covers the version matrix and the local database
targets; [demo/README.md](demo/README.md) covers the tour.

## Contributing

Not yet; see [CONTRIBUTING](CONTRIBUTING.md). Safety-relevant issue reports are
welcome even at this stage.

## License

[Apache 2.0](LICENSE)
