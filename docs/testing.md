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
  real PostgreSQL, across every supported major in CI.
- **The matrix is a gate, not advisory.** The `ci-ok` sentinel requires the
  full version matrix; docs-only changes are the only path that skips it.
- **Coverage never regresses.** Deleting or skipping a test to get green is
  forbidden (same rule as the hooks: no `--no-verify`, no `nolint`). A
  numeric coverage ratchet on `pkg/` packages is wired into CI once Phase 1
  lands the first core package — until then this clause is enforced in
  review.

## Test-methodology invariants (TM)

How tests are *built*, mined from the peer suites (pgroll, pg_repack,
pg-delta — the topology survey below covers *what environments* they test;
this covers *how*). Each rule binds from the phase noted.

### TM-1 — Lifecycle fixture, not happy-path tests

Every executor/migration integration test drives the **full lifecycle
through one shared fixture** — start → assert → abort → assert → restart →
complete → assert — so interrupted-and-retried is the default tested path,
not a special case. Once checkpointing exists, kill → resume joins the
lifecycle. *Binds:* Phase 2 (native executor) onward. *Source:* pgroll
`ExecuteTests` (`pkg/migrations/op_common_test.go`).

### TM-2 — Two oracles for safety-encoding SQL

Generated SQL whose exact shape carries a safety property (chunk
continuation predicates, `ON CONFLICT` arbiters, timeout preludes,
fallback-mode trigger bodies) is **frozen by exact-string test AND proven
behaviorally against a real database** — never just one of the two.
*Binds:* Phase 2 onward. *Source:* pgroll trigger/backfill template tests;
pg-delta's snapshot + roundtrip pairing.

### TM-3 — Fault injection is real, and asserts durable state

Contention tests hold a real `ACCESS EXCLUSIVE` lock from a second
connection; cancellation tests use context deadlines. After any injected
failure the test asserts the **durable state** (no wedged migration record,
no leaked shadow objects/slots/triggers) and the ability to proceed — not
merely the returned error type. *Binds:* Phase 2 onward; full
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
print the residual diff and the original plan. *Binds:* Phase 3.
*Source:* pg-delta `tests/integration/roundtrip.ts`.

### TM-6 — Every mutation direction per property

For each object property the diff handles: absent → present, present →
changed, present → absent — plus replacement where PostgreSQL cannot ALTER
in place. *Binds:* Phase 3. *Source:* pg-delta operation suites.

### TM-7 — Benchmarks carry correctness assertions

Performance tests (copy throughput at multiple row scales; fallback-mode
trigger write amplification) verify post-benchmark data correctness and tag
results with commit SHA + PG version. A fast wrong answer is a failure.
*Binds:* Phase 4 onward. *Source:* pgroll `internal/benchmarks`.

### TM-8 — A compiled-binary e2e path exists in CI

Separate from Go package tests, CI runs the **built `pg-sprite` binary**
against a real database with checked-in example inputs as the acceptance
corpus — exit codes, output, and resulting database state asserted.
*Binds:* Phase 2 (first executing command). *Source:* pgroll `make
examples` CI job; pg_repack driving its CLI through `pg_regress`.

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
| `make test-supported-postgres` | Full suite against every supported major, 14 → 18 — the local mirror of the CI matrix. |
| `make db-up` / `make test-db` / `make db-down` | Long-lived compose database on localhost; the suite connects to it via `PG_DSN` instead of starting per-test containers. Fastest loop for repeated integration runs. |

The harness is [internal/testutil](../internal/testutil/postgres.go):
`StartPostgres` returns a connection URL (container, or `PG_DSN` when set)
and `NewSchema` gives each test a throwaway schema so parallel tests never
collide — which also means every integration test runs against a
**non-`public` schema**, an axis some peer tools (pgroll) treat as a separate
matrix dimension. `StartPostgresTLS` starts a TLS-only server with a
generated CA for verify-full tests. The harness has its own tests proving
the version selected by `PG_VERSION` is the version actually running, and
that throwaway schemas are isolated.

## Version matrix vs real Aurora

CI runs the matrix against **vanilla PostgreSQL 14 → 18 images** — the floor
promised in [postgresql-version-support.md](postgresql-version-support.md) is
enforced by CI, not just documented. Vanilla PostgreSQL is *not* Aurora:
storage internals, replication, and failover behavior differ, and some
Aurora-specific behavior (e.g. `rds.logical_replication`, failover slot
loss) cannot be exercised in public CI. Validation against real Aurora
engine versions is a separate, environment-specific gate that lives outside
this repository's CI.

## Current coverage (Phase 0)

| Area | Tests |
| --- | --- |
| CLI grammar / config | [internal/cli](../internal/cli/cli_test.go) |
| Pool config, bounded session timeouts | [pkg/dbconn](../pkg/dbconn/pool_config_test.go), [integration](../pkg/dbconn/dbconn_integration_test.go) |
| Retry classification and behavior | [pkg/dbconn/retry_test.go](../pkg/dbconn/retry_test.go) |
| RDS/Aurora TLS (unit) | [pkg/dbconn/rds_test.go](../pkg/dbconn/rds_test.go) |
| Verify-full TLS against a live TLS-only server | [pkg/dbconn/tls_integration_test.go](../pkg/dbconn/tls_integration_test.go) |
| Targeted blocker termination | [pkg/dbconn/dbconn_integration_test.go](../pkg/dbconn/dbconn_integration_test.go) |
| Test harness self-checks | [internal/testutil](../internal/testutil/postgres_test.go) |

## Deferred test obligations (Phases 1–3)

These are owed when the corresponding implementation lands — they are not
written speculatively against unimplemented behavior. The authoritative
per-phase test lists live in the build plan; the invariant registry
([invariants.md](invariants.md)) carries the per-invariant enforcement
points.

| Phase | Test obligations (summary) |
| --- | --- |
| 1 — statement/classifier | Parse-based classification per DDL form; refusal (`not-native-safe`) contract; every classification decision tested against the reference table in [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md). |
| 2 — native executor | Each native idiom (`CONCURRENTLY`, `NOT VALID` + `VALIDATE`, fast default, `USING INDEX`) exercised against all supported majors; bounded lock behavior under contention; invalid-index cleanup. |
| 3 — declarative diff | Desired-state → `ALTER` derivation correctness; diff idempotency (no-op on converged schema); refusal propagation through the diff path. |

Copy-and-swap (shadow table, CDC, checksum gate, cutover, resume) is
Phases 4–7 and carries its own obligations, including checksum-gate and
checkpoint/resume fault-injection tests.

## Topology obligations from peer-tool CIs

A survey of the CI setups of pgroll, Reshape, pg-osc, pg_repack,
pg-schema-diff, pg-delta, migra, Atlas, SchemaHero, and Bytebase found no
peer testing physical replicas, poolers as live intermediaries, failover, or
cloud-managed PostgreSQL — those remain environment-gate territory (see
above). The patterns worth carrying, tied to the phase whose implementation
makes them meaningful:

| Pattern (peer precedent) | Where it lands here |
| --- | --- |
| TLS-required server, verify-full + untrusted-CA rejection (pg-delta) | **Done** — `StartPostgresTLS` + [tls_integration_test.go](../pkg/dbconn/tls_integration_test.go). |
| Non-`public` schema placement (pgroll matrix dimension) | Structural — every test already runs in a throwaway non-`public` schema; Phase 1 classifier tests must keep qualifying objects. |
| Partitioned tables (pg_repack regression, pg-schema-diff acceptance) | Phases 1–2 — classifier and native-executor cases for partitioned parents/partitions (`DETACH PARTITION CONCURRENTLY` is PG 14+). |
| Tablespaces, including quoted names (pg_repack) | Phases 4–7 — shadow-table placement must preserve tablespace. |
| `wal_level=logical` server + publication interaction (pg_repack, pg-delta) | Phases 4–7 — CDC tests run against logical-decoding-enabled servers; add `wal_level=logical` to the harness/compose when Phase 4 starts. |
| Pinned minor versions in the matrix (pgroll, SchemaHero) vs floating major tags | Deliberate choice: we track floating major tags (`postgres:14` … `postgres:18`) so CI follows each major's latest minor automatically. Revisit if a minor-specific regression ever matters. |
