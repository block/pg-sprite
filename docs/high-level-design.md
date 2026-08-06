# High-level design: a decoupled schema-migration engine for Aurora PostgreSQL

The conceptual design. It frames the problem, the architecture philosophy, the three layers and
their responsibilities, the execution patterns and when each is chosen, and the coverage at a
glance — **without** package names, interface signatures, or library choices. Those, plus the
full coverage matrix and the remaining later-phase decisions, live in the
**[low-level design](low-level-design.md)**, which is what you read when designing the
interfaces and packages.

Working name: **`pg-sprite`**. It is a **separate, purpose-built PostgreSQL tool**, not "Spirit with
PostgreSQL support" — Spirit stays MySQL-only (too many MySQL-isms to retrofit cleanly). pg-sprite
instead **derives design practices** from several proven tools — Spirit (MySQL), pg-osc,
pg_repack, and pgroll — and adapts them to Aurora PostgreSQL. It builds on the
reasons to build.

## Table of contents

- [The problem in one paragraph](#the-problem-in-one-paragraph)
- [The core idea: one planner, many executors](#the-core-idea-one-planner-many-executors)
- [The three layers](#the-three-layers)
- [Two migration front doors: optimistic vs classified](#two-migration-front-doors-optimistic-vs-classified)
- [Architecture at a glance](#architecture-at-a-glance)
- [The execution patterns (and when each is chosen)](#the-execution-patterns-and-when-each-is-chosen)
- [Two front-ends: declarative and imperative](#two-front-ends-declarative-and-imperative)
- [Advisory mode: suggest the safe rewrite, don't silently run the risky one](#advisory-mode-suggest-the-safe-rewrite-dont-silently-run-the-risky-one)
- [The copy-and-swap path, conceptually](#the-copy-and-swap-path-conceptually)
- [What it covers (and what it deliberately does not)](#what-it-covers-and-what-it-deliberately-does-not)
- [Key design choices](#key-design-choices)
- [Where to go next](#where-to-go-next)

## The problem in one paragraph

A schema change on a large, busy Aurora PostgreSQL table is dangerous for two different reasons:
some changes take an `ACCESS EXCLUSIVE` lock that, behind a long transaction, can stall the
whole application (the lock queue);
and some changes **rewrite the entire table**, which a single `ALTER` cannot do online. The
engine's job is to take the change the user wants and run it **safely** — using the cheap native
PostgreSQL idiom when one exists, and a clear **not native-safe** refusal when one doesn't —
without the user having to know which case they are in. Later phases add an in-house controlled
table-copy path for those refused rewrites.

## The core idea: one planner, many executors

The engine is **not** a single-purpose copy-and-swap tool. It is deliberately split so that
copy-and-swap is just *one* of several interchangeable strategies. The same front-end
understands every change; a routing decision picks the right strategy per change:

> **Decide *what* changes once; decide *how* per migration.** A shared planner classifies every
> operation; a router picks an executor; interchangeable executors carry it out. New strategies
> can be added without touching the planner.

This is the answer to *"why build copy-and-swap when pgroll already wins some cases?"* — we do
not choose one pattern globally. We route each migration to the pattern whose tradeoffs fit.

## The three layers

```diagram
╭───────────────────────────────────────────────────────────--──╮
│ PLANNER   decides WHAT changes                                │
│   parse the change (imperative ALTER or declarative diff),    │
│   introspect the live schema, classify each operation as      │
│   native-safe · needs-rewrite · refuse, lint for safety       │
╰───────────────────────────────┬─────────────────────────────--╯
                                │ a classified Plan
╭───────────────────────────────▼─────────────────────────────--╮
│ ROUTER    decides WHICH strategy                              │
│   given policy + cluster facts (reversibility needed? app     │
│   schema-version aware? logical replication available? table  │
│   shape?) assign each change to an executor — the one place   │
│   migration policy lives                                      │
╰───────────────────────────────┬─────────────────────────────--╯
                                │ per-change strategy
╭───────────────────────────────▼─────────────────────────────--╮
│ EXECUTORS decide HOW (interchangeable)                        │
│   native DDL · later: copy-and-swap / expand-contract · refuse│
╰─────────────────────────────────────────────────────────────--╯
```

- **Planner** — decides *what* must change. It has no idea how any executor works.
- **Router** — decides *which* strategy, from policy and cluster facts. The single home for
  migration policy.
- **Executors** — decide *how*, behind one common contract, so a new strategy slots in without
  reworking the front-end.

The classifier, declarative diff, dry-run, and status reporting are written **once** and shared
by every executor. Linting is part of this shared front-end design but is not built yet.

## Two migration front doors: optimistic vs classified

The current CLI has two migration front doors; the optimistic path is not an implementation of
the planner's classifier:

- **Bounded optimistic migrate path (Phase 1).** No schema model and no
  classification logic — just a cheap **statement-type gate** (`ALTER TABLE` only — the
  statements the instant path can help; index/constraint statements are refused with the
  safe-idiom pointer rather than run as risky literals) and a **table-size guard**, then **attempt the
  change directly** under a tight lock/time budget. If it completes within budget it was
  effectively an instant / in-place change and we are done; if it can't (the lock isn't granted
  quickly, or the work would exceed the budget) we **cancel and treat it as needing a rewrite**.
  This mirrors Spirit's original front door, which simply tried `ALGORITHM=INSTANT` and handled
  the errors (then tried known-safe `INPLACE` options). It ships an end-to-end useful tool with
  almost no parsing logic. This path lives at the `migrate` front door.
- **Classified planning path (Phases 2.1–2.4).** Parse the statement and introspect the live schema to
  **predict the path up front** — native-safe, needs-rewrite, or refuse — without trial
  execution. The planner drives `diff` and `migrate --dry-run`; Phase 3 will make its classified
  route drive execution and remove the wasted/aborted attempts that the optimistic path can incur.

> **PostgreSQL caveat.** Unlike MySQL's `ALGORITHM=INSTANT`, PostgreSQL has **no assertion** that
> forces a change to be instant-or-error — a rewrite attempt acquires `ACCESS EXCLUSIVE` and does
> real work until cancelled, **blocking all reads and writes for the whole budget window**, so a
> bounded attempt is not a free probe. Optimistic classification therefore bounds the attempt
> with a tight `lock_timeout` **and** `statement_timeout`, and **skips the attempt entirely above
> a table-size threshold** (`pg_class.relpages`), classifying the change as a rewrite — **refused
> with the reason** until the in-house copy engine lands (see the build plan).

## Architecture at a glance

```diagram
        user: --alter "..."   OR   --desired schema.sql
                         │
              ╭──────────▼──────────-╮
              │ CLI: migrate · diff ·│
              │ fmt · lint · status  │
              ╰──────────┬──────────-╯
                         ▼
                 ╭───────────────╮      shared front-end:
                 │   PLANNER     │      parse · introspect ·
                 │  (classify)   │      diff · classify · lint
                 ╰───────┬───────╯
                         ▼
                 ╭───────────────╮
                 │    ROUTER     │      policy + cluster facts
                 ╰───────┬───────╯
        ╭────────────────┼────────────────┬───────────────╮
        ▼                ▼                ▼               ▼
   native DDL      copy-and-swap    expand/contract     refuse with
   CONCURRENTLY    (Pattern A,      via pgroll          not-native-safe
   NOT VALID …     later)           (Pattern B, later)  verdict
   fast default
        ╰────────────────┴────────────────╯
                         │ cross-cutting: connection mgmt,
                         │ lock bounding, Aurora-aware throttling
                         ▼
              ╭─────────────────────╮
              │  Aurora PostgreSQL  │  writer (DDL/copy/cutover,
              │   writer + readers  │  logical slot) · readers (lag signal)
              ╰─────────────────────╯
```

The package-level version of this diagram (with the concrete components for each box) is in the
[low-level design](low-level-design.md#proposed-architecture-end-to-end).

## The execution patterns (and when each is chosen)

| Pattern | When the router picks it | Key property | Tradeoff |
| --- | --- | --- | --- |
| **native DDL** | The change has a safe online PostgreSQL idiom (most changes) | Cheapest correct path; no copy | None beyond bounding the brief lock |
| **copy-and-swap** (Pattern A, later) | A genuine table rewrite with **no** native online path (`int→bigint`, repack, volatile-default add) | **Transparent** — same table name, no app changes | Not yet available; needs logical replication for the low-overhead mode |
| **expand/contract** via pgroll (Pattern B, later) | A **breaking** change where instant reversibility / two live schema versions matter | **Reversible** within the rollout window | Requires the **app to be schema-version aware** |
| **refuse** | Not native-safe, unsafe, or unsupported | First-class verdict with the reason, later-phase copy-and-swap note, and a safer native alternative where one exists | n/a — it is the safe outcome |

The crucial point: **reversibility and transparency are properties of the pattern, not features
you toggle.** copy-and-swap is transparent but not reversible-by-design; pgroll is reversible but
requires app coordination. You pick one *per migration*. To stay faithful to "decisions, not
options", the router has a **clear default with a narrow, signposted opt-in** (e.g. auto-route,
with an explicit strategy flag only when reversibility is requested) rather than a bare menu.

## Two front-ends: declarative and imperative

The engine accepts a change two ways, both feeding the **same** planner pipeline:

- **Declarative** — the user supplies the **desired end-state** (a checked-in `CREATE TABLE`
  `.sql` file); the engine **derives** the `ALTER` by diffing desired vs live, then runs it
  through classify → route. Phase 3 adds execution of the classified route.
- **Imperative** — the user supplies the `ALTER` directly. It is the **same** pipeline with the
  diff step skipped.

Declarative did the harder front-end work (introspect + diff + ordering); both front-ends now
share classify → route, with imperative input handing the user's statement to the *same*
classifier. Schemas can therefore live as reviewed, version-controlled files and CI can compute
"what would change". Destructive-diff confirmation and explicit rename intent are planned. The
diff algorithm and its safety rules are detailed in the
[low-level design](low-level-design.md#declarative-mode-desired-state-schema-diff).

## Advisory mode: suggest the safe rewrite, don't silently run the risky one

The classifier doesn't only choose an execution path — it can also act as a **suggestion
engine**. When a submitted statement is risky *as written* but has a safer native form,
the engine's default is to **return the recommendation and stop**, rather than execute the
literal statement:

```diagram
 user: CREATE INDEX idx ON orders (customer_id)
                         │
                         ▼
            ╭────────────────────────────╮
            │ classifier: risky literal? │
            │ safer native idiom exists? │
            ╰─────────────┬──────────────╯
                          │ yes
                          ▼
   ┌──────────────────────────────────────────────────────────-┐
   │  RECOMMENDATION (does NOT execute):                       │
   │   you asked:   CREATE INDEX idx ON orders (customer_id)   │
   │   safer form:  CREATE INDEX CONCURRENTLY idx ON orders …  │
   │   why: a plain CREATE INDEX takes SHARE and blocks writes │
   │        for the whole build; CONCURRENTLY does not.        │
   └──────────────────────────────────────────────────────────-┘
        │ Phase 3: apply recommendation       │ Phase 3: insist on literal
        ▼                                      ▼
   execute the safe idiom                 --force  ⇒  DANGER prompt +
   (classified sequence)                 explicit approval, then run as-is
```

Examples of what it suggests (the same idioms the classifier already knows):

| You submit | It recommends | Why |
| --- | --- | --- |
| `CREATE INDEX …` | `CREATE INDEX CONCURRENTLY …` | plain build holds `SHARE`, blocks writes for the whole build |
| `ALTER TABLE … ADD CONSTRAINT … CHECK/FK` | `ADD … NOT VALID` then `VALIDATE CONSTRAINT` | avoids a full-table validation scan under a strong lock |
| `ALTER TABLE … ADD PRIMARY KEY (…)` | build a unique index `CONCURRENTLY`, then `ADD PRIMARY KEY USING INDEX` | avoids a blocking build inside the `ALTER` |
| `DROP INDEX …` | `DROP INDEX CONCURRENTLY …` | plain drop takes `ACCESS EXCLUSIVE` briefly |

Two principles govern this:

- **Never silently execute the dangerous literal.** If a safer form exists, the engine
  surfaces it rather than running the risky form behind the user's back. This is the
  transparent, review-friendly counterpart to *classify-first* — the user still doesn't need to
  know the idiom (the engine names it), but nothing dangerous runs unannounced. The safer form
  is not a semantic equivalent: it reaches the same end state with different locking,
  transactionality, and failure modes (a failed `CONCURRENTLY` build leaves an `INVALID` index
  that must be detected and rebuilt — see the
  [online DDL reference](postgres-online-ddl-reference.md)), which is why the engine owns
  executing it rather than handing it to the user to run manually.
- **The planned force route is loud and explicit.** Phase 3 adds a `--force`
  (run-as-submitted) flag for the rare case where the operator genuinely wants the literal
  statement. It will be gated behind
  prominent **DANGER / CAUTION** output explaining exactly what will block and for how long, and
  an **explicit confirmation** (typed acknowledgement, not a bare `-y`), and the override is
  logged. Force is an escape hatch, not a convenience.

Today the classifier constructs safer sequences and `diff` / `migrate --dry-run` render them;
default `migrate` still uses the bounded optimistic Phase 1 path. Phase 3 adds substitution and
execution of classifier-produced SQL.

In non-interactive contexts (CI), advisory mode is a natural gate: the engine prints the
recommended rewrites and exits non-zero if a submitted statement would need a riskier path than
the policy allows — see the [low-level design](low-level-design.md#advisory-mode-and-the-force-escape-hatch)
for the surfacing/approval mechanics.

## The copy-and-swap path, conceptually

When a later phase adds the in-house copy-and-swap executor, its lifecycle is:

```diagram
 build a shadow table  ─▶  capture concurrent writes  ─▶  bulk-copy existing rows
 with the new schema       (log-based, off the WAL)        in parallel chunks
                                                                  │
                                                                  ▼
        cut over  ◀──  CHECKSUM GATE  ◀──  drain the captured-change backlog
   (brief ACCESS            (must prove          onto the shadow
    EXCLUSIVE swap,          shadow == source
    bounded + retried)       before cutover)
```

Two things define this path and distinguish it from existing PostgreSQL tools:

- **The checksum is a mandatory gate** — the engine refuses to cut over unless the shadow
  provably equals the source.
- **Cutover timing is controllable** — the swap can be deferred until an operator signal, with a
  continuous re-verification loop while it waits.

The mechanism (logical decoding, chunking, the transactional swap, checkpoint/resume) and the
MySQL→PostgreSQL primitive mapping are in the low-level design and
mysql-vs-postgresql.md.

## What it covers (and what it deliberately does not)

No single tool covers every Aurora PostgreSQL topology, configuration, and schema shape, and
being explicit about the supported matrix is part of the "decisions, not options" philosophy. At
a high level, v1 targets:

- **Topology:** Aurora PostgreSQL provisioned (writer + readers) as the primary target; RDS
  PostgreSQL as a bonus; Serverless v2 with caveats. Not Serverless v1, not Babelfish.
- **Schema shape:** a single table that **has a primary key**, **no foreign keys or triggers on
  it**, **no PK change**, and **no lossy conversion** — intentionally close to Spirit's supported
  surface.
- **Capture:** logical decoding where logical replication is enabled; a trigger-based fallback
  otherwise (which inherits the overheads of pg_osc-style tools).

The full deployment/precondition/schema matrices, the per-constraint *reasons*, and the
Postgres-specific preconditions (logical replication, slot/role privileges, unchanged-TOAST
handling) are in the
[low-level design](low-level-design.md#coverage-and-limitations-does-this-cover-all-of-aurora-postgresql).

## Key design choices

The choices that shape everything else (the full categorized list is in
[design-principles.md](design-principles.md)):

- **Classify-first.** Take the cheapest correct path; reserve copy-and-swap for genuine
  rewrites. Users get the safe PostgreSQL idiom automatically.
- **Safety over speed.** Correctness gates (the mandatory checksum) and bounded locks come
  before throughput.
- **Decisions, not options.** Sensible defaults over knobs; a second pattern is a signposted
  opt-in, not a menu.
- **Log-based capture, not triggers** (where possible) — near-zero source overhead, the key
  differentiator vs pg_osc. The trade is robustness: on Aurora a **writer failover can lose the
  logical slot mid-migration**, so the bulk copy resumes but the CDC catch-up may need a
  checksum-repair reconciliation (not a full re-copy), and the trigger path is the failover-safe
  fallback. The default is cluster-dependent — see the
  [change-capture trade-off](change-capture-tradeoff.md) and
  [low-level-design's failover analysis](low-level-design.md#failover-during-migration-what-survives-and-what-doesnt).
- **Operator mental-model parity with Spirit** — the same lifecycle and verbs, so
  operators carry one mental model across MySQL and PostgreSQL.


## Where to go next

- How an orchestrator drives the engine (verbs, adapter contract, constraints) →
  **[schemabot-integration.md](schemabot-integration.md)**.
- The detailed interfaces, package layout, libraries, lifecycle internals, full coverage matrix,
  and later-phase decisions → **[low-level-design.md](low-level-design.md)**.
- How Spirit (the inspiration) works → spirit-architecture-notes.md.
- The phased plan to build it → build-plan.md.
