# pg-sprite vision

> **pg-sprite is the go-to engine for PostgreSQL schema changes: every change — from an
> instant `ADD COLUMN` to a full online table rewrite — planned deterministically, executed
> online with provable safety, and invisible to the applications it serves. It is the
> reliable PostgreSQL execution layer for a GitOps front-end like
> [SchemaBot](https://github.com/block/schemabot) — what
> [Spirit](https://github.com/block/spirit) is for MySQL — and a standalone CLI any
> engineer can pick up today, no GitOps layer required.**

One tool, one mental model, for *all* PostgreSQL schema changes. Today teams assemble a
toolchain: one tool to diff and plan the easy DDL, another to copy-and-swap the genuine
rewrites, an app-coordinated pattern for expand/contract, and hand-rolled scripts for
everything else. Each covers one slice. pg-sprite covers the whole surface in one engine —
and adds, deliberately, what no slice of that toolchain ships: data proven identical by
checksum before any destructive step, a durable checkpoint so a mid-change crash resumes
instead of orphaning, and managed-platform failover (Aurora) treated as a modeled state
rather than an unhandled surprise.

**pg-sprite embraces the Unix design philosophy: do one thing, and do it perfectly.** The
one thing is changing the *shape* of live PostgreSQL tables while applications keep
reading and writing them. Everything inside that problem — classification, online idioms,
rewrites, verification, crash-resume — belongs in this engine and is done to the safety
bar below. Everything outside it — data changes, catalog bootstrap, GitOps orchestration,
access control — is deliberately another tool's job: the engine refuses it with a typed
reason, and [capabilities.md](capabilities.md) names the tool class that owns each job.
"One tool for all schema changes" and "do one thing" are the same claim read from two
sides: depth across every table-shape change, never sprawl across object types.

## The five pillars

### 1. All schema changes, one engine

Classify-first ([design-principles.md](design-principles.md)): every requested change is
routed to exactly one of three outcomes — a **native-safe idiom** (`CONCURRENTLY`,
`NOT VALID` + `VALIDATE`, fast defaults) when PostgreSQL can do it online; a
**checksum-gated, log-based copy-and-swap** when it genuinely requires a rewrite; or a
**refusal with a verdict** that names the hazard and the alternative when neither is safe.
No "use this tool for indexes and that tool for rewrites"; no silent fall-through to a
lock-storm `ALTER`. And unlike diff/planning tools that stop at emitting hazard-annotated
statements, pg-sprite owns the execution too: the same classification feeds the engine that
runs the safer sequence or the rewrite, with the locking, verification, and resume
guarantees below.

The one-liner: **pg-sprite's classifier is PostgreSQL's missing `ALGORITHM=` / `LOCK=`
declaration.** MySQL lets authors assert the cost bracket and the concurrency impact and
fails closed if either can't be honored; PostgreSQL silently runs whichever cost applies.
The classifier proves both before execution — and routes to the safest combination that
exists instead of failing. The per-operation mapping is
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md).

### 2. GitOps-ready: the engine under SchemaBot

pg-sprite itself is not the GitOps layer — it does not watch repositories, open pull
requests, or orchestrate fleets. That is SchemaBot's job, already proven for MySQL with
[Spirit](https://github.com/block/spirit) as its execution engine. pg-sprite's job is to
be *reliably drivable* by that layer: deterministic planning (the same SQL input always
yields the same plan, so the hazard-annotated plan a reviewer approves on the PR is the
plan that executes), machine-readable verdicts and progress, idempotent crash-resume, and
no interactive operator judgment required mid-flight. The desired schema stays plain
`CREATE TABLE` SQL in git — no DSL, no migration numbering, no imperative up/down scripts;
SchemaBot handles GitOps orchestration, pull requests, and approvals; pg-sprite diffs,
classifies, routes, and executes
([schemabot-integration.md](schemabot-integration.md)).

**GitOps-ready, not GitOps-required.** The same properties that make pg-sprite drivable by
an orchestrator — deterministic plans, typed verdicts, bounded locks — make it a safe
*direct* tool: an engineer with a DSN and the CLI gets the same engine, without adopting
SchemaBot first. What that buys today is the refusal discipline: every session runs under
a bounded `lock_timeout`, and a change the engine cannot prove safe ends in seconds with a
typed verdict — not a hand-typed `ALTER` in a bare `psql` session holding an
`ACCESS EXCLUSIVE` lock on a hot table. Each capability the phased plan lands
(copy-and-swap, the checksum gate, durable crash-resume) reaches the direct user the
moment it ships, because both front doors are held to one design rule: every capability is
reachable from the CLI, and the CLI consumes the same plan, verdict, and lint contracts an
orchestrator would. Meeting users where they are is part of the point: standalone CLI use
is a supported front door, not a demo mode.

### 3. Developer-friendly, application-invisible

Zero app-coupling to the change mechanism: no `search_path` opt-ins, no deploy-ordering
entanglement with N application teams, no dual-version application code. The table keeps
its identity for the entire schema change; the application cannot tell a change happened. A
developer's whole job is: edit the SQL, open the PR, read the verdict.

### 4. Safety is the product

Speed, breadth, and convenience are all downstream of one non-negotiable: **the engine must
never round uncertainty up to success.** Concretely ([invariants.md](invariants.md)): a
mandatory, non-skippable checksum gate before any destructive step (CO-1); a durable
checkpoint so `SIGKILL` at any phase resumes rather than orphans (ST-1/2); every strong lock
bounded in *total* time, not per-attempt (LK-2) — and when the lock cannot be acquired,
pg-sprite backs off and retries; it never terminates other sessions to clear its own path;
fail-closed mutual exclusion (LK-1); and
metadata fidelity asserted, not assumed (ST-5). Each invariant ships with a test named for
it — the honesty rule is that these claims are design-time until those tests land.

### 5. Built for managed reality

The design treats managed platforms (Aurora/RDS) as first-class targets rather than assuming
an idealized self-hosted primary: replication-slot loss
on failover is a modeled state transition with a checksum-repair path (ST-4), logical
decoding is the default capture mode with triggers as a deliberate fallback
([change-capture-tradeoff.md](change-capture-tradeoff.md)), and the CI matrix proves
PG 14→18 — the fleet floor, not the newest release
([postgresql-version-support.md](postgresql-version-support.md)).

## Where the existing ecosystem stops

The tools in this space fall into two problem classes, and a fair comparison starts by
naming which class a tool is in. **Convergence planners** —
[pg-schema-diff](https://github.com/stripe/pg-schema-diff) and
[pg-delta](https://github.com/supabase/pg-toolbelt/tree/main/packages/pg-delta) — treat a
schema change as an *artifact*: an ordered DDL script that takes catalog state from A to
B across a broad object model (tables, views, functions, enums, and more), executed as
ordinary DDL when the operator chooses. **Online executors** —
[pgroll](https://github.com/xataio/pgroll), [pg-osc](https://github.com/shayonj/pg-osc),
and [pg-sprite](https://github.com/block/pg-sprite) — treat a schema change as a
*production operation*: their value is what
happens to live traffic while the change runs — the locks held, the concurrent writes
captured, the data verified, the cutover window, the recovery story after a crash.

pg-sprite is an online executor. pgroll and pg-osc are peers, not competitors — three
answers to the same problem, each built to different design criteria: pgroll serves
multiple schema versions simultaneously so applications can roll forward and back through
a change; pg-osc reconstructs a single table via trigger-based copy-and-swap; pg-sprite
exists to satisfy the criteria laid out in
[design-principles.md](design-principles.md) — safety over speed, classify-first routing
to native idioms, log-based capture, a mandatory checksum gate before any destructive
step, and application invisibility. The convergence planners solve a different problem
and meet pg-sprite only at its declarative front door — there a broad-model planner and a
deep executor are potential complements, not alternatives.

pg-sprite exists because today the whole surface is covered only by assembling a
toolchain, and each piece stops where the next is needed. Declarative diff-and-plan
tools generate and apply native DDL, but hand a genuine table rewrite to PostgreSQL
as-is. Expand/contract tools execute rewrites online by serving multiple schema
versions — a real capability, bought with the application coupling pg-sprite refuses:
per-change opt-in and a multi-version window. Online repack tools copy-and-swap a
table without changing its definition. Versioned-file frameworks own the workflow
this vision rejects and delegate the execution mechanics entirely.

Each of these is excellent at its slice — [README.md](README.md) credits the specific
tools whose practices this engine deliberately mines. What none of them combine is
classify-first routing, a mandatory checksum gate before any destructive step, durable
crash-resume, and application invisibility in one engine. That combination — not any
single feature — is why pg-sprite is built rather than assembled (the diff-engine
build-vs-adopt decision is recorded in
[low-level-design.md](low-level-design.md#5-declarative-diff-engine-decided)).

## What "go-to" means

pg-sprite has succeeded when:

- a team's *entire* schema-change workflow — through SchemaBot, with pg-sprite executing —
  is "edit SQL, merge PR": for every change type, on every table size, with no
  per-change operator judgment;
- an engineer with nothing but a DSN reaches for the pg-sprite CLI *instead of* a raw
  `psql` session — first for day-to-day changes on teams that haven't adopted a GitOps
  layer, and, once the tool has earned that trust, for the urgent mid-incident change
  where bounded locks, a typed verdict, and crash-resume matter most — and gets the same
  engine and the same guarantees as the fleet;
- the SchemaBot fleet drives PostgreSQL changes through pg-sprite the way it drives MySQL
  changes through [Spirit](https://github.com/block/spirit) — same declarative front-end,
  same orchestration, same safety posture;
- "we verified the data before cutover" is the *expected* baseline for PostgreSQL schema
  changes in the OSS ecosystem, because a shipped tool made it table stakes;
- the safety differentiators this doc set claims — the checksum gate, durable resume,
  bounded locks — read "proven", each backed by a landed test, not "(design)".

## What pg-sprite is not

This section is the do-one-thing philosophy applied — every "not" below is the same
boundary drawn from a different angle. Scope honesty keeps the vision credible:
pg-sprite is not the GitOps layer itself — it does
not watch git, manage pull requests, or schedule fleets; it is the engine that layer
(SchemaBot) drives. It is not an application-rollout coordinator (the multi-version schema
window some expand/contract tools offer is a real capability we deliberately trade away
for application invisibility); not a data-migration/backfill framework; not a MySQL tool
(Spirit exists); and not a bypass for semantics — a `DROP COLUMN` still needs the
applications to stop reading the column first.
pg-sprite makes the *mechanics* of schema change invisible and safe; the *meaning* of a
breaking change remains an engineering decision.
