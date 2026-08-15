# pg-sprite documentation

> **pg-sprite is the go-to engine for PostgreSQL schema changes: every change — from an
> instant `ADD COLUMN` to a full online table rewrite — planned deterministically, executed
> online with provable safety, and invisible to the applications it serves. It is the
> reliable PostgreSQL execution layer for a GitOps front-end like
> [SchemaBot](https://github.com/block/schemabot) — what
> [Spirit](https://github.com/block/spirit) is for MySQL.**

pg-sprite derives and combines the best practices of established tools rather than porting
any single one of them: [Spirit](https://github.com/block/spirit) (the copy-and-swap
lifecycle and operator model), [pg_osc](https://github.com/shayonj/pg-osc) and
[pg_repack](https://github.com/reorg/pg_repack) (the shadow-table and repack mechanics),
[pgroll](https://github.com/xataio/pgroll) (expand/contract execution), and
[pg-schema-diff](https://github.com/stripe/pg-schema-diff) (declarative diffing). On top of
what it mines, the engine adds the combination nothing ships for PostgreSQL today:
multi-threaded chunked copy, **log-based CDC** (logical decoding, not triggers), a
**checksum-gated atomic cutover**, and **checkpoint/resume** — Aurora-aware, not
Aurora-only. Why that combination is the product is [vision.md](vision.md); start there.

## Documents

| Doc | Contents |
| --- | --- |
| [vision.md](vision.md) | The **vision statement** — what pg-sprite is (the reliable execution engine under a GitOps front-end like [SchemaBot](https://github.com/block/schemabot), as [Spirit](https://github.com/block/spirit) is for MySQL) and what it deliberately is not. Five pillars, success criteria, and explicit non-goals. Start here for the why. |
| [architecture.md](architecture.md) | The **one-screen codebase map** — the three layers, the package map with build status, the copy-and-swap lifecycle, and where to read more. Start here for orientation. |
| [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md) | The PostgreSQL equivalent of MySQL's [InnoDB Online DDL Operations](https://dev.mysql.com/doc/refman/8.4/en/innodb-online-ddl-operations.html) reference — lock levels, rewrite/scan behaviour, and concurrent-DML safety per operation. |
| [high-level-design.md](high-level-design.md) | The **high-level design** — the conceptual overview: the problem, the planner → router → executor philosophy, the execution patterns and when each is chosen, and coverage at a glance. No package/interface detail. Start here for the architecture. |
| [low-level-design.md](low-level-design.md) | The **low-level design** — the detailed engineering design: package layout, the `Executor` interface, library choices, copy-and-swap lifecycle internals, the full coverage matrix, table requirements, and the decisions remaining for later execution phases. Read this when designing the interfaces and packages. |
| [design-principles.md](design-principles.md) | The canonical **design principles** that govern the engine — safety over speed, decisions-not-options, classify-first, mandatory checksum gate, log-based CDC, and the PostgreSQL/Aurora-specific rules everything else traces back to. |
| [postgresql-version-support.md](postgresql-version-support.md) | The **PostgreSQL version matrix** — which PG majors pgroll, pg_osc, and pg_repack support, which majors Aurora still ships, the minimum PG version each native idiom needs, and the resulting decision to **pivot on PostgreSQL 14+** (validated 14 → 18). |
| [change-capture-tradeoff.md](change-capture-tradeoff.md) | The canonical **triggers vs logical-decoding** trade-off for copy-and-swap — overhead, failover survival, WAL risk, and whether either lets us drop the checksum/checkpoint (answer: keep the checksum; triggers simplify but don't remove the checkpoint). Any doc proposing logical decoding as the default points here. |
| [invariants.md](invariants.md) | The canonical **invariant registry** — testable runtime MUST-statements (correctness, locking, state/resume, refusals, orchestration), each with its enforcement point and source. Mined from this doc set plus [Spirit](https://github.com/block/spirit)'s stated safety invariants and [SchemaBot](https://github.com/block/schemabot)'s control-plane discipline; the build plan's phases carry per-invariant test obligations. |
| [tcb-model.md](tcb-model.md) | The **TCB model** — the trusted-computing-base partition of the engine: which components are the small trusted core that enforces the invariant registry vs the untrusted periphery, the never-trust-callers rule, domain types that make illegal states unrepresentable, the in-TCB engineering rules (from TigerBeetle TIGER_STYLE, s2n-tls, qmail, bitcoin-core), the verification ladder, and the per-side AI-assisted development policy. |
| [plan-report.md](plan-report.md) | The **plan report contract** — the versioned JSON shape both front doors emit for dry-run plans: fields, closed vocabularies, the fingerprint identity, required consumer behavior for unknown versions/values, and one generated example per source (pinned by test). |
| [limitations.md](limitations.md) | The **current limitations** — schema changes pg-sprite refuses today, why they are unsafe or unsupported, and where an operator must act outside the engine. |
| [lint-report.md](lint-report.md) | The **lint report contract** — the versioned JSON shape `pg-sprite lint` emits for offline CI gating: finding fields (verbatim SQL, line/column), the codes table, severities and exit behavior, the offline-conservatism rules, and how the contract versions relative to the plan report. |
| [suggest-report.md](suggest-report.md) | The **suggest report contract** — the versioned JSON shape `pg-sprite suggest` emits for offline advice: the typed caveat vocabulary (what changes about how you must run a safer form, and what a failed step leaves behind), the typed guidance codes for rewrites the planner cannot construct, and the operation → safer form → caveats table (pinned by test). |
| [engine-role.md](engine-role.md) | The **engine-role provisioning contract** — the tiered minimum access a PostgreSQL user needs to run schema changes against tables it does not own: role membership for owner-gated DDL, schema `CREATE` for index builds and shadow objects, `SET ROLE` for owner-correct shadow creation, replication access for CDC, and the explicit list of powers the engine role must *not* have. Preflight refusals name the missing `GRANT` and point here. |
| [invalid-index-recovery.md](invalid-index-recovery.md) | The **operator runbook** for the one native-path outcome that needs a human — an invalid index the executor found or may have left. What each typed state licenses: when `DROP INDEX CONCURRENTLY` is proven safe, when the entry may be another actor's healthy in-flight build, and what to check when the executor could prove nothing. |
| [testing.md](testing.md) | The **test-suite guide** — how to run the suite (unit, per-major, all supported majors, compose database), current coverage, the remaining executor-phase test obligations, and the vanilla-PostgreSQL-matrix vs real-Aurora validation boundary. |
| [schemabot-integration.md](schemabot-integration.md) | The **single home for orchestrator integration** — how SchemaBot (the reference orchestrator) drives the engine: the pluggable-engine overview, the verb mappings, the concrete adapter contract, and the design constraints (OC-* invariants) the integration imposes on the core. |

## The decided shape

pg-sprite is a **decoupled planner → router → executor** engine — not a port of any one
tool. The planner decides *what* changes, the router decides *which strategy*, and
interchangeable executors (`native`, **copy-and-swap**, **expand/contract**) decide *how*.
The governing philosophy ([design-principles.md](design-principles.md)): *safety over
speed*, *decisions not options* (sensible defaults over config knobs), a *mandatory
checksum correctness gate* before cutover, *dynamic time-based chunking*, and
*checkpoint/resume*.

1. **Classify first — optimistically, then fully.** The
   [classifier](high-level-design.md#two-migration-front-doors-optimistic-vs-classified) is the front
   door, in two forms. **Optimistic classification** (a statement-type gate + table-size
   guard, no schema model) *attempts* the change under a tight `lock_timeout` +
   `statement_timeout`; if it completes it was effectively instant/in-place, and if it
   can't it is cancelled and treated as a rewrite — the analog of Spirit's "attempt
   INSTANT/INPLACE first", adapted to a database with no instant-or-error assertion.
   **Full classification** (parse-based, `pkg/planner`) *predicts* the path up front
   (`CREATE INDEX CONCURRENTLY`, `ADD ... NOT VALID` + `VALIDATE`, fast defaults,
   `ADD PK USING INDEX`, binary-coercible type change), powering dry-run, lint, and the
   declarative diff.
2. **Otherwise copy — refuse honestly until the copy engine lands.** Genuine table
   rewrites (`ALTER COLUMN TYPE` general, volatile-default `ADD COLUMN`, `STORED`
   generated column, repack) get a clear **refusal with the classification and reason** —
   no delegation to external copy tools — until our own **log-based, checksum-gated,
   resumable copy-and-swap** lifts those refusals. PostgreSQL does far more changes as
   native instant operations than MySQL, so refusing the rewrite cases still leaves the
   tool useful for the majority of changes from day one.
3. **Change capture is log-based by default.** A change-capture abstraction with
   **logical decoding** as the primary implementation and a **trigger-based** fallback for
   environments that cannot enable `rds.logical_replication` or can't accept slot loss on
   failover. The default is cluster-dependent, not absolute — see
   [change-capture-tradeoff.md](change-capture-tradeoff.md).
4. **Two front doors, one pipeline.** *Declarative* (`diff`/`fmt`) takes a desired
   `CREATE TABLE` and derives the change by diffing against the live schema; *imperative*
   (`migrate`) takes the DDL as written. Both share the **same** classify → route
   pipeline; declarative mode adds the diff step, imperative mode skips it.

See [high-level-design.md](high-level-design.md) for the conceptual architecture, then
[low-level-design.md](low-level-design.md) for the package/interface detail and the open
decisions.
