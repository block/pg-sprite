# Testing

How the test suite is organized, what it covers today, and what each build
phase is obligated to add. Core logic is validated against a **real
PostgreSQL** — no mocked-DB tests for core logic (see
[design-principles.md](design-principles.md)).

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
