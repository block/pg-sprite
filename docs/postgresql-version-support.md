# PostgreSQL version support across the OSS tools (and the version we pivot on)

**We intend to support `pg-sprite` starting from PostgreSQL / Aurora PostgreSQL 14**, and to
validate it against the Aurora-supported matrix 14 → 18. PostgreSQL 14 is the engine's minimum
target; we do not intend to support PostgreSQL 13 or earlier. The rest of this doc is the
evidence behind that floor.

Before fixing that **minimum PostgreSQL version**, it helps to see what the existing OSS
online-DDL tools actually support, what Aurora PostgreSQL itself still ships, and which
PostgreSQL release each *native idiom* we rely on first appeared in. This doc collects all
three and derives the version floor.

All figures are sourced from each tool's own README / docs / CI matrix and from the
[Aurora PostgreSQL release calendar](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraPostgreSQLReleaseNotes/aurorapostgresql-release-calendar.html)
as of **June 2026**; they drift, so re-check before relying on an exact bound.

## Table of contents

- [Tool support matrix](#tool-support-matrix)
- [Aurora PostgreSQL supported majors](#aurora-postgresql-supported-majors)
- [Native-idiom version floors](#native-idiom-version-floors)
- [The version we pivot on](#the-version-we-pivot-on)

## Tool support matrix

| Tool | Pattern | Min PG | Max PG | Form factor | Notes |
| --- | --- | ---: | ---: | --- | --- |
| **pgroll** ([xataio](https://github.com/xataio/pgroll)) | B — expand/contract via versioned views | **14.0** | no stated cap (CI/benchmarks through **18**) | Go single binary; no server extension | PG 14 caveat: versioned views can't use `security_invoker = true` (added PG 15), so **RLS tables are unsafe on PG 14** |
| **pg-osc** ([shayonj](https://github.com/shayonj/pg-osc)) | A — trigger + shadow-table + swap | **9.6** | no stated cap (smoke-tested **9.6, 13.6**) | Ruby gem / Docker | Needs `TRIGGER` priv or `SUPERUSER`; requires a PK; low recent activity (latest v0.9.10, Oct 2024) |
| **pg_repack** ([reorg](https://github.com/reorg/pg_repack)) | A — repack-scoped copy + swap | **9.5** | **19** | C **server extension** (must be installed on the instance) | Client binary version **must match** the in-DB extension version; RDS/Aurora-approved extension; requires PK or UNIQUE-NOT-NULL |

The headline: **the trigger-based tools reach back to 9.x; pgroll is the one that draws a
hard `>= 14` line** — and 14 is precisely the floor that also unlocks the native idioms below
and matches Aurora's supported majors.

## Aurora PostgreSQL supported majors

What Aurora actually runs bounds what we must support. As of June 2026:

| Aurora PG major | Standard-support status |
| ---: | --- |
| 18 | ✅ Supported (GA June 2026) |
| 17 | ✅ Supported |
| 16 | ✅ Supported |
| 15 | ✅ Supported |
| 14 | ✅ Supported (end of standard support **Feb 2027**) |
| 13 | ❌ End of standard support **28 Feb 2026** (RDS Extended Support only) |
| 12, 11 | ❌ End of standard support passed (Extended Support only) |

So the **realistic Aurora target window is 14 → 18**. Building for anything below 14 means
building for engines AWS no longer offers under standard support.

## Native-idiom version floors

The whole point of [classify-first](design-principles.md#classify-first-leverage-native-postgresql)
is to run the safe native sequence (see
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md)). Each of those
idioms has a minimum PostgreSQL version:

| Native idiom (classify-first path) | Available since | Used for |
| --- | ---: | --- |
| Fast default — `ADD COLUMN ... DEFAULT <constant>` without a rewrite | **PG 11** | cheap `ADD COLUMN` with a constant default |
| `ADD CONSTRAINT ... NOT VALID` then `VALIDATE` (incl. the `SET NOT NULL` via validated `CHECK` trick) | **PG 12** | online `SET NOT NULL`, FK, CHECK |
| `REINDEX INDEX CONCURRENTLY` | **PG 12** | online index rebuild / repack-lite |
| `CREATE INDEX CONCURRENTLY`, `ADD ... USING INDEX`, `... NOT VALID` (FK/CHECK) | PG 9.x–11 | concurrent index build, online PK/UNIQUE/FK |
| `DETACH PARTITION CONCURRENTLY` | **PG 14** | online partition detach |
| `security_invoker` views (only needed by the **pgroll** expand/contract backend) | **PG 15** | RLS-safe versioned views |

Every native idiom the engine depends on is available **at or below PG 14**, except
`security_invoker` views — and those are only relevant to the optional pgroll backend, where
pgroll itself already documents the PG 14 RLS limitation.

## The version we pivot on

**Target a minimum of PostgreSQL / Aurora PostgreSQL 14, and validate against the
Aurora-supported matrix 14 → 18 (currently centring on 16/17 LTS).**

Why 14 is the right floor:

- **It matches the most restrictive reusable backend.** pgroll (our expand/contract executor)
  hard-requires `>= 14`; picking any lower floor would mean the pgroll path is unavailable on
  part of our supported range — an inconsistent engine.
- **It matches Aurora reality.** 14 is the oldest Aurora major still under standard support;
  13 and below are EOL/Extended-Support-only. We should not engineer for engines AWS is
  sunsetting.
- **Every classify-first native idiom is present.** Fast default (PG 11), `NOT VALID` +
  `VALIDATE` and `REINDEX CONCURRENTLY` (PG 12), and `DETACH PARTITION CONCURRENTLY` (PG 14)
  are all available, so the native executor has its full toolkit at the floor.
- **Logical decoding is mature — and 14 is specifically where it stops hurting.** The log-based
  CDC differentiator (logical decoding via `rds.logical_replication`) is well-established by 14,
  so the copy-and-swap path's primary CDC source is solid across the whole target range. Just as
  important, **PostgreSQL 14 added streaming of in-progress transactions** (`pgoutput`
  protocol v2): before 14, a large transaction is buffered/spilled to disk and only emitted to
  the consumer *at commit*, which adds apply lag and inflates slot/WAL retention for exactly the
  big batch writes a long migration runs alongside. PG 14 streams those changes incrementally,
  and PG 16 adds *parallel apply* (protocol v4). The serialized, commit-ordered apply stream is a
  real throughput ceiling for CDC catch-up (see
  risks-and-mitigations); 14+ is where the worst
  of that is mitigated, which is an independent reason the floor is 14 and not 12/13.

What this *excludes* and why it's fine:

- **PG ≤ 13.** Below standard support on Aurora; the trigger-based tools (pg-osc, pg_repack)
  still cover those engines if a one-off change is ever needed there, so we lose nothing by
  not targeting them ourselves.
- **`security_invoker`-dependent RLS on PG 14.** Only affects the optional pgroll backend on
  exactly PG 14; the native and copy-and-swap executors are unaffected, and PG 15+ removes the
  limitation entirely.

See why-build-this-engine.md for why we reuse these tools as
executors rather than replace them, and build-plan.md for how the
version floor feeds the phased build.
