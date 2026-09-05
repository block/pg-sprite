# Capabilities and support matrix

One page answering the three questions users actually ask: **does pg-sprite support this
change?**, **what is pg-sprite and why does it exist?**, and **what does pg-sprite
deliberately not do?** Every operation and object type lands in exactly one of three
tiers, and every planned tier-2 item corresponds to a real roadmap item — nothing is
vaguely "future work".

This page is the support matrix; the *mechanics* of each current refusal (what lock the
refused form would take, what an operator who accepts a maintenance window can do) live in
[limitations.md](limitations.md).

## Contents

- [What pg-sprite is — and why it exists](#what-pg-sprite-is--and-why-it-exists)
- [The support model: three tiers](#the-support-model-three-tiers)
  - [The engine path](#the-engine-path)
- [The two front doors](#the-two-front-doors)
- [Support matrix](#support-matrix)
  - [Column changes](#column-changes)
  - [Constraints](#constraints)
  - [Indexes](#indexes)
  - [Partitioned tables](#partitioned-tables)
  - [The declarative model (desired files, diff, pull)](#the-declarative-model-desired-files-diff-pull)
  - [Types and non-table objects](#types-and-non-table-objects)
  - [Data and whole-table operations](#data-and-whole-table-operations)
- [Peers share these limits — for different reasons](#peers-share-these-limits--for-different-reasons)
- [Why typed refusal, not passthrough](#why-typed-refusal-not-passthrough)
- [Deliberately operator-owned](#deliberately-operator-owned)

## What pg-sprite is — and why it exists

pg-sprite is an **online schema-change engine** for PostgreSQL: it takes one table-shape
change, classifies it against the live database, and either executes it through the
safest known online pattern under bounded sessions, or refuses with a typed reason. The
measure of the tool is not how many object
types it models but whether a change it accepts can hurt a production workload. The full
positioning is [vision.md](vision.md); how it differs from planners and imperative
copy tools by *problem class* is [architecture.md](architecture.md).

This is the Unix design philosophy applied to schema changes — **do one thing, and do it
perfectly**. The one thing is online table-shape change under concurrent load; this whole
page is the map of where that one thing ends and another tool's job begins.

Two consequences follow, and they explain most of this page:

1. **Tables and their indexes are the model, by design.** Online safety is a
   readers-and-writers problem, and readers and writers touch tables. Objects with no
   concurrent-access problem (extensions, functions, grants) are not in scope — not
   because they are hard, but because there is nothing for an online engine to solve.
2. **A refusal is a feature, not a gap.** `migrate` and its dry-run exit with code 0 only
   when the change is executable through an online-safe path; a refusal exits 2 with a
   typed reason. CI can gate on the exit code alone. That contract is only worth
   something if pg-sprite never executes what it cannot vouch for — see
   [Why typed refusal, not passthrough](#why-typed-refusal-not-passthrough).

## The support model: three tiers

| Tier | Meaning | What you see today |
| --- | --- | --- |
| **T1 — supported today** | The engine executes the change through an online-safe pattern | Execution (or the safer rewritten sequence), exit 0 |
| **T2 — planned** | A known online pattern exists (or requires the copy-and-swap engine); building it is on the roadmap | A **typed refusal** naming the reason, exit 2 — never a silent fallback to a blocking form |
| **T3 — out of scope by design** | No online-safety problem to solve, solving it belongs to a different tool class, or PostgreSQL offers no online mechanism to build on | A typed refusal or a parse-level rejection, with the reason stating *why it is not planned* |

T3 rows carry one of three marks, because they mean different things — and only one of
them is a limitation:

- **⚪ no online-safety problem to solve** — the operation does no table scan and no
  rewrite; at most it takes a brief catalog lock. There is nothing for an *online*
  engine to add; run it through owner tooling or psql. Where that brief lock lands on a
  **live** table (a trigger, a view swap, a greenfield foreign key), the row says so:
  the statement queues behind long-running queries and blocks sessions behind it while
  it waits, so run it under a `lock_timeout`.
- **🔵 a different tool class owns it** — the job is real but belongs to another kind of
  tool (data-change runners, provisioning/IaC, convergence planners, expand/contract
  frameworks). The row's "Online-safety problem?" column names the class to look for.
- **❌ no online mechanism exists** — PostgreSQL itself provides no online pattern to
  build on, so pg-sprite refuses rather than silently run the blocking form. These are
  the only rows where "unsupported" is the honest reading.

Every matrix table carries an **"Online-safety problem?"** column: "Yes" means there is a
readers-and-writers problem for an online engine to solve (pg-sprite solves it, plans to,
or — ❌ — nothing can today); "No" states which tool class users should reach for
instead.

The invariant: **every T2 row is a tracked roadmap item; T3 rows deliberately have
none.** If a refusal message points at a "planned" capability, that plan exists —
otherwise the change is out of scope by design, and this page — not the refusal text,
which today is one undifferentiated unsupported-statement reason for everything outside
the imperative front door — names the tool class that owns the job.

### The engine path

Tier answers *whether* pg-sprite stands behind a change; the **Engine path** column in
each matrix table answers *how* — the route the change takes (or will take) through the
engine:

- **native, as-is** — the statement is already online-safe (metadata-only, or already
  the online idiom); executed directly under bounded sessions. The bound is
  `lock_timeout`/`statement_timeout`, except that a concurrent index build may instead
  be bounded by an explicit caller-owned cancellable context — the executor refuses a
  context that cannot be cancelled, so the build stays bounded either way.
- **native, safer sequence** — the blocking form is substituted with the equivalent
  online sequence before execution; the rewrites are catalogued in
  [safer-sequences.md](safer-sequences.md).
- **native, planned flow** — a native online pattern exists (or a modeling gap is being
  closed); the row is a typed refusal until that flow lands, and each such row is a
  tracked roadmap item.
- **copy-and-swap** — the change is a genuine table rewrite and routes to the
  shadow-copy engine; a typed refusal until that engine lands (see
  [optimistic-attempt.md](optimistic-attempt.md) for how the router proves the rewrite).
- **—** — no engine path: either the job belongs to another tool class (⚪ / 🔵) or
  PostgreSQL offers no online mechanism to build on (❌).

Path and tier are orthogonal on purpose: a 🟡 row's path says what kind of work lifts
the refusal — building a native flow or landing the copy engine — and a ✅ row's path
says whether the engine ran your statement or substituted a safer one.

## The two front doors

Support differs by front door, so the matrix marks the exceptions:

- **Imperative** (`migrate --alter`, `plan`, `lint`, `suggest`): takes one DDL statement,
  classifies it, and executes the online form — rewriting a blocking statement into its
  safer sequence where one exists. This door has the **broadest coverage**.
- **Declarative** (`diff`, `pull`, desired files): compares a desired `CREATE TABLE`
  file against the live table. This door depends on the canonical table *model*, which
  is deliberately narrower: a table the model cannot fully describe gets a typed
  refusal rather than a silently lossy description. Today that means tables that are
  partitioned (or are partitions), participate in classic table inheritance, own or are referenced by foreign keys, are unlogged,
  carry explicit collations, or take defaults from sequences they do not own.

An operation can therefore be T1 imperatively and T2 declaratively — foreign keys are
the canonical example.

## Support matrix

**53 operations: 17 supported today, 20 planned behind a typed refusal, 14 out of scope
by design, and 2 with no online mechanism in PostgreSQL to build on.**

Status legend: ✅ T1 (supported today) · 🟡 T2 (planned; typed refusal today) ·
⚪ T3 (out of scope; **no online-safety problem** — run directly, or through whatever
review the object warrants) ·
🔵 T3 (out of scope; **a different tool class owns it**) ·
❌ T3 (out of scope; **no online mechanism exists** in PostgreSQL).

### Column changes

| Operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| `ADD COLUMN` (no default, or constant default) | ✅ | native, as-is | Yes | Metadata-only / fast default (PG 11+); executes instantly under bounded locks |
| `ADD COLUMN` with volatile default (`now()`, `gen_random_uuid()`, …) | 🟡 | copy-and-swap | Yes | Table rewrite; routes to copy-and-swap and is refused until that engine lands |
| `ADD COLUMN ... GENERATED ... STORED` | 🟡 | copy-and-swap | Yes | Table rewrite; copy-and-swap route. The copy engine must **recompute, never copy,** generated columns on the shadow table |
| `ADD COLUMN` with inline `UNIQUE`/`PRIMARY KEY`/`REFERENCES`/`CHECK` | 🟡 | native, planned flow | Yes | The inline constraint does its index build or validation scan under the `ADD COLUMN`'s `ACCESS EXCLUSIVE` lock; refused with guidance to add the column first, then build the constraint online |
| `DROP COLUMN` | ✅ | native, as-is | Yes | Metadata-only; flagged **destructive** in the plan report |
| `ALTER COLUMN TYPE`, binary-coercible (proven against live column facts) | ✅ | native, as-is | Yes | Catalog relabel, e.g. `varchar(50)` → `varchar(100)`, `varchar` → `text`; PostgreSQL itself refuses the change when a view, rule, or `STORED` generated column depends on the column — see [binary-coercible-type-changes.md](binary-coercible-type-changes.md#no-rewrite-is-not-no-cost) |
| `ALTER COLUMN TYPE`, general (or with `USING`) | 🟡 | copy-and-swap | Yes | Table rewrite; copy-and-swap route, refused today |
| `SET DEFAULT` / `DROP DEFAULT` / `DROP NOT NULL` | ✅ | native, as-is | Yes | Metadata-only |
| `SET NOT NULL` | ✅ | native, safer sequence | Yes | Executed as the native four-step pattern: `ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID` → online `VALIDATE` → `SET NOT NULL` (catalog flip, PG 12+) → drop the scaffold check |
| `RENAME COLUMN` / `RENAME TABLE` | ✅ | native, as-is | Yes | Metadata-only for PostgreSQL but **app-breaking** across deployed instances; executed with a typed reason so lint/plan consumers can steer away |
| `SET TABLESPACE` | 🟡 | copy-and-swap | Yes | Physical relocation is a rewrite; copy-and-swap route |

### Constraints

| Operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| `ADD PRIMARY KEY` / `ADD UNIQUE` (plain key columns) | ✅ | native, safer sequence | Yes | Rewritten to the online sequence: `CREATE UNIQUE INDEX CONCURRENTLY` → `ADD CONSTRAINT ... USING INDEX` |
| `ADD CHECK` / `ADD FOREIGN KEY` (imperative) | ✅ | native, safer sequence | Yes | Rewritten to the online sequence: `ADD CONSTRAINT ... NOT VALID` (brief metadata lock) → `VALIDATE CONSTRAINT` (writes keep flowing during the scan) |
| `ADD CONSTRAINT ... NOT VALID` / `... USING INDEX` / `VALIDATE CONSTRAINT` | ✅ | native, as-is | Yes | Already the online idiom; executed as-is |
| `ADD FOREIGN KEY ... NOT VALID` on a **partitioned parent** | 🟡 | native, planned flow | Yes | PostgreSQL supports this only from version 18; refused on 14–17 |
| `EXCLUDE` constraints (and unrecognized constraint forms) | ❌ | — | Yes — unsolvable today | No online pattern exists in PostgreSQL — the build scans under `ACCESS EXCLUSIVE` with no `NOT VALID`/`USING INDEX` equivalent. Refused; revisit only if PostgreSQL grows one |
| `DROP CONSTRAINT` | ✅ | native, as-is | Yes | Metadata-only; flagged **destructive** |

### Indexes

| Operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| `CREATE [UNIQUE] INDEX` on a plain table — including partial, expression, covering (`INCLUDE`), GIN/GiST/BRIN | ✅ | native, safer sequence | Yes | Executed as (or rewritten to) `CREATE INDEX CONCURRENTLY`, with validity verification and typed invalid-index outcomes ([runbook](invalid-index-recovery.md)) |
| `DROP INDEX` | ✅ | native, safer sequence | Yes | Rewritten to `DROP INDEX CONCURRENTLY`; flagged **destructive** |
| `REINDEX` | ✅ | native, safer sequence | Yes | Rewritten to `REINDEX ... CONCURRENTLY` |
| Index build on a **partitioned parent** | 🟡 | native, planned flow | Yes | PostgreSQL has no parent-level `CONCURRENTLY`; the blocking form is refused by policy (`--force` does not bypass it). The partition-aware flow — `CREATE INDEX ON ONLY` → per-partition CIC → `ATTACH PARTITION`, with crash-resume per leaf — is planned |
| `ADD CONSTRAINT ... USING INDEX` on a partitioned parent | ❌ | — | Yes — unsolvable today | PostgreSQL does not support adopting an index on a partitioned parent in any supported version; refused before execution |

### Partitioned tables

| Operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| `CREATE TABLE ... PARTITION OF` | 🟡 | native, planned flow | Yes | Typed refusal at both doors: the imperative door does not take `CREATE TABLE`, and the declarative create path refuses the form at plan time and re-checks it at apply — attaching a partition takes a brief `ACCESS EXCLUSIVE` on the **parent**, which the greenfield absence proof does not cover. The partition-aware flow is planned |
| `ATTACH PARTITION` | ✅ | native, as-is | Yes | Executed; the safer idiom (pre-prove the bound with a validated `CHECK` so the attach skips its scan) is surfaced as guidance. A classify-first flow that constructs the proof itself is planned |
| `DETACH PARTITION [CONCURRENTLY]` | ✅ | native, safer sequence | Yes | `CONCURRENTLY` is the idiom; the blocking form is rewritten to it |
| Partitioned parents in the **declarative model** | 🟡 | native, planned flow | Yes | Typed refusal: the model does not yet carry partition keys, and rendering a partitioned parent as a plain `CREATE TABLE` would be silently wrong |
| Partitioned tables in **copy-and-swap** | 🟡 | copy-and-swap | Yes | Root-vs-leaf publication semantics and per-partition swap; sequenced after the copy engine core |

### The declarative model (desired files, diff, pull)

| Table shape | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| Plain tables + their indexes | ✅ | native, as-is | Yes | `diff`, `pull`, and desired-file rendering round-trip the canonical model |
| Classic table inheritance (`INHERITS`) | 🟡 | native, planned flow | Yes | Typed refusal for both parents and children: the model cannot express inheritance edges, and flattening inherited columns would produce a silently lossy baseline |
| Tables that own **or are referenced by** foreign keys | 🟡 | native, planned flow | Yes | Typed refusal on both sides — an incoming FK cannot be expressed in the table's own desired file, and a lossy description would be worse than none. Declarative FK support (composite keys as the primary case, two-phase `NOT VALID` → `VALIDATE` execution) is planned |
| Unlogged tables | 🟡 | native, planned flow | Yes | Typed refusal: persistence is not modeled, converging it (`SET LOGGED`) is a full rewrite, and rendering the table as plain `CREATE TABLE` would silently change crash-safety |
| Explicit column collations | 🟡 | native, planned flow | Yes | Typed refusal: dropping a `COLLATE` clause from a rendered baseline silently changes sort order and index semantics; a collation delta cannot converge without a rewrite |
| Columns whose default uses a sequence the column does not own | 🟡 | native, planned flow | Yes | Typed refusal: in a desired-state model that sequence exists only inside the scratch transaction, so no derived plan can reference it. Column-owned (`serial`-style) sequences are fine |
| Greenfield `CREATE TABLE` apply (the table does not exist yet — a fresh database or a new table in a live one) | ✅ | native, as-is | Yes — a `REFERENCES` clause would take a brief `SHARE ROW EXCLUSIVE` on each **referenced** live table, but desired files refuse foreign keys today, so no live table is locked | Desired-state execution creates the table: `CheckTableAbsent` verifies the table relation and composite-type name are free, the executor verifies every relation name the desired file states (explicit index names and first-choice constraint-index and column-sequence names) is free in the schema, and `CheckCreatePrivileges` verifies the role can create there. It then runs the `CREATE TABLE` and index builds as brief bounded steps under the engine's budgets. An occupied claimed name is a typed `create-collision` refusal before execution — drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column. Duplicate-name SQLSTATEs backstop races for explicit names; for server-chosen names, the probe narrows the race to the time-of-check window, but nothing catches a name taken inside it. `PARTITION OF`, `INHERITS`, `LIKE`, `OF`, `IF NOT EXISTS`, and in-set duplicate names refuse at plan time and are re-checked at apply, while `REFERENCES` and `CONCURRENTLY` are refused upstream at desired-file parse and re-checked at admission as defense in depth |

### Types and non-table objects

| Object / operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| Enum-typed columns on plain tables | 🟡 | native, planned flow | Yes | Tolerance end to end (introspection already canonicalizes via `format_type`; desired-file admission and scratch-database mechanics are being verified) |
| `ALTER TYPE ... ADD VALUE` | 🟡 | native, planned flow | Yes | Metadata-only and online-safe (PG 14+ allows it in a transaction; the value is usable after commit) — planned as an owned operation. No peer online executor owns it |
| Enum value rename / removal | 🟡 | copy-and-swap | Yes | PostgreSQL has no `DROP VALUE`; this is a type swap + table rewrite — routes to a typed refusal toward copy-and-swap |
| Enum/domain type creation and drop | ⚪ | — | No — owner tooling (psql, shipped with the code change) | Bootstrap/catalog work with no concurrent-access problem; owner tooling applies it in the same change that ships the code |
| Views, materialized views (create and replace) | ⚪ | — | No — owner tooling | Transactional catalog work, but `CREATE OR REPLACE VIEW` takes a brief `ACCESS EXCLUSIVE` on the view and queues behind in-flight readers — run it under a `lock_timeout` |
| `REFRESH MATERIALIZED VIEW` | 🔵 | — | No — data jobs / owner tooling | A data operation, not catalog work: the plain form holds `ACCESS EXCLUSIVE` on the matview for the whole rebuild (`CONCURRENTLY` needs a unique index and trades the lock for churn). Scheduling refreshes belongs to data jobs |
| PL/pgSQL function bodies (`CREATE OR REPLACE FUNCTION`) | ⚪ | — | No — owner tooling | Transactional catalog work that takes no lock on any relation; nothing for an online engine to add. No peer online executor owns it either |
| Triggers (`CREATE TRIGGER`) | ⚪ | — | No — owner tooling | Catalog work — no scan, no rewrite — but it takes a brief `SHARE ROW EXCLUSIVE` on the table, queues behind long-running queries, and blocks writers while it waits — run it under a `lock_timeout` |
| Extensions (`CREATE EXTENSION`) | ⚪ | — | No — owner tooling | Same: catalog bootstrap, owner tooling |
| Grants, roles, row-level-security policies | 🔵 | — | No — provisioning / IaC | Access control, not table shape; belongs to provisioning (see [engine-role.md](engine-role.md) for what the *engine's own* role needs) |
| Standalone sequences | ⚪ | — | No — owner tooling | Transactional catalog work on an object with no readers-and-writers problem |
| Publications, subscriptions | 🔵 | — | No — replication provisioning / IaC | Replication provisioning, not table shape (`ALTER PUBLICATION ... ADD TABLE` also takes `SHARE UPDATE EXCLUSIVE` on the table) |

### Data and whole-table operations

| Operation | Status | Engine path | Online-safety problem? | Behavior and why |
| --- | --- | --- | --- | --- |
| `DROP TABLE` | ⚪ | — | No — owner tooling, through a reviewed process | Discards the table and its data in one brief `ACCESS EXCLUSIVE` step: there is nothing online for an engine to make safer, only an irreversible decision an operator must own. Both front doors refuse it — the imperative door as an unsupported statement kind, and the declarative diff is single-table scoped, so a live table with no desired file is not in its view. pg-sprite never plans or executes it; accounting for undeclared tables is the whole-schema owner's job, described under [Deliberately operator-owned](#deliberately-operator-owned) |
| Data backfills, `UPDATE`/`DELETE` batches, DML of any kind | 🔵 | — | No — data-change runners, application batch jobs | pg-sprite changes table *shape*, never table *contents*. Versioned-script runners and application jobs own data changes |
| Column-transform expressions during a copy-and-swap rewrite | 🟡 | copy-and-swap | Yes | The one principled exception: when a rewrite is already copying every row, deriving a new column's value by expression is part of the shape change, not a data job. Planned as part of the copy engine |
| Online table rebuild with no shape change (bloat reclamation) | 🟡 | copy-and-swap | Yes | A copy-and-swap with an identical target shape — the pg_repack use case with checksum-gated cutover and crash-resume. Planned once the copy engine lands |
| Whole-schema convergence (apply a directory of desired files, dependency-ordered) | 🔵 | — | No — convergence planners (pg-schema-diff, pgschema, pgdelta) | Convergence planning across objects is a planner's job; pg-sprite stays the execution engine for the table-shape subset |
| Versioned schema-change-file workflow (Flyway-style ordered scripts) | 🔵 | — | No — versioned-script runners (Flyway-style) | Declarative-only by design; see [vision.md](vision.md) |
| Expand/contract dual-schema versions (pgroll/reshape style) | 🔵 | — | No — pgroll/reshape own this model | Rejected: application invisibility is a core invariant; see [vision.md](vision.md) |

## Peers share these limits — for different reasons

Every tool in this space draws a line around what it models. What differs is *why* the
line sits where it does, and what happens when you cross it:

- **Imperative online executors** (pg-osc, pg_repack; gh-ost and
  [Spirit](https://github.com/block/spirit) in MySQL) never create enums, extensions, or
  triggers because the question never arises: they execute exactly the `ALTER` handed to
  them. The scope limit is implicit and undocumented — you discover it when raw SQL
  errors out mid-change.
- **[pgroll](https://github.com/xataio/pgroll)** models a JSON operation vocabulary, and
  everything outside it goes through a **raw-SQL passthrough**: executed verbatim, with
  none of pgroll's safety machinery applied. That is *passthrough, not support* — the
  tool runs what it cannot analyze.
- **Declarative planners** ([pg-schema-diff](https://github.com/stripe/pg-schema-diff),
  [pgschema](https://github.com/pgschema/pgschema),
  [pgdelta](https://github.com/pgdelta-dev/pgdelta)) model far more breadth — enums,
  views, functions — because their product is a *DDL artifact*, not an execution. Breadth
  is cheap when you don't own what happens under concurrent load.

pg-sprite's position: model narrowly, execute what the model covers with provable online
safety, and make every boundary a **typed refusal**, with this page stating its tier —
planned (with a real plan) or out of scope (with the tool class that owns the job). The
scope limit is explicit, documented on this page, and machine-checkable via exit codes.

## Why typed refusal, not passthrough

The obvious middle ground — refuse, then offer `--apply-anyway` — is deliberately not
implemented. The trade was weighed, not overlooked:

**What passthrough would buy.** One pipeline and one audit trail for every change; no
side-channel psql sessions; lower adoption friction when users hit a boundary. pgroll
demonstrates the demand is real.

**Why it loses.** The exit-code contract — *0 means this ran through an online-safe
path* — is the product. A passthrough mode means pg-sprite executed something it cannot
vouch for, and every incident that follows lands on the tool's reputation, not the
flag. It also breaks the ownership model that crash recovery depends on: the executor
can reason about an interrupted change (see
[invalid-index-recovery.md](invalid-index-recovery.md)) only because it knows exactly
what it runs; arbitrary SQL has no resume semantics. And refusals are the forcing
function that gets real capabilities built — a passthrough is where demand signals go to
die.

**What you get instead.** Every refusal names the classification, the reason, and —
where one exists — the exact safer sequence or the statement an operator can run
deliberately, outside the engine, in a maintenance window. The operator stays in
control; the engine stays honest. `--force` never bypasses a policy refusal.

A constrained variant is **planned**: an explicit, dedicated flag (distinct from
`--force`) that executes an otherwise-refused change through the engine's own bounded
`lock_timeout` sessions ("unsafe DDL under a bounded lock budget", which raw psql does
not give you), with the refusal analysis still printed before execution and the verdict
unmistakably marked as executed without an online-safety guarantee. The plain success
contract stays reserved for online-safe paths, and refusals for unrecognized SQL are
never eligible — only changes the engine understands but cannot run *safely*.

## Deliberately operator-owned

Three related jobs stay with humans on purpose:

- **Dropping a table nobody declares any more.** The declarative diff is single-table
  scoped and whole-schema convergence is a planner's job (see the 🔵 row in the matrix), so
  pg-sprite has no view of *which* tables a schema should contain — only of each table's
  shape. Under a declarative model the only convergence for a live table with no desired
  file is `DROP TABLE`, and pg-sprite never plans or executes one. Whoever owns the whole
  schema — an orchestrator, or an operator running `diff` per file — therefore owns the
  set of tables: decide how far the declared files are authoritative, enumerate the live
  tables, and treat each undeclared one as a destructive divergence to resolve rather than
  as up to date. Declare it (`pull` or `export` writes the desired file) or drop it through
  a reviewed process; whether the owner blocks on it or quarantines it first (`ALTER TABLE …
  SET SCHEMA` / `RENAME TO`) is the owner's policy, not pg-sprite's. A schema being
  onboarded is entirely undeclared tables, so declare the existing tables first (`pull`
  writes one desired file per table), or scope the owner's authority to the schemas it
  manages. The enumeration is a catalog query the owner runs itself — the listing `pull`
  baselines a schema from, with `INHERITS` children also excluded because export refuses
  them anyway. The exclusions matter, because every false positive blocks a table nobody
  touched:

  ```sql
  SELECT c.relname
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'app'
    AND c.relkind IN ('r', 'p')                      -- ordinary and partitioned tables only
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i
                    WHERE i.inhrelid = c.oid)         -- partitions and INHERITS children belong to their parent
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
                    WHERE d.classid = 'pg_catalog.pg_class'::regclass
                      AND d.objid = c.oid
                      AND d.deptype = 'e')            -- extension-owned tables have no file to write
  ORDER BY c.relname;
  ```

  Qualify the catalog with `pg_catalog.` so a user-first `search_path` cannot shadow it
  into an empty — passing — result. Views, materialized views, foreign tables, and
  sequences are outside the model and are not undeclared tables. The planner's scratch
  objects live in a schema of their own (`pgsprite_scratch_<random>`) inside a transaction
  that is always rolled back, so a listing scoped to the owner's schema never sees them.
- **Invalid-index recovery.** A failed `CREATE INDEX CONCURRENTLY` leaves an invalid
  index; an in-flight healthy build looks identical. The executor proves what it can and
  **never drops an index itself** — PostgreSQL drops by name, not identity, so an
  automatic drop could destroy another actor's build. The typed three-state ownership
  model and what each state licenses is [invalid-index-recovery.md](invalid-index-recovery.md).
- **Index maintenance (`REINDEX` automation, bloat-driven rebuild scheduling).** The
  engine executes `REINDEX CONCURRENTLY` when asked (see matrix); *deciding* when an
  index needs rebuilding is monitoring-and-operations territory. Automating it is
  considered but on hold for the same ownership reasons as above.

---

**Keeping this page honest:** any change that adds, lifts, or re-tiers a refusal must
update this matrix in the same PR, and each release's notes link here. A support
question this page cannot answer is a bug in this page.
