# Buzz corpus replay

A throwaway-Docker harness for assessing pg-sprite against a real, public schema-change
corpus: the SQLx migration history of [block/buzz](https://github.com/block/buzz), a
Rust service whose relay database exercises range partitioning, composite foreign keys,
enum types, generated TSVECTOR columns, GIN and partial indexes, and PL/pgSQL triggers —
a demanding, honest sample of what production PostgreSQL schemas look like.

Everything here runs against a local `postgres:17-alpine` container with state confined
to the container. The corpus is fetched from the public buzz repository at a pinned
commit; nothing ever points at a real environment.

## Why this exists

pg-sprite's Go suite tests the engine against schemas written *for* the tests. A replay
of an independent project's full history tests something different: how much of a real
service's schema-change workload lands in each support tier (see
[docs/capabilities.md](../../docs/capabilities.md)) —

- **T1** — table-shape changes pg-sprite executes online today (the corpus's plain
  `CREATE TABLE`, `ADD COLUMN`, and concurrent-index candidates): expect success.
- **T2** — planned capability, a typed refusal today (foreign keys, partitioned-parent
  indexes, rewrites pending copy-and-swap): expect the refusal to fire, with the right
  reason.
- **T3** — out of scope by design (PL/pgSQL functions and triggers, data changes,
  dynamic `DO` blocks, extensions): applied via psql only to keep database state
  advancing; never assessed.

The corpus files are buzz's SQLx migrations (their terminology, cited as-is); pg-sprite
assesses only the schema-change content inside them.

## Usage

```sh
./fetch.sh          # download the corpus (pinned commit) into corpus/
./harness.sh up     # start postgres:17-alpine, apply corpus 0001 as baseline
./harness.sh dsn    # DSN for pg-sprite / any client on the host
./harness.sh reset  # back to the pristine baseline (drop + recreate + re-apply)
./harness.sh down   # remove the container and all state
```

The baseline is corpus file `0001_initial_schema.sql` applied via psql: bootstrap DDL on
an empty database has no online-safety problem for pg-sprite to solve, so the harness
applies it directly — pg-sprite enters the picture for the changes *after* the baseline.

`PORT` (default 5439, clear of the compose database's 5432) and `CONTAINER` (default
`pgsprite-buzz-replay`) are overridable via environment.

## What the harness is not

- Not part of `make test` or CI — the Go suite remains the correctness oracle; this is
  an assessment tool run deliberately.
- Not a second demo — [demo/](../../demo/) tours the CLI; this replays someone else's
  history.
- Not a vendored copy of buzz — `corpus/` is gitignored and re-fetched; the pin in
  `fetch.sh` records exactly which buzz commit the assessment ran against.
