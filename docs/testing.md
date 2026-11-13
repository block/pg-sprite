# Testing

How the test suite is organized, what it covers today, and what each build
phase is obligated to add. Core logic is validated against a **real
PostgreSQL** — no mocked-DB tests for core logic (see
[design-principles.md](design-principles.md)).

## The coverage invariant

The suite is part of the safety argument, not a formality. The standing rule,
binding on every merge:

> **No behavior lands without a test that would fail without it. Core logic
> is proven against real PostgreSQL on every supported major. CI runs the
> whole suite — unit, integration, TLS, and the 14 → 18 version matrix — as
> a merge gate on every PR. Coverage only ratchets up.**

Concretely:

- **Same-PR tests.** New behavior and the tests proving it land in the same
  PR — code never merges ahead of its tests, and an invariant from
  [invariants.md](invariants.md) lands with the test named for it (see the
  build-phase mapping there).
- **Regression-first bug fixes.** A bug fix lands with a test that reproduces
  the bug and fails on the pre-fix code.
- **Real database, race-enabled.** Unit tests run with `-race`; core (`pkg/`)
  logic is never validated against mocks — integration tests run against
  real PostgreSQL, across every supported major in CI. The integration
  suite runs under `-race` on the newest major only (PostgreSQL 18): the
  code that runs goroutines does not branch on the server version, so a
  data race CI catches surfaces there whatever version it is named for.
  That race run is split across two jobs, `pkg/schemachange` alone and
  every other package, so the slowest package does not set its length. The
  other majors run the suite without the detector.
- **The matrix is a gate, not advisory.** The `all-green` sentinel job
  requires the full version matrix; docs-only changes are the only path
  that skips it, and even they must pass the unit job — the guards that
  pin prose to code (`internal/safety/proof_types_test.go` for the
  proof-type registries, the `docs_test.go` files for report and CLI
  output examples) run on every PR regardless of what it touches.
- **Coverage never regresses.** Deleting or skipping a test to get green is
  forbidden (same rule as the hooks: no `--no-verify`, no `nolint`). A
  numeric coverage ratchet on `pkg/` packages is not yet wired into CI, so
  this clause remains enforced in review.

## Supabase compatibility

The required Supabase CI job runs the database suites and
[`integration/supabase`](../integration/supabase) against pinned, disposable
Supabase services. The [Supabase guide](supabase.md#what-works-today) maps each
compatibility claim to its tests. The [test reader guide](../integration/supabase/README.md)
explains the cases, shared assertions, and what a passing result establishes.

When a new DDL route affects Supabase tables, extend this suite in the same PR.
Verify the resulting schema and data, access policies and grants, and the app's
API or Realtime behavior where relevant. Refusals must assert a typed reason and
unchanged durable state; execution failures must assert any committed leftovers.
When copy-and-swap can run on a Supabase table, replace the corresponding refusal
expectation with successful execution and preservation checks—do not simply
remove it. Keep the guide's support claims aligned with that evidence.

## Test-methodology invariants (TM)

How tests are *built*, mined from the peer suites (pgroll, pg_repack,
pg-delta — the topology survey below covers *what environments* they test;
this covers *how*). Each rule binds from the phase noted.

### TM-1 — Lifecycle fixture, not happy-path tests

Every executor/schema-change integration test drives the **full lifecycle
through one shared fixture** — start → assert → abort → assert → restart →
complete → assert — so interrupted-and-retried is the default tested path,
not a special case. Where a run checkpoints (the copy-and-swap `Runner`),
kill → resume is part of the lifecycle. *Binds:* Phase 3 (native executor)
onward. *Source:* pgroll `ExecuteTests` (`pkg/migrations/op_common_test.go`).

### TM-2 — Two oracles for safety-encoding SQL

Generated SQL whose exact shape carries a safety property (chunk
continuation predicates, `ON CONFLICT` arbiters, timeout preludes,
trigger bodies if trigger capture is ever built) is **frozen by exact-string test AND proven
behaviorally against a real database** — never just one of the two.
*Binds:* Phase 3 onward. *Source:* pgroll trigger/backfill template tests;
pg-delta's snapshot + roundtrip pairing.

### TM-3 — Fault injection is real, and asserts durable state

Contention tests hold a real `ACCESS EXCLUSIVE` lock from a second
connection; cancellation tests use context deadlines. A statement-budget
overrun is forced with a `ddl_command_end` event trigger that sleeps inside
the statement, never with a table large enough to be slow: a fixed row
count is a bet on how fast the server is. After any injected
failure the test asserts the **durable state** (no wedged schema-change record,
no leaked shadow objects/slots/triggers) and the ability to proceed — not
merely the returned error type. *Binds:* Phase 3 onward; full
phase-boundary kill/resume matrix at Phases 4–8. *Source:* pgroll's
lock-holder pattern — and pg_repack's absence of it, the gap peers left
that our copy-and-swap phases must fill.

### TM-4 — The adversarial schema corpus only grows

Integration fixtures include the shapes that break naive engines: quoted
and whitespace identifiers, dropped-column tuple layouts, TOASTed values,
expression/partial indexes, non-default reloptions, generated and identity
columns, partitioned parents, tablespaces (including quoted names). The
corpus is shared across phases and **never shrinks to make a phase land**.
*Binds:* Phase 1 onward. *Source:* pg_repack `regress/sql/repack-setup.sql`.

### TM-5 — Convergence is the diff oracle

Every declarative-diff test proves, against two real databases: the derived
plan applies cleanly; re-introspect + re-diff yields **empty**; a second
derivation emits nothing (idempotency). Comparison is **semantic catalog
state** (normalized), with SQL snapshots as the secondary oracle; failures
print the residual diff and the original plan. *Binds:* Phase 2 declarative diff onward.
*Source:* pg-delta `tests/integration/roundtrip.ts`.

`testutil.ReadTableCatalog` is the catalog-state side. It reads a table's
columns (attribute options included), owned sequences, constraints, indexes,
and table settings (persistence, access method, tablespace, storage
parameters, replica identity, row security) with direct catalog queries, never through `pkg/schemadiff`, so a property the diff model
ignores on both sides still fails the comparison. A re-diff alone would come
back empty for that property. `TestRunDesiredRoundTrip` in `pkg/migrate` holds
`RunDesired` to it: each case converges a live table to a desired file and
compares the result with the same table built directly from that file.

#### Test harness

`internal/testutil` provides a workload table with identity and unique keys,
numeric and TOAST-able values, and timestamps. Its shape is fixed except for what
`WorkloadShape` varies, such as an `integer` identity key for a test that retypes the
key. Its seeded load generator runs inserts, updates, deletes, and transactional
unique-key swaps while a copy is active, and `Snapshot` reads what it has committed so
far without stopping it, so a test can prove a write landed inside a phase. The
convergence oracle implements TM-5 with a typed symmetric difference computed in a
single read-only `REPEATABLE READ` snapshot, so both directions and both row counts
describe the same instant. It can ignore columns that a test deliberately excludes,
except the primary key, which every comparison projects so rows stay distinct.

A background writer in a test runs each statement detached from its own stop context
under a short bound (`context.WithTimeout(context.WithoutCancel(ctx), …)`), and observes
the stop only between statements. A stop then never cancels a statement mid-flight, so
every write the writer made is decided on the server, committed or failed, before the
stop returns, and a statement held past its bound is a recorded failure rather than a
hang. The load generator in `internal/testutil` and the per-package writers follow this
one convention.

A load spec with `RecordFailures` keeps its workers writing through statements that
fail for a reason other than an expected race, and lists each with its SQLSTATE and
time, so a test measures what a swap does to an application's statements.
`TouchedIDs` lists the key of every row a committed mutation wrote, in commit order
(both keys of a unique-key swap or a key move), and `Progress` reads the counts and the
list's length without copying it. The oracle's `ExcludeKeys` leaves keys out of both
sides and both counts, so a test that marks the touched list at an instant compares
exactly the rows last written before it. The [load proof](#the-load-proof) is built on
these three.

### TM-6 — Every mutation direction per property

For each object property the diff handles: absent → present, present →
changed, present → absent — plus replacement where PostgreSQL cannot ALTER
in place. *Binds:* Phase 2 declarative diff onward. *Source:* pg-delta operation suites.

### TM-7 — Benchmarks carry correctness assertions

Performance tests (copy throughput at multiple row scales; fallback-mode
trigger write amplification) verify post-benchmark data correctness and tag
results with commit SHA + PG version. A fast wrong answer is a failure.
*Binds:* Phase 4 onward. *Source:* pgroll `internal/benchmarks`.

### TM-8 — A compiled-binary e2e path is required in CI

Separate from Go package tests, CI runs the **built `pg-sprite` binary**
against a real database with checked-in example inputs as the acceptance
corpus — exit codes, output, and resulting database state asserted.
CI's `demo` job does this: `make demo-check` runs the demo tour
([demo/tour.sh](../demo/tour.sh)) in check mode against the compose
database, asserting on `--json` fields and exit codes only, never on prose.
The operator-run [corpus replay](../replay/README.md) (`make replay`) drives
the same binary through a real project's schema-change history and is not
a CI gate; so is the [load proof](#the-load-proof) (`make load-proof`), which
drives it through a copy-and-swap under a concurrent write load. *Binds:*
Phase 2 (first executing command). *Source:* pgroll `make examples` CI job; pg_repack driving its CLI through `pg_regress`.

### TM-9 — The operation must outlive the observer

Any test that observes or interrupts an operation **in flight** (progress
polling, kill/resume mid-copy, injected faults between phases) seeds enough
rows that the operation demonstrably spans the observation or injection
point — otherwise the operation can finish before the fault lands and the
test passes without testing anything. Vacuous runs are a failure: the test
asserts the interruption actually hit mid-operation (e.g. the checkpoint
shows partial progress), not just the final state. *Binds:* Phase 1
(budget-cancellation fixtures, which seed enough rows that a rewrite cannot
finish inside its statement budget) onward. *Source:* SchemaBot's in-flight
progress tests, which seed large row counts so an operation spans a poll
interval.

**Beyond the peers:** none of the three does generative testing. From
Phase 3 we add **seeded schema/DDL generation** (generate desired state →
plan → apply → re-diff must be empty), printing the seed on failure and
promoting failing seeds to fixed regression cases. This is deliberately a
capability no peer suite has.

## How to run

| Command | What it does |
| --- | --- |
| `make test-unit` | Race-enabled unit tests, no Docker (`SKIP_INTEGRATION=1`). |
| `make test` | Full suite; integration tests start disposable PostgreSQL containers (testcontainers). `PG_VERSION` selects the major (default 16). |
| `make test-supported-postgres` | Full suite against every supported major, 14 → 18 — the local mirror of the CI matrix, except that it runs `-race` on every major where CI runs it on PostgreSQL 18 only. |
| `make db-up` / `make test-db` / `make db-down` | Long-lived compose database on localhost; the suite connects to it via `PG_DSN` instead of starting per-test containers. Fastest loop for repeated integration runs, and the path CI's version matrix uses — per-test containers oversubscribe a small CI runner and get killed mid-test. Runs under `-race` by default; `make test-db RACE=` drops the detector for a faster loop. `TEST_PKGS` narrows the packages (default `./...`). `make test-db-slow` and `make test-db-rest` are the two halves CI's race job runs on separate runners: `pkg/schemachange` alone, and every other package. |
| `make test-own-server` | The tests that need a server of their own — they create a cluster-level role or a tablespace, or check which image the harness chose — and therefore skip when `PG_DSN` is set. Runs exactly those tests against disposable containers, with `PG_DSN` cleared. CI's version matrix and the release workflow run it right after `make test-db`, so a `PG_DSN` run plus this target is the whole suite. A test opts in through `testutil.SkipOnSharedServer`, and a completeness test holds the target's `-run` list to the set of callers. |
| `make test-aws-boundary` | AWS-boundary tests against Ministack's RDS/Aurora control plane. Needs Docker only; see the tier table below. |
| `make demo-check` | The demo tour in check mode against the built `bin/pg-sprite` and the compose database — CI's artifact smoke test (TM-8). Needs Docker and `jq`. |
| `make check-capabilities` | Regenerates the generated regions of [capabilities.md](capabilities.md) from `pkg/capabilities/capabilities.yaml` and fails if the committed page differs. CI runs it; no Docker. |
| `make check-vulnerabilities` | Scans the module with a pinned `govulncheck` and fails if the code calls into a known vulnerability. CI runs it as the `Vulnerability Scan` workflow on every pull request, on every push to `main`, and daily on `main`; the workflow is not part of `all-green`. Exit 3 means reachable findings; any other failure means the scan itself could not run. No Docker. |
| `make replay` / `make replay-refresh` / `make replay-down` | Corpus replay ([replay/README.md](../replay/README.md)): drive the built binary through a pinned real-project schema-change history, asserting each statement's typed outcome. Operator-run, not a CI gate. |
| `make load-proof LOAD_PROOF_FREE_DISK=…` | The [load proof](#the-load-proof): the built binary carries a copy-and-swap over a large table while a concurrent workload writes it, and writes `results.json`. Operator-run, not a CI gate; skipped unless `LOAD_PROOF=1`, which the target sets. |

The harness is [internal/testutil](../internal/testutil/postgres.go):
`StartPostgres` returns a connection URL (container, or `PG_DSN` when set)
and `NewSchema` gives each test a throwaway schema so parallel tests never
collide — which also means every integration test runs against a
**non-`public` schema**, an axis some peer tools (pgroll) treat as a separate
matrix dimension. `StartPostgresTLS` starts a TLS-only server with a
generated CA for verify-full tests. The harness has its own tests proving
the version selected by `PG_VERSION` is the version actually running, and
that throwaway schemas are isolated.

Event triggers are database-global, and under `make test-db` every package
shares one database, so a test installs one only through
`InstallEventTrigger` (or `RunDuringDDL`, built on it): its cleanup keeps
the trigger's function alive until every transaction that may still hold
the trigger in its event-trigger cache has ended, where an inline drop
would fail another package's in-flight DDL with `cache lookup failed for
function`. A harness test fails the build on an inline
`CREATE EVENT TRIGGER` elsewhere in the suite. A trigger's body runs for
every other test's DDL too, as whatever role issues it, so it finds its
schema's objects by name in the catalogs and never resolves a
schema-qualified name in an expression its guard evaluates; otherwise an
unrelated test fails with `permission denied for schema`.
`InstallEventTrigger` holds each DDL trigger to this when it is installed,
by running a probe statement as a role without access to the schema.
A trigger that injects a fault must also confine it to its own schema's
objects — matching the `schema_name` or `object_identity` that
`pg_event_trigger_ddl_commands()` reports, not statement text alone —
or another package's DDL on the shared database is refused or loses its
connection.

The privilege-ladder tests additionally create throwaway **cluster-level
roles** (`NewRole`), so the role behind an external `PG_DSN` needs
`CREATEROLE` — a step up from "a database you can create schemas in".
The compose database and per-test containers connect as superuser, so
this only matters when pointing `PG_DSN` at a shared server; tests whose
requirements go further (replication attributes, extension ownership) skip
themselves when the server refuses.

### The load proof

`make load-proof LOAD_PROOF_FREE_DISK=<free space>` builds `bin/pg-sprite` and runs
`TestLoadProof` in [integration/loadproof](../integration/loadproof/loadproof_test.go).
It seeds a workload table with `LOAD_PROOF_ROWS` rows (default 1,000,000), starts
`LOAD_PROOF_WORKERS` × `LOAD_PROOF_RATE` mutations a second (default 8 × 100: inserts,
updates and deletes, half the updates rewriting the out-of-line column, and
`LOAD_PROOF_HOT` of the updates aimed at ten keys), and runs
`pg-sprite migrate --copy-and-swap --keep-old --json` with
`ALTER TABLE … ALTER COLUMN id TYPE bigint` while the load writes. The CLI calls the
same `migrate.Run` an importer calls, so the proof covers that entry point with the
options the CLI sets; an importer's own cancellation and resume are not part of it.

A pass proves:

- the binary exits 0 with `executed-via-copy-and-swap`;
- the retained `_old` table and the live table agree row for row over every key the
  load did not write after the mark, the last checkpoint poll before the cutover. The
  mark must fall in the verify pass, after the run inserted, updated and deleted. Writes
  during the drain and the swap are carried but not compared; `rows.excluded` in
  `results.json` counts the keys left out;
- the live table's catalog equals that of a table the statement changed natively;
- every load failure is one the swap causes, when it can cause it: a lock timeout or a
  client deadline between the mark and one statement deadline after the binary exits,
  or a stale prepared statement (`0A000`) after the mark;
- the load inserts again after the swap, so the identity sequence the swapped-in table
  took over is past every key it holds.

A run needs a server with `wal_level=logical`, a role that can create replication
slots and publications, and disk for two full tables (the retained source and the
live one) plus the WAL the slot retains. The workload row is about 10 KiB with its
out-of-line column, so the default size is about 10 GiB a table. `LOAD_PROOF_FREE_DISK`
has no default: it is the operator's statement of the free space on the data volume,
and the preflight refuses the run when twice the table would not fit. A target other
than this machine (`localhost`, a loopback address, or a Unix socket) is refused unless
`LOAD_PROOF_ALLOW_REMOTE=1` says that server is disposable; overriding
`LOAD_PROOF_DSN` also skips starting the compose database. The test refuses to start
when `go test`'s `-timeout` would end it before the binary's own two-hour budget,
because a test the deadline ends skips the cleanup that drops the run's replication
slot; the target passes `3h`. The record, `results.json` in the package directory
(`LOAD_PROOF_RESULTS` moves it), is written whether the run passed or failed: phase
timings, peak slot lag per phase, load counts, failures by SQLSTATE, the slowest
mutation, and the verdict.

Copy-and-swap under the default load is not supported yet, and the default run ends in
`execution-failed`. Uniform writes at 8 × 100 a second keep changing the chunks the live
verify pass rereads, and the pass stops on a chunk that changed on every reread; with
`LOAD_PROOF_HOT=0.2` the applier falls behind during the verify pass until the slot-lag
ceiling stops the run. The defaults stay at this load so the engine fixes are proven by
the same command. A light load passes:
`make load-proof LOAD_PROOF_ROWS=20000 LOAD_PROOF_WORKERS=2 LOAD_PROOF_RATE=5 LOAD_PROOF_FREE_DISK=10GiB`.
A load much lighter than that can commit no delete before the mark, and the run then
fails as one that judged no stream.

## Aurora-shaped environments: three tiers, each proving what it can

CI runs the matrix against **vanilla PostgreSQL 14 → 18 images** — the floor
promised in [postgresql-version-support.md](postgresql-version-support.md) is
enforced by CI, not just documented. Vanilla PostgreSQL is *not* Aurora:
storage internals, replication, and failover behavior differ. The suite is
explicit about which environment proves which claim, and never lets a
cheaper tier stand in for a claim it cannot prove:

| Tier | Environment | Proves | Cannot prove |
| --- | --- | --- | --- |
| Data plane | Real PostgreSQL containers, majors 14 → 18 (the CI matrix) | All core SQL/DDL/catalog behavior | Aurora-only semantics |
| AWS boundary | [Ministack](https://github.com/ministackorg/ministack) (`ProvisionAuroraPostgres` in [internal/testutil](../internal/testutil/ministack.go)) | The RDS/Aurora **control plane**: cluster + instance provisioning through the real RDS API, endpoint discovery, connecting to the discovered endpoint through `pkg/dbconn` | Aurora data-plane behavior — the database behind the endpoint is real vanilla PostgreSQL in a sibling container, not Aurora. The production RDS TLS path (`pkg/dbconn`'s `IsRDSHost` detection and verify-full against the embedded Amazon CA bundle) — no emulator can present a chain that bundle trusts; that path is proven by `pkg/dbconn`'s TLS unit and integration tests instead |
| Real Aurora | Environment-specific gate outside public CI | Aurora-only semantics: `rds.logical_replication`, failover slot loss, storage-level replication, fast DDL | — |

The AWS-boundary tier is **never** a substitute for the data-plane tier:
core logic keeps its no-mocked-DB rule and runs against real PostgreSQL.
Ministack earns its keep only at the seam where the engine talks to AWS —
today the provisioning/discovery flow, and as those features land, Secrets
Manager DSN resolution and reader/writer endpoint selection. To be honest
about what that means right now: **no production pg-sprite code makes an
AWS API call yet**, so today's test proves the harness itself — that the
real RDS API shapes provision a cluster, that discovery returns the
endpoint the test then connects to, and that the emulator serves the
requested PostgreSQL major. It pins the seam in place for the features
that will sit on it; it does not yet exercise shipped code the data-plane
tier misses. One platform caveat: the discovered endpoint is the sibling
database's container-internal address, which CI's Linux host routes to
directly; on macOS, where Docker runs in a VM, the test detects the
unreachable address and falls back to the sibling's host-published port —
so "connects to the discovered endpoint" is proven by CI, not by a macOS
laptop.

Ministack is MIT-licensed and needs no auth token, so the tier runs
anywhere Docker runs — locally and on every CI PR, forks included. Both
tiers take the target major from the same `PG_VERSION` (`PGVersion()` in
`internal/testutil`), and the AWS-boundary test asserts the provisioned
server's major matches — an invariant, not a convention: the two tiers
cannot silently drift onto different majors, so version-specific behavior
has nowhere to hide. The Ministack container mounts the host Docker socket
to start the sibling database container; run it only on Docker hosts you
own (the CI job refuses to run on anything but an ephemeral GitHub-hosted
runner). `MINISTACK_IMAGE` overrides the pinned image for upgrades or
mirrors.

### How much of the suite runs on Ministack

Deliberately almost none: one test whose subtests share one provisioned
cluster ([ministack_integration_test.go](../internal/testutil/ministack_integration_test.go))
— provisioning costs minutes, so the tier provisions once. The harness
enables Ministack's cluster replication (`MINISTACK_RDS_PG_CLUSTER_REPLICATION`),
which adds a second, reader member to the cluster: the reader and stop/start
subtests need a real standby, and the price is a longer provisioning wait
while the emulator brings both members to `available`. Two subtest
orderings are load-bearing, and the test documents them where it fixes the
order: stop/start restarts cluster compute and waits for every member to
return before the metadata failover, which targets the reader member; the
password rotation runs last because it changes the shared cluster's master
password. Each subtest pins one AWS seam:

- **provision & connect** — provision → instance `available` → endpoint
  discovery → `dbconn` connect → PG-major assertion → DDL smoke;
- **control-plane error contract** — unknown identifiers and duplicate
  creations surface as the AWS SDK's typed RDS faults, matched with
  `errors.As` — exactly the match an AWS-facing caller makes. The pinned image
  reports the duplicate-instance fault under the wire code real AWS uses,
  so the test carries no divergence workaround;
- **password rotation** — RDS-managed password generation, rotation, and
  Secrets Manager resolution, plus what a rotation does to a running schema
  change (see below) and pg-sprite's contract that the resulting auth failure
  is terminal, not retryable;
- **reader read-only boundary** — a real streaming-replication standby accepts
  catalog preflight reads, refuses writes with SQLSTATE `25006`, and the
  connection layer classifies that refusal as terminal;
- **stop/start resilience** — stopping real cluster compute interrupts active
  DDL, and the test pins the *cause*, not just the failure: the backend's
  `admin_shutdown` / `crash_shutdown` SQLSTATE or a connection-level error
  carrying no server response — never `query_canceled` from the session's own
  `statement_timeout`, which would pass a test that proved nothing about the
  stop. The interruption is terminal for `dbconn.Retryable`, so the engine's
  outcome is the fail-closed `execution-failed`. Starting compute again
  restores every member, the interrupted DDL has left no trace in the
  catalog, and a fresh schema change succeeds;
- **metadata failover keeps the writer session** — `FailoverDBCluster` flips
  the API-visible writer and reports `failing-over` while an established
  writer transaction remains usable. Ministack does not yet promote the
  standby at the data plane, so the test deliberately makes no such claim.

### What a password rotation does to a running schema change

PostgreSQL authenticates a session at connection time only, so a master
password rotation does **not** break a schema change in flight — the
established sessions keep working, possibly for hours. The failure lands
on the next *dial*: the pool growing past its idle set, a
`MaxConnLifetime` recycle, or a reconnect after a network blip. That
delayed failure carries no causal link to the rotation an operator will
remember, which is exactly why it is pinned by a test: the stale
credentials fail with SQLSTATE `28P01`, and `dbconn.Retryable` classifies
that as terminal, so the engine surfaces one clean failure rather than
retrying against an endpoint that will keep refusing it.

This is the design input for the planned credential-refresh hook: because
the failure is per-dial, the hook must resolve credentials per-dial
(`BeforeConnect`), not capture a password at pool construction. It matters
most where the engine dials twice far apart — an executor that acquires a
fresh connection for a post-failure verdict, hours into an index build,
must not lose a provable verdict to a rotation that happened in between.

Everything else — all parser, planner, executor, and connection
behavior — runs on the data-plane tier against real PostgreSQL. That
split is policy, not accident: Ministack exists only for the seam where
the engine talks to AWS APIs, and its share grows only when AWS-facing
features land, never by moving core-logic tests onto it. Planned growth,
in dependency order:

- reader/writer topology tests — writer-endpoint targeting with a reader
  present, endpoint re-discovery after a global-cluster failover
  (metadata-level: every Ministack endpoint resolves to one shared
  container) — once the engine has endpoint-selection logic to test. Which
  endpoint a schema change targets is a *safety* property, not a
  performance one: DDL against a reader endpoint fails in confusing ways,
  and against the wrong cluster member is worse — so when endpoint
  selection lands it must be visible in the plan report, not just inside
  `dbconn`;
- Secrets Manager DSN resolution, when that feature lands;
- rotation *recovery* — the engine re-resolving credentials and
  reconnecting mid-schema-change — once `pkg/dbconn` grows a
  credential-refresh hook (per-dial, for the reason above).

Logical-replication behavior is a data-plane concern and is tested on
real PostgreSQL, never on this tier.

The harness and its test are behind the `ministack` build tag: a plain
`go test ./...` (and therefore `make test`) never compiles them, so the
default suite needs no Docker-socket mount and the AWS SDK stays out of
ordinary builds. `make test-aws-boundary` is the only way in. In CI the
tier runs as the `aws-boundary` job — a **signal, not a merge gate**: it
is deliberately outside `all-green`'s required set while no production
code makes AWS API calls, because an emulator or infrastructure failure
should not block a merge the tier can say nothing about. It gets promoted
to the required set when the first AWS-facing feature (Secrets Manager
DSN resolution) lands — the day a failure means something an author can
fix. It is also intentionally **not** part of the pre-push hook, which
stays unit-only so pushes remain fast.

## Current coverage

The table names the entry-point test file for each area; sibling `*_test.go` files in the
same package cover the rest of it.

| Area | Tests |
| --- | --- |
| CLI grammar / config | [internal/cli](../internal/cli/cli_test.go) |
| Pool config, bounded session timeouts | [pkg/dbconn](../pkg/dbconn/pool_config_test.go), [integration](../pkg/dbconn/dbconn_integration_test.go) |
| Retry classification and behavior | [pkg/dbconn/retry_test.go](../pkg/dbconn/retry_test.go) |
| RDS/Aurora TLS (unit) | [pkg/dbconn/rds_test.go](../pkg/dbconn/rds_test.go) |
| Verify-full TLS against a live TLS-only server; a CA bundle never falls back to plaintext against a server that accepts both (`TestTLSIntegrationNoPlaintextFallback`, `StartPostgresTLSOrPlaintext`) | [pkg/dbconn/tls_integration_test.go](../pkg/dbconn/tls_integration_test.go), [CA pinning (unit)](../pkg/dbconn/ca_tls_test.go) |
| Targeted blocker termination | [pkg/dbconn/dbconn_integration_test.go](../pkg/dbconn/dbconn_integration_test.go) |
| Test harness self-checks | [internal/testutil](../internal/testutil/postgres_test.go) |
| RDS control-plane provisioning → endpoint discovery → `dbconn` connect, reader read-only behavior, stop/start resilience, metadata failover, error contract, password rotation (Ministack) | [internal/testutil/ministack_integration_test.go](../internal/testutil/ministack_integration_test.go) |
| Parse boundary, typed operations, and advisory rewrites | [pkg/statement](../pkg/statement/statement_test.go), [operation tests](../pkg/statement/ops_test.go) |
| Native / copy-and-swap / refuse classification and safer SQL | [pkg/planner](../pkg/planner/planner_test.go) |
| Backend routing and copy-and-swap unavailable disposition | [pkg/router](../pkg/router/router_test.go) |
| Plan report contract: exact JSON shape, versioning, field omissions | [pkg/plan](../pkg/plan/plan_test.go) |
| Offline lint findings: typed codes, severities, counts, JSON contract; CLI exit contract | [pkg/lint](../pkg/lint/lint_test.go), [CLI lint](../internal/cli/lint_test.go) |
| Script splitting through the grammar (canonical statements, parse failures) | [pkg/statement](../pkg/statement/split_test.go) |
| Scratch execute-and-introspect, ordered diff, and convergence (`TestDiffConverges`) | [pkg/schemadiff](../pkg/schemadiff/schemadiff_integration_test.go), [diff tests](../pkg/schemadiff/diff_test.go) |
| CLI `diff`, `fmt`, and classified `migrate --dry-run`, including applying text output and re-diffing to empty (`TestDiffTextPlanIsExecutableSQL`) | [diff integration](../internal/cli/diff_integration_test.go), [fmt](../internal/cli/diff_test.go), [dry-run integration](../internal/cli/dryrun_integration_test.go) |
| Library front door (`diffplan.Plan`): ordered routed plan, missing-table, no-op, copy-and-swap refusal, never-writes, deterministic fingerprint | [diffplan unit](../pkg/diffplan/diffplan_test.go), [diffplan integration](../pkg/diffplan/diffplan_integration_test.go) |
| Bounded optimistic native attempt and table preflight | [pkg/executor](../pkg/executor/optimistic_integration_test.go), [pkg/preflight](../pkg/preflight/preflight_integration_test.go) |
| Table usage evidence (`ReadTableUsage`, `Judge`): window edges, recent scans and lock holders, PostgreSQL 14/15 unknown, resets never idle, one statistics snapshot per read, `track_counts` turned off by a saved setting, partitioned tables judged across every leaf partition | [pkg/preflight](../pkg/preflight/table_usage_integration_test.go), [coverage](../pkg/preflight/table_usage_coverage_integration_test.go), [partitions](../pkg/preflight/table_usage_partitions_integration_test.go), [verdict (unit)](../pkg/preflight/table_usage_verdict_test.go), [partition aggregation (unit)](../pkg/preflight/table_usage_partitions_test.go) |
| Index usage evidence (`ReadIndexUsage`): each index's own scan count, uniqueness, last scan time on PostgreSQL 16+, a partitioned index summed over its leaf indexes at any depth, `track_counts` turned off by a saved setting, a non-index OID refused | [pkg/preflight](../pkg/preflight/index_usage_integration_test.go), [partitions](../pkg/preflight/index_usage_partitions_integration_test.go), [evidence and detail (unit)](../pkg/migrate/index_usage_evidence_test.go) |
| Safer-sequence runner: autocommit steps, committed-prefix semantics on a mid-sequence failure | [pkg/executor](../pkg/executor/sequence_integration_test.go) |
| Concurrent index build: caller-owned session, invalid-index detection into a typed state, and the proven recovery (`RebuildAbandonedIndex`, `DropAbandonedIndex`) | [pkg/executor](../pkg/executor/recover_integration_test.go) |
| Greenfield `CREATE TABLE` path: shape refusals, claimed-name collisions, post-commit name verification | [pkg/executor](../pkg/executor/create_integration_test.go) |
| Accepted-blocking execution primitive (`ExecuteAcceptedBlocking`) and its eligibility rule | [pkg/executor](../pkg/executor/accepted_blocking_integration_test.go), [pkg/verdict](../pkg/verdict/accepted_blocking_test.go) |
| Imperative front door (`migrate.Run`): gate, resolve, route, execute, verdict; `--force` acknowledgement; refusal reason → class registry | [pkg/migrate](../pkg/migrate/run_integration_test.go), [registry](../pkg/migrate/refusal_registry_test.go) |
| Irreversible-operations registry: every entry refused without its acknowledgement and a usage error from its own check when the acknowledgement names another table, with nothing executed; a restart's leftover left standing; each CLI acknowledgement flag reaching exactly its own field and sentinel | [pkg/migrate](../pkg/migrate/irreversible_registry_integration_test.go), [registry (unit)](../pkg/migrate/irreversible_registry_test.go), [CLI flags](../internal/cli/acknowledgement_flags_test.go) |
| Desired-state execution loop (`migrate.RunDesired`): whole-plan admission, destructive guard, fingerprint pin, per-statement verdicts | [pkg/migrate](../pkg/migrate/desired_integration_test.go) |
| Offline `suggest`: safer-form mapping, typed caveats, and residue checks against a live server | [pkg/suggest](../pkg/suggest/suggest_test.go), [residue](../pkg/suggest/residue_integration_test.go) |
| `pull`: per-table export, refuse-don't-guess shapes, create-only file writes, and the managed-table catalog query | [CLI pull](../internal/cli/pull_integration_test.go), [pkg/schemadiff](../pkg/schemadiff/managed_tables_integration_test.go) |
| Capability matrix: YAML validity, tier rules, rendered page agreement, `capabilities --json` contract | [pkg/capabilities](../pkg/capabilities/capabilities_test.go), [CLI](../internal/cli/capabilities_test.go) |
| Progress snapshots, the observer seam, and `CancelBuild` | [pkg/progress](../pkg/progress/progress_test.go) |
| Copy-and-swap preflight: privilege, shape, and environment refusals | [pkg/preflight](../pkg/preflight/copy_swap_check_integration_test.go) |
| Chunked copy: keyset chunks, the guarded never-overwriting chunk insert, the in-flight ledger, resume from the watermark | [pkg/copier](../pkg/copier/copier_integration_test.go), [chunker](../pkg/copier/chunker_test.go) |
| Logical decoding and the slot lifecycle | [pkg/decode](../pkg/decode/stream_integration_test.go), [slots](../pkg/decode/slot_integration_test.go) |
| Catch-up apply: buffer, flusher, and the stream consumer loop | [pkg/applier](../pkg/applier/catchup_integration_test.go), [flusher](../pkg/applier/flusher_integration_test.go) |
| Checksum gate: divergence policies, repair and recopy, the live pass under the stream | [pkg/checksum](../pkg/checksum/check_integration_test.go), [live](../pkg/checksum/live_integration_test.go) |
| Checkpoint store and orphan-slot reaper | [pkg/checkpoint](../pkg/checkpoint/store_integration_test.go), [reaper](../pkg/checkpoint/reap_integration_test.go) |
| Shadow build, cutover gate and swap, slot-loss reconcile, and the `Runner` end to end, kill/resume included | [shadow](../pkg/schemachange/shadow_integration_test.go), [cutover](../pkg/schemachange/cutover_integration_test.go), [reconcile](../pkg/schemachange/reconcile_integration_test.go), [Runner](../pkg/schemachange/runner_integration_test.go), [live Runner](../pkg/schemachange/runner_live_integration_test.go), [crash](../pkg/schemachange/runner_crash_integration_test.go) |
| Copy-and-swap through `migrate.Run`: write-blocking and live runs, refusals, restart | [pkg/migrate](../pkg/migrate/copy_swap_integration_test.go), [block writes](../pkg/migrate/copy_swap_block_writes_integration_test.go), [live](../pkg/migrate/copy_swap_live_integration_test.go), [restart](../pkg/migrate/copy_swap_restart_integration_test.go) |
| Supabase compatibility: pooler connections through `pkg/dbconn`, and the DDL, RLS, realtime, and failure matrix against pinned Supabase services | [pkg/dbconn](../pkg/dbconn/supabase_integration_test.go), [integration/supabase](../integration/supabase/README.md) |

## Landed and deferred test obligations

Completed obligations are recorded alongside those owed when the corresponding implementation
lands; tests are not written speculatively against unimplemented behavior. The authoritative
per-phase test lists live in the build plan; the invariant registry
([invariants.md](invariants.md)) carries the per-invariant enforcement
points.

| Status | Test obligations (summary) |
| --- | --- |
| Done — Phases 2.1–2.5 | Parse-based operation descriptors and classification, refusal contracts, declarative desired-state → ordered `ALTER` derivation, routing, the versioned plan report, offline lint and suggest, and convergence testing against real PostgreSQL. |
| Done — Phase 3 native executor | Each native idiom (`CONCURRENTLY`, `NOT VALID` + `VALIDATE`, fast default, `USING INDEX`) exercised against all supported majors; bounded lock behavior under contention with the lock-timeout retry; invalid-index detection and the proven recovery; the greenfield create path; the accepted-blocking primitive. |
| Done — copy-and-swap engine | Shadow build, chunked copy, logical decoding and catch-up, the checksum gate under every divergence policy and under the stream, the cutover fidelity gate and swap, checkpoint/resume, slot-loss reconcile, and kill/resume fault injection, each against real PostgreSQL. |
| Remaining — continuous checker | The checker that runs `CheckLive` on a loop lands with its tests. |

## Topology obligations from peer-tool CIs

A survey of the CI setups of pgroll, Reshape, pg-osc, pg_repack,
pg-schema-diff, pg-delta, migra, Atlas, SchemaHero, and Bytebase found no
peer testing physical replicas, poolers as live intermediaries, failover, or
cloud-managed PostgreSQL. The pooler is now covered here: a pooled
connection string is what a hosted platform hands an operator by default, so
`StartPostgresBehindPgBouncer` runs a real PgBouncer in each pool mode —
session (accepted), transaction and statement (both refused with
`ErrNoSessionAffinity`) — as part of the default suite, and returns its URL
only once a login through it has run a statement on the server behind it
([sessionstate_integration_test.go](../pkg/dbconn/sessionstate_integration_test.go),
[table_lock_pooler_integration_test.go](../pkg/dbconn/table_lock_pooler_integration_test.go)).
Replicas, failover, and cloud-managed PostgreSQL remain environment-gate
territory (see above). The patterns worth carrying, tied to the phase whose
implementation makes them meaningful:

| Pattern (peer precedent) | Where it lands here |
| --- | --- |
| TLS-required server, verify-full + untrusted-CA rejection (pg-delta) | **Done** — `StartPostgresTLS` + [tls_integration_test.go](../pkg/dbconn/tls_integration_test.go); `StartPostgresTLSOrPlaintext` proves a CA bundle never falls back to plaintext. |
| Non-`public` schema placement (pgroll matrix dimension) | Structural — every test already runs in a throwaway non-`public` schema; Phase 1 classifier tests must keep qualifying objects. |
| Partitioned tables (pg_repack regression, pg-schema-diff acceptance) | Phases 1–2 — classifier and native-executor cases for partitioned parents/partitions (`DETACH PARTITION CONCURRENTLY` is PG 14+). |
| Tablespaces, including quoted names (pg_repack) | **Done** — the shadow is built in the source's tablespace (`TestBuildShadowKeepsTheSourceTablespace`). |
| `wal_level=logical` server + publication interaction (pg_repack, pg-delta) | **Done** — the compose database runs `wal_level=logical`, and `StartPostgresWithSettings` starts a server of its own with it for the decoding tests; a publication other than the engine's own is a preflight refusal (`copy-and-swap-publication-member`). |
| Pinned minor versions in the matrix (pgroll, SchemaHero) vs floating major tags | Deliberate choice: we track floating major tags (`postgres:14` … `postgres:18`) so CI follows each major's latest minor automatically. Revisit if a minor-specific regression ever matters. |
