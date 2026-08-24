# Corpus replay

A throwaway-Docker pattern for assessing pg-sprite against a real project's
schema-change history: fetch the project's migration files from its public repository
at a pinned commit, replay them in order against a local PostgreSQL container, and
assert — per statement — that pg-sprite either **executes the change for real** or
produces **exactly the expected typed refusal**.

Everything runs against a local container with state confined to it; nothing ever
points at a real environment. Each project lives in its own directory here
([buzz/](buzz/) is the first) and shares the same four scripts.

## Why this exists

pg-sprite's Go suite tests the engine against schemas written *for* the tests. A
replay of an independent project's full history tests something different: how much
of a real service's schema-change workload lands in each support tier (see
[docs/capabilities.md](../docs/capabilities.md)) —

- **executed** — table-shape changes pg-sprite runs online today: the replay requires
  exit 0 and outcome `executed-natively`, and pg-sprite itself mutates the database —
  real execution, not dry-run.
- **typed refusal** — everything `pg-sprite migrate` declines with a named reason:
  planned capability (partitioned-parent indexes, rewrites pending copy-and-swap) but
  also statements outside `migrate`'s intake, like `CREATE TABLE` — bootstrap DDL that
  capabilities.md classes as having no online-safety problem, yet which the imperative
  CLI still refuses as `unsupported-statement` rather than executing. The replay
  requires exit 2 and *exactly* the expected reason (a reason mismatch is a failure,
  not a pass), then applies the same statement via psql so the history keeps advancing.
- **psql-only** — content out of scope by design (PL/pgSQL functions and triggers,
  data changes, dynamic `DO` blocks, extensions, session-scoped `LOCK`/`SET LOCAL`):
  applied via psql in one transaction, never assessed.

## Assessing your own project

Three inputs define a replay project: the **repository** (a public github.com repo),
a **commit** to pin, and the **path** of the schema-change files inside it. Scaffold
from exactly those:

```sh
./init.sh chuzz https://github.com/example/chuzz main migrations
```

This resolves the ref to a commit, captures the corpus file list at that pin into
`chuzz/project.conf`, gitignores `chuzz/corpus/`, and writes a skeleton
`chuzz/assessment.tsv`. Then:

```sh
make replay REPLAY_PROJECT=chuzz    # fetch + harness + replay, one shot
# curate chuzz/assessment.tsv — probe individual statements with:
#   bin/pg-sprite migrate --url "$(./harness.sh chuzz dsn)" --json --alter '...'
# then rerun until green:
make replay REPLAY_PROJECT=chuzz
```

The first corpus file is assumed to be the baseline (bootstrap DDL applied directly
via psql — on an empty database it has no online-safety problem for pg-sprite to
solve); adjust `BASELINE` in `project.conf` if your history starts differently, or
set it empty to start from an empty database.

## Make targets and scripts

From the repository root (`REPLAY_PROJECT` defaults to `buzz`):

```sh
make replay [REPLAY_PROJECT=<project>]           # build + fetch + harness + replay
make replay-refresh [REPLAY_PROJECT=<project>]   # report corpus drift beyond the pin
make replay-down [REPLAY_PROJECT=<project>]      # remove the project's container
```

The scripts underneath, for scaffolding and finer-grained control:

```sh
./init.sh <project> <repo> <ref> <path>   # scaffold a new project directory
./fetch.sh <project>                      # download the corpus (pinned commit)
./fetch.sh <project> refresh [ref]        # compare the pin against current history
./harness.sh <project> up                 # start postgres, apply the baseline
./harness.sh <project> dsn                # DSN for pg-sprite / any client on the host
./harness.sh <project> psql ...           # psql inside the container
./harness.sh <project> reset              # back to the pristine baseline
./harness.sh <project> down               # remove the container and all state
./replay.sh <project>                     # replay (starts or resets the harness itself)
```

Each project claims its own host port (`PORT` in `project.conf`), so harnesses can
coexist. The container name defaults to `pgsprite-<project>-replay`.

## The manifest

`<project>/assessment.tsv` is a line-range index into the pinned corpus — one row per
replay step, in strict corpus order:

```
<migration-prefix> <start>-<end> <execute|refuse:<reason>|psql>
```

Ranges are coupled to the pin in `project.conf` by design: an assessment must never
silently apply to a corpus it was not written against, so bumping the pin means
re-curating the manifest. The replay exits non-zero on any verdict mismatch and ends
with a per-statement results table plus a bucket summary (executed / typed refusals
by reason / psql-only / mismatches).

## Refreshing a corpus

Projects keep shipping schema changes. `./fetch.sh <project> refresh [ref]` queries
the GitHub API for the migrations directory at `ref` (default: the current default
branch) and reports any files beyond the pin — without touching the corpus, the pin,
or the manifest. Picking up new history is then a deliberate three-step: bump
`COMMIT`, extend `FILES`, and re-curate `assessment.tsv` against the new pin before
replaying.

## What this is not

- Not part of `make test` or CI — the Go suite remains the correctness oracle; this
  is an assessment tool run deliberately.
- Not a second demo — [demo/](../demo/) tours the CLI; this replays someone else's
  history.
- Not a vendored copy of any project — each `corpus/` is gitignored and re-fetched;
  the pin in `project.conf` records exactly which commit the assessment ran against.
