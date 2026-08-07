# Online schema change engine for Aurora PostgreSQL

Research and design notes for building an online schema-change engine targeting
**Aurora PostgreSQL**, by deriving and combining the best practices from established tools —
[Spirit](https://github.com/block/spirit) (Aurora MySQL),
[pg_osc](https://github.com/shayonj/pg-osc), [pg_repack](https://github.com/reorg/pg_repack),
[pgroll](https://github.com/xataio/pgroll), and
[pg-schema-diff](https://github.com/stripe/pg-schema-diff) (declarative diffing) — rather than
porting any single one of them.

## Table of contents

- [Motivation](#motivation)
- [Documents](#documents)
- [TL;DR recommendation](#tldr-recommendation)

## Motivation

[Spirit](https://github.com/block/spirit) (block/spirit) is an excellent online schema
change tool, but it is **MySQL / Aurora MySQL only** — there is no PostgreSQL support.

On the PostgreSQL side, the existing OSS tools either:

- use the **expand/contract + views** model (pgroll, Reshape) — great, but the application
  must become schema-version aware; or
- use the **shadow-table + swap** model (pg_osc, pg_repack) — but all of them capture
  concurrent writes with **triggers**, which add synchronous write amplification to the
  source table.

**Nobody** ships the full **log-based copy-and-swap** combination for Postgres: multi-threaded chunked copy +
**log-based CDC (logical decoding, not triggers)** + checksum-gated atomic cutover +
checkpoint/resume, tuned for Aurora. That is the gap this engine targets.

## Documents

| Doc | Contents |
| --- | --- |
| [vision.md](vision.md) | The **vision statement** — what pg-sprite is (the reliable execution engine under a GitOps front-end like [SchemaBot](https://github.com/block/schemabot), as [Spirit](https://github.com/block/spirit) is for MySQL) and what it deliberately is not. Five pillars, success criteria, and explicit non-goals. Start here for the why. |
| [architecture.md](architecture.md) | The **one-screen codebase map** — the three layers, the package map with build status, the copy-and-swap lifecycle, and where to read more. Start here for orientation. |
| [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md) | The Aurora PostgreSQL equivalent of MySQL's [InnoDB Online DDL Operations](https://dev.mysql.com/doc/refman/8.4/en/innodb-online-ddl-operations.html) reference — lock levels, rewrite/scan behaviour, and concurrent-DML safety per operation. |
| [high-level-design.md](high-level-design.md) | The **high-level design** — the conceptual overview: the problem, the planner → router → executor philosophy, the execution patterns and when each is chosen, and coverage at a glance. No package/interface detail. Start here for the architecture. |
| [low-level-design.md](low-level-design.md) | The **low-level design** — the detailed engineering design: package layout, the `Executor` interface, library choices, copy-and-swap lifecycle internals, the full coverage matrix, table requirements, and the decisions remaining for later execution phases. Read this when designing the interfaces and packages. |
| [design-principles.md](design-principles.md) | The canonical **design principles** that govern the engine — safety over speed, decisions-not-options, classify-first, mandatory checksum gate, log-based CDC, and the PostgreSQL/Aurora-specific rules everything else traces back to. |
| [postgresql-version-support.md](postgresql-version-support.md) | The **PostgreSQL version matrix** — which PG majors pgroll, pg_osc, and pg_repack support, which majors Aurora still ships, the minimum PG version each native idiom needs, and the resulting decision to **pivot on PostgreSQL 14+** (validated 14 → 18). |
| [change-capture-tradeoff.md](change-capture-tradeoff.md) | The canonical **triggers vs logical-decoding** trade-off for copy-and-swap — overhead, failover survival, WAL risk, and whether either lets us drop the checksum/checkpoint (answer: keep the checksum; triggers simplify but don't remove the checkpoint). Any doc proposing logical decoding as the default points here. |
| [invariants.md](invariants.md) | The canonical **invariant registry** — testable runtime MUST-statements (correctness, locking, state/resume, refusals, orchestration), each with its enforcement point and source. Mined from this doc set plus [Spirit](https://github.com/block/spirit)'s stated safety invariants and [SchemaBot](https://github.com/block/schemabot)'s control-plane discipline; the build plan's phases carry per-invariant test obligations. |
| [tcb-model.md](tcb-model.md) | The **TCB model** — the trusted-computing-base partition of the engine: which components are the small trusted core that enforces the invariant registry vs the untrusted periphery, the never-trust-callers rule, domain types that make illegal states unrepresentable, the in-TCB engineering rules (from TigerBeetle TIGER_STYLE, s2n-tls, qmail, bitcoin-core), the verification ladder, and the per-side AI-assisted development policy. |
| [plan-report.md](plan-report.md) | The **plan report contract** — the versioned JSON shape both front doors emit for dry-run plans: fields, closed vocabularies, the fingerprint identity, required consumer behavior for unknown versions/values, and one generated example per source (pinned by test). |
| [testing.md](testing.md) | The **test-suite guide** — how to run the suite (unit, per-major, all supported majors, compose database), current coverage, the remaining executor-phase test obligations, and the vanilla-PostgreSQL-matrix vs real-Aurora validation boundary. |
| [schemabot-integration.md](schemabot-integration.md) | The **single home for orchestrator integration** — how SchemaBot (the reference orchestrator) drives the engine: the pluggable-engine overview, the verb mappings, the concrete adapter contract, and the design constraints (OC-* invariants) the integration imposes on the core. |

## TL;DR recommendation

Build a Go tool (`pg-sprite`, working name) as a **decoupled planner → router → executor**
engine — **not** a one-to-one Spirit port. The planner decides *what* changes, the router
decides *which strategy*, and interchangeable executors (`native`, **copy-and-swap**,
**expand/contract**) decide *how*. We **derive design philosophies from several tools** —
Spirit (the copy-and-swap lifecycle and operator model), pg_osc (the shadow-table + trigger
fallback shape), and pgroll (the **expand/contract executor**) — rather than copying any one of
them; pg_repack informs the repack path. The
philosophy we adopt: *safety over speed*, *decisions not options* (sensible defaults over
config knobs), a *mandatory checksum correctness gate* before cutover, *dynamic time-based
chunking*, and *checkpoint/resume*.

1. **Classify first — optimistically, then fully.** The
   [classifier](high-level-design.md#two-ways-to-classify-optimistic-vs-full) is the front
   door, and we ship it in two forms. **Optimistic classification** (build first, minimal
   parsing — a statement-type gate + table-size guard, no schema model)
   simply *attempts* the change under a tight `lock_timeout` + `statement_timeout`; if it
   completes it was effectively instant/in-place, and if it can't it is cancelled and treated as
   a rewrite. This is the analog of Spirit's "attempt INSTANT/INPLACE first" — adapted to a
   database with no instant-or-error assertion. **Full classification** (parse-based) is
   implemented in `pkg/planner` and *predicts* the path up front (`CREATE INDEX CONCURRENTLY`,
   `ADD ... NOT VALID` + `VALIDATE`, PG11+ fast default, `ADD PK USING INDEX`, binary-coercible
   type change), powering dry-run, advisory, and the declarative diff.
2. **Otherwise copy — refuse honestly until the engine exists.** For genuine table rewrites
   (`ALTER COLUMN TYPE` general, volatile-default `ADD COLUMN`, `STORED` generated column,
   repack), the **near-term** stance is a clear **refusal with the classification and reason** —
   no delegation to external copy tools. The **longer-term** path is our own **log-based,
   checksum-gated, resumable copy-and-swap** (shadow table + chunked parallel copy + CDC
   catch-up + checksum + atomic transactional cutover) that lifts those refusals. We have the
   runway for this because PostgreSQL does far more changes as native instant operations than
   MySQL, so refusing the rewrite cases still leaves the tool useful for the majority of changes
   from day one.
3. **CDC via a change-capture abstraction** with **logical decoding** as the primary
   implementation (the differentiator vs pg_osc) and a **trigger-based** fallback for
   environments that cannot enable `rds.logical_replication` or can't accept slot loss on
   failover. This default is cluster-dependent, not absolute — see the
   [change-capture trade-off](change-capture-tradeoff.md).
4. **Two front-ends, one pipeline.** *Declarative* (`diff`/`fmt`)
   lets the user submit a desired `CREATE TABLE` and derives the `ALTER` by diffing against the
   live schema (the analog of Spirit's declarative workflow). It and the *imperative*
   (`--alter`) path both exist and share the **same** classify → route pipeline; declarative
   mode adds the diff step, while imperative mode skips it.

See [high-level-design.md](high-level-design.md) for the conceptual architecture, then
[low-level-design.md](low-level-design.md) for the package/interface detail and the open
decisions.
