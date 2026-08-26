# The optimistic attempt and the table-size guard

**When the planner cannot prove a schema change is instant, pg-sprite asks the server —
under budgets that bound the cost of being wrong.** This page is the deep treatment of
that design: the full entry-to-exit map every statement walks, which routes are
size-checked and which are exempt, why a bounded "gamble" is the right design on
PostgreSQL specifically, what other schema-change tools do instead, and how the guard's
role evolves once the copy-and-swap engine lands. The two front doors themselves are
introduced in
[high-level-design.md](high-level-design.md#two-migration-front-doors-optimistic-vs-classified);
what a mid-sequence failure leaves behind is
[execution-model.md](execution-model.md).

## Table of contents

- [TL;DR: the escalation ladder](#tldr-the-escalation-ladder)
- [The full map: entry to exit for every change type](#the-full-map-entry-to-exit-for-every-change-type)
  - [The walk, gate by gate](#the-walk-gate-by-gate)
  - [Exit inventory](#exit-inventory)
- [The size guard knob](#the-size-guard-knob)
- [Proof-based vs budget-based routes](#proof-based-vs-budget-based-routes)
  - [Who gets size-checked](#who-gets-size-checked)
  - [The budget mechanics](#the-budget-mechanics)
- [Q&A](#qa)
  - [Aren't all native-safe changes online-safe by design, without a limit?](#arent-all-native-safe-changes-online-safe-by-design-without-a-limit)
  - [Why gamble at all? Do pg_osc and the other online tools do this?](#why-gamble-at-all-do-pg_osc-and-the-other-online-tools-do-this)
  - [Is the gamble temporary until copy-and-swap exists?](#is-the-gamble-temporary-until-copy-and-swap-exists)
  - [Why does a wrong guess cost differently on small vs large tables?](#why-does-a-wrong-guess-cost-differently-on-small-vs-large-tables)
- [Timeline: the four bounded-attempt scenarios](#timeline-the-four-bounded-attempt-scenarios)
- [What other tools do](#what-other-tools-do)
- [The invariants at a glance](#the-invariants-at-a-glance)

## TL;DR: the escalation ladder

pg-sprite executes a PostgreSQL schema change through an escalation ladder:

1. **Proof:** the planner classifies the statement. Shapes it can *prove* are
   metadata-only, or that it can substitute with a known online idiom (e.g.
   `CREATE INDEX CONCURRENTLY`, [safer-sequences.md](safer-sequences.md)), run without
   any size check — their online-safety comes from the proof, not from a bet.
2. **Bounded attempt:** shapes the planner cannot prove are *attempted* under two
   tight budgets — `lock_timeout` and `statement_timeout` — set with `SET LOCAL`
   inside the attempt's transaction. If the change was really instant, it succeeds in
   milliseconds. If it turns out to do real work (a table rewrite), the server cancels
   it, PostgreSQL's transactional DDL rolls it back cleanly, and a typed budget error
   surfaces. Nothing executed, no debris.
3. **The table-size guard** protects rung 2 only: above `Options.MaxTableSizeBytes`,
   the bounded attempt is refused *before any DDL runs* with a typed
   `*preflight.SizeError`, because on a big table even a losing gamble costs a full
   budget of hard downtime under `ACCESS EXCLUSIVE`
   ([why the cost scales with size](#why-does-a-wrong-guess-cost-differently-on-small-vs-large-tables)).

The ladder is **permanent design, not a stopgap**: when the copy-and-swap engine lands
(Phase 5), it adds a rung below the attempt rather than replacing it, and the same size
refusal becomes a routing signal into the copy engine instead of a dead end
([details](#is-the-gamble-temporary-until-copy-and-swap-exists)).

## The full map: entry to exit for every change type

Every DDL statement enters through one gate and leaves through a typed exit. Four
lanes cover every change type: **A** — planner-proven online work (what MySQL would
call in-place with long work), **B** — the bounded attempt, whose success means the
change was effectively *instant* (catalog-only), **C** — proven table rewrites, which
route to copy-and-swap (refused today, executed once the Phase 5 engine lands), and
**F** — the operator's forced override, which rejoins lane B. The walkthrough below
narrates each gate; the [exit inventory](#exit-inventory) catalogs the endings.

```diagram
                              DDL statement submitted
                (imperative migrate / declarative desired-state)
                                        │
                                        ▼
                      parse: exactly one statement, or error
                                        │
                                        ▼
                      ┌─────────────────────────────────┐
                      │ planner + router: classify the  │
                      │ shape, pick a route             │
                      └────┬───────────┬───────────┬────┘
       F. FORCED OVERRIDE  │           │           │
       (operator ack) ────▶│ runs the  │           │
       submitted form as   │ bounded   │           │
       one blind attempt — │ attempt,  │           │
       joins lane B,       │ lane B    │           │
       still size-guarded  │           │           │
              ┌────────────┘           │           └────────────┐
              ▼                        ▼                        ▼
┌──────────────────────────┐ ┌─────────────────────┐ ┌──────────────────────────┐
│ A. PROVEN ONLINE IDIOM   │ │ B. UNPROVEN RESIDUE │ │ C. PROVEN REWRITE or     │
│ or SUBSTITUTED SEQUENCE  │ │ metadata-only or    │ │ UNSUPPORTED SHAPE        │
│ ("in-place, long work":  │ │ rewrite? PostgreSQL │ │ (volatile default, TYPE  │
│ CREATE INDEX → CIC,      │ │ cannot say without  │ │ w/ USING, enum removal,  │
│ planner-safer sequence)  │ │ asking the server   │ │ SET TABLESPACE, …)       │
└────────────┬─────────────┘ └──────────┬──────────┘ └────────────┬─────────────┘
             │                          │                         │
             ▼                          ▼                         ▼
   size guard: EXEMPT          size guard: table          typed plan refusal —
   (NoSizeLimit — long         over the limit?            "routes to copy-and-swap,
   work on large tables            │        │             not implemented"
   is the purpose)              no │        │ yes                 │
             │                     │        ▼                     ▼
             ▼                     │   EXIT 4: SizeError    EXIT 5: typed
   partition + tier gates          │   refused BEFORE       refusal (terminal
   (typed refusal on fail)         │   any DDL — no         today)
             │                     │   lock taken                 ┆
             ▼                     ▼                      ┌ ─ ─ ─ ─▼─ ─ ─ ─ ─ ┐
   sequence executor:      partition + tier gates           copy-and-swap
   each brief step under   (typed refusal on fail)        │ engine — Phase 5,  │
   its budgets; the long           │                        planned: shadow
   build under one                 ▼                      │ table + backfill + │
   overall deadline        BOUNDED ATTEMPT                  checksum + cutover
        │                  SET LOCAL lock_timeout         └ ─ ─ ─ ─┬─ ─ ─ ─ ─ ┘
        │                  + statement_timeout                     ▼
        ▼                          │                       future EXIT 6:
   EXIT 1: success          ┌──────┼───────────┐           online rewrite
   (online build            │      │           │           succeeds
   completed) — or          ▼      ▼           ▼
   typed executor       55P03   completes   57014
   refusal (invalid-    lock    in budget   statement budget:
   index leftovers,     budget      │       real work (rewrite);
   cancelled, …)           │        │       cancelled + rolled
                           ▼        ▼       back cleanly
                    EXIT 3a:    EXIT 2:         │
                    refusal     success —       ▼
                    budget-     it really   EXIT 3b: refusal
                    lock-       was         budget-statement-
                    exceeded    metadata-   exceeded (terminal
                    (contended  only        today; future: the
                    table;      ("instant") routing signal into
                    nothing                 the copy engine)
                    executed)
```

### The walk, gate by gate

What happens to one statement, in order:

1. **Entry.** Two front doors, one pipeline
   ([high-level-design.md](high-level-design.md#two-front-ends-declarative-and-imperative)):
   the imperative `migrate` door takes the DDL as written; the declarative
   desired-state door derives statements by diffing and feeds each one through the
   same gates below. An orchestrator embedding the library (see
   [schemabot-integration.md](schemabot-integration.md)) enters the same way. Nothing
   below differs between the doors — except lane F, which the declarative door
   rejects outright (`ErrForceNotSupported`).
2. **Parse gate.** The statement must parse (libpg_query) and be *exactly one*
   statement — the executor accepts only a parsed `statement.Statement`, so
   multi-statement smuggling is impossible by construction. Failure ends here; the
   database was never touched.
3. **The fork.** The planner and router classify the *shape* — statement kind,
   clauses, column types — and pick a lane. No DDL has run yet:
   - **Lane A** for shapes with a proven online form: a known idiom
     (`CREATE INDEX` → `CREATE INDEX CONCURRENTLY`) or a planner-substituted safer
     sequence ([safer-sequences.md](safer-sequences.md)).
   - **Lane B** for the unproven residue: PostgreSQL gives no upfront answer to
     "instant or rewrite?", so the only way to find out is a bounded attempt.
   - **Lane C** for shapes *proven* to rewrite the table (volatile default, `TYPE`
     with `USING`, …) or not yet supported ([capabilities.md](capabilities.md)): the
     walk ends at plan time with a typed refusal — today's terminal exit, tomorrow's
     on-ramp to copy-and-swap.
   - **Lane F**, the operator's force acknowledgement (`Options.Force`, the CLI's
     `--force`), overrides the routing but not the guards: it re-enters lane B as one
     blind bounded attempt, audited and recorded in the verdict's `Forced` field.
4. **Size guard — lanes B and F only.** `preflight.CheckTable` measures the table's
   full on-disk footprint (heap + indexes + TOAST, all partitions) against
   `Options.MaxTableSizeBytes` ([the knob](#the-size-guard-knob)). Over it, the walk
   ends at Exit 4: a typed `*preflight.SizeError`, before any DDL and before any
   lock. Lane A passes `preflight.NoSizeLimit` — long work on large tables is its
   purpose ([who gets size-checked](#who-gets-size-checked)).
5. **Partition and tier gates — every executing lane.** Partition handling is checked
   against the server version, and the connected role is checked at the privilege
   tier the routed steps actually need ([engine-role.md](engine-role.md)) — a role
   that would die mid-change is refused upfront, with the exact provisioning `GRANT`,
   instead of failing halfway through.
6. **Execution.**
   - **Lane A → sequence executor.** Each brief step runs under budgets; the long
     build (the index build itself) runs under one overall deadline with
     `lock_timeout` disabled, because snapshot waits are healthy there
     ([execution-model.md](execution-model.md)). Ends at Exit 1 — or a typed executor
     refusal (invalid-index leftover, external cancellation —
     [invalid-index-recovery.md](invalid-index-recovery.md)).
   - **Lane B → bounded attempt.** `SET LOCAL lock_timeout` + `statement_timeout`,
     then run the submitted form once ([the budget mechanics](#the-budget-mechanics)).
     Three endings: it really was catalog-only and commits in milliseconds (Exit 2);
     the lock never arrived on a contended table (Exit 3a, nothing executed); or the
     statement did real work — a rewrite — and the server cancelled it, transactional
     DDL rolling it back cleanly (Exit 3b).
7. **Every ending is typed.** Success verdicts, SQLSTATE-mapped budget refusals,
   `SizeError`, plan refusals — automation branches on codes and error types, never
   prose. The full catalog is next.

Mapping to MySQL vocabulary, since the question is often asked in those terms:
MySQL's **`INSTANT`** (metadata-only) is lane B *succeeding within budget* — the
difference is that MySQL tells you upfront and PostgreSQL only tells you by trying;
**`INPLACE` with long work** (e.g. online index build) is lane A; **`COPY`** (table
rewrite) is lane C. The whole reason lane B exists is that PostgreSQL has no upfront
assertion separating "instant" from "copy"
([why gamble at all](#why-gamble-at-all-do-pg_osc-and-the-other-online-tools-do-this)).

### Exit inventory

| Exit | Outcome | Typed as | DDL executed? |
|---|---|---|---|
| 1 | Online idiom / substituted sequence completed | Success verdict | Yes — online by proof |
| 2 | Bounded attempt completed within budget | Success verdict | Yes — it was catalog-only |
| 3a | Lock not granted within `lock_timeout` | `budget-lock-exceeded` (SQLSTATE `55P03`) | No — nothing executed |
| 3b | Statement ran past `statement_timeout` | `budget-statement-exceeded` (SQLSTATE `57014`) | No — rolled back cleanly |
| 4 | Table exceeds the size limit | `*preflight.SizeError` | No — refused before any DDL, no lock taken |
| 5 | Shape routes to an unimplemented strategy, or is refused by a plan/partition/tier gate | Typed plan refusal | No |
| 6 *(future)* | Copy-and-swap rewrite completes online | Success verdict (Phase 5) | Yes — shadow table + cutover |

## The size guard knob

The limit is policy, not capability — one number, set by the embedding caller:

- **Knob:** `Options.MaxTableSizeBytes` (`pkg/migrate`); the CLI exposes it as
  `--max-table-size`.
- **Default:** 1 GiB (`DefaultOptions`) — a default to tune, not a recommendation.
- **What it measures:** the table's full on-disk footprint — heap, indexes, and
  TOAST, across all partitions (`preflight.CheckTable`, `pkg/preflight`).
- **Validation:** the value must be positive; a zero or negative value is rejected at
  the front door, before any database work.
- **Enforcement point:** before any DDL statement is issued. Above the limit the
  caller gets a typed `*preflight.SizeError` and no lock was ever taken.

The refusal means pg-sprite cannot *prove* the change is instant at that size — not
that the change is unsafe. On a table you operate deliberately, raising the limit is
the sanctioned way to converge a bounded-attempt change
([limitations.md](limitations.md)).

## Proof-based vs budget-based routes

### Who gets size-checked

The size guard applies to exactly the executions whose safety is a bounded bet rather
than a proof — `sizeGuardApplies` in `pkg/migrate`:

| Execution shape | Size-guarded? | Why |
|---|---|---|
| Planner-proven online idiom (e.g. `CREATE INDEX CONCURRENTLY`) | No — passes `preflight.NoSizeLimit` | Long work on large tables is its *purpose*; every brief step is still budget-bounded |
| Substituted safer sequence (planner replaced the submitted form) | No — passes `preflight.NoSizeLimit` | Same: the sequence is the proof |
| Blind bounded attempt of the submitted form | **Yes** | Safety comes from the budget, and the budget's worst case scales with table size |
| **Forced** run (operator acknowledged override) | **Yes** | A forced run bypasses shape admission but not the size guard — it is still one blind bounded attempt |

`NoSizeLimit` is "a size limit no PostgreSQL relation can exceed" (`math.MaxInt64`);
it and `SizeError` live in `pkg/preflight`.

### The budget mechanics

The bounded attempt (`pkg/executor`'s optimistic front door) runs under two timers,
applied with `SET LOCAL` inside the attempt's transaction so session defaults cannot
weaken them:

- **`lock_timeout`** — how long we will wait to *acquire* the `ACCESS EXCLUSIVE`
  lock. Overrun ⇒ SQLSTATE `55P03`, typed as "the table is too contended for a blind
  attempt right now". Nothing was executed.
- **`statement_timeout`** — how long the DDL may *run while holding* the lock.
  Overrun ⇒ SQLSTATE `57014`, typed as "the change is doing real work — a rewrite —
  not an in-place catalog change". Rolled back cleanly.

PostgreSQL errors are matched by SQLSTATE only, never message text. The outcomes have
stable codes — `budget-lock-exceeded` and `budget-statement-exceeded` (`pkg/executor`)
— and a budget error is **a refusal input, not an operational failure**: the caller
turns it into a not-native-safe verdict. `DefaultOptions` budgets the brief attempt at
3 s of lock wait and 30 s of statement runtime.

## Q&A

### Aren't all native-safe changes online-safe by design, without a limit?

Only the *proven* ones. pg-sprite has two distinct sources of online-safety:

- **Proof-based routes** — the planner classified the statement as metadata-only, or
  substituted a known online idiom. Their safety comes from the plan; table size is
  irrelevant, so they pass `NoSizeLimit` and are never size-checked.
- **Budget-based route** — the shape could not be proven, so the engine *attempts* it
  under tight budgets and lets the server tell it whether the change was instant.
  That safety is a bounded gamble, and the gamble's downside grows with table size.

So the terse version: **online-safety of proven routes comes from the plan; the size
limit protects the one path whose safety comes from a bounded gamble instead of a
proof.** Above the limit, the budget-based path is not even attempted — the typed
`SizeError` is returned before any DDL runs.

### Why gamble at all? Do pg_osc and the other online tools do this?

The gamble exists because of a PostgreSQL-specific gap, and it is sane because of a
PostgreSQL-specific strength:

- **The gap: PostgreSQL has no `ALGORITHM=INSTANT` equivalent.** On MySQL you can
  assert `ALTER TABLE ... ALGORITHM=INSTANT` and the server *refuses upfront, for
  free*, if it cannot comply — which is why Spirit's instant-first strategy is not a
  gamble. PostgreSQL offers no such assertion: the same `ALTER` syntax is instant or
  a full rewrite depending on the clause, the column type, and the server version.
  The classifier proves many shapes; the residue can only be resolved by asking the
  server — i.e. attempting it.
- **The strength: transactional DDL.** A cancelled PostgreSQL DDL statement rolls
  back completely — no half-built table, no orphaned metadata, no debris. The failed
  probe is genuinely free (modulo the blocked-traffic window,
  [below](#why-does-a-wrong-guess-cost-differently-on-small-vs-large-tables)). MySQL
  DDL is not transactional in this way, which is one reason MySQL tooling prefers
  upfront refusal over attempt-and-rollback.

Other tools answer the same uncertainty differently
([what other tools do](#what-other-tools-do)): the copy-based tools always pay the
maximum cost (full copy-and-swap even for a metadata-only change); pgroll
restructures the workflow itself. pg-sprite's answer — pay a cheap bounded probe
first — is only available *because* PostgreSQL makes the probe cheap to lose.

### Is the gamble temporary until copy-and-swap exists?

No — it's permanent. The optimistic attempt and copy-and-swap solve different
problems, and the attempt stays the cheapest first rung even after the copy engine
exists.

**Why the attempt survives:** a large class of PostgreSQL changes really are
metadata-only (add nullable column, add column with non-volatile default on PG11+,
type widenings the classifier can't prove for every version/edge). Forcing those
through copy-and-swap means paying a full table rewrite — hours on a big table — for
a change that would have completed in milliseconds under a `lock_timeout`. The
copy-based tools' "always copy" model is the maximally expensive answer to
uncertainty; pg-sprite's design is to pay the cheap probe first, and PostgreSQL's
transactional DDL makes the failed probe genuinely free.

**What changes when copy-and-swap lands (Phase 5):** the *meaning* of a refused
attempt. Today, when the size guard (or the budget) refuses, that's a dead end — the
caller sees a terminal refusal because there is no next rung. With a copy engine, the
same refusal becomes a routing signal: "this shape couldn't be proven or cheaply
attempted, so fall through to copy-and-swap." The ladder becomes
proof → bounded attempt → copy, instead of proof → bounded attempt → refuse.

The planner is already built for this: the route/disposition model has copy-and-swap
routes whose disposition is "routes to an execution strategy this build does not
implement" ([capabilities.md](capabilities.md) lists the rewrite shapes that route
there). The slot exists; the engine behind it doesn't yet. So the size guard doesn't
get deleted later either: it stops being a hard ceiling and becomes the threshold
that decides *which engine* runs.

One nuance: even then, "route to copy" isn't automatically "safe to proceed" —
copy-and-swap on a multi-TB table has its own cost (disk headroom, replication lag,
cutover risk). The routing moves the decision to an engine that is online *by
construction* rather than online *by luck*.

### Why does a wrong guess cost differently on small vs large tables?

The *budget* is the same, but what the budget buys differs. The wrong-guess cost is
"how long did we hold `ACCESS EXCLUSIVE` before finding out" — and that's where table
size bites, because a rewrite's duration scales with size while the budget doesn't.

- On a **small table**, the worst case is "the rewrite finished, slightly slower than
  a metadata change" — a small rewrite completes inside the statement budget, so a
  "wrong" guess still *succeeds*, cheaply.
- On a **big table**, the worst case is "full budget of hard downtime, then rollback,
  guaranteed" — the rewrite cannot finish inside any sane budget by construction, so
  the attempt burns the entire statement budget holding `ACCESS EXCLUSIVE`, blocking
  every read and write, and then rolls back having achieved nothing.

Two amplifiers make the big-table case worse than "one statement waits":

- **`ACCESS EXCLUSIVE` queues everything.** While the rewrite churns, every read and
  write on the table blocks. And PostgreSQL's lock queue is fair: even *after* the
  rollback releases the lock, the pile-up drains serially — the outage echo outlasts
  the attempt.
- **The rollback is clean but not free.** Transactional DDL means no debris, but the
  blocked-traffic window already happened; you can't roll back an outage.

The size guard exists to make that case unreachable: above the limit, the gamble's
downside is no longer bounded-and-tolerable, so we don't roll the dice at all.

## Timeline: the four bounded-attempt scenarios

```diagram
Case 1 — guess right (metadata-only), any table size
────────────────────────────────────────────────────
t0        t0+ms
│ acquire │ catalog-only update │ commit
│ AEL     │ (no data touched)   │
└─────────┴─────────────────────┘
Traffic blocked: milliseconds. This is the payoff case.

Case 2 — guess wrong, SMALL table (rewrite fits in budget)
──────────────────────────────────────────────────────────
t0        t0+ms                    t0+~1s
│ acquire │ rewrite all rows       │ commit
│ AEL     │ (small ⇒ fast)         │
└─────────┴────────────────────────┘
Traffic blocked: ~1s. "Wrong" guess still *succeeds* —
a small rewrite completes inside the budget. Tolerable.

Case 3 — guess wrong, BIG table, NO size guard
──────────────────────────────────────────────
t0        t0+ms                        budget end (e.g. +30s)
│ acquire │ rewrite churns, table 100% │ statement_timeout
│ AEL     │ locked: reads+writes queue │ fires → ROLLBACK
└─────────┴────────────────────────────┴──────────────────┘
Traffic blocked: the ENTIRE budget — and you get nothing
for it. The rewrite was never going to finish; you paid a
full-budget outage just to learn the guess was wrong.

Case 4 — BIG table, WITH size guard
───────────────────────────────────
t0
│ size check: table > limit → typed SizeError
└─ refuse. No lock taken, no DDL run, zero stall.
```

(`AEL` = `ACCESS EXCLUSIVE` lock. A fifth outcome — the lock is never *granted*
within `lock_timeout` because the table is contended — refuses with
`budget-lock-exceeded` before any DDL effect, at the cost of briefly queueing other
waiters behind the lock request.)

## What other tools do

| Tool | Model | Cost of uncertainty |
|---|---|---|
| **pg-sprite** | Proof, then bounded attempt, then (planned) copy-and-swap | Cheap probe; wrong guess bounded by budget and size guard |
| **pg_osc / pg_repack** (PostgreSQL) | Always copy-and-swap (shadow table + triggers/logical replay + cutover) | Maximum: full copy even for a metadata-only change |
| **pgroll** (PostgreSQL) | Expand/contract: versioned views over an evolving physical schema | No gamble, but the application must participate in the two-phase workflow |
| **pt-osc / gh-ost** (MySQL) | Always copy-and-swap | Same maximum-cost answer; built for a world without transactional DDL |
| **Spirit** (MySQL) | `ALGORITHM=INSTANT` first, fall back to copy | Not a gamble: MySQL refuses the instant assertion upfront, for free |

The column that explains pg-sprite's divergence: PostgreSQL's transactional DDL makes
a *failed attempt* cheap, and its lack of an `INSTANT` assertion makes an *upfront
answer* impossible. The bounded attempt is the design that exploits the strength to
cover the gap.

## The invariants at a glance

The canonical, testable registry is [invariants.md](invariants.md); the ones this
page rests on:

1. **The size check precedes all DDL.** Above the limit, the refusal is a typed
   `*preflight.SizeError` returned before any statement is issued — no lock is taken.
2. **Proof-exempt, attempt-guarded.** Planner-proven idioms and substituted sequences
   pass `NoSizeLimit`; blind attempts of the submitted form — including forced runs —
   are always size-guarded.
3. **Budgets are non-negotiable.** `lock_timeout` and `statement_timeout` are applied
   with `SET LOCAL` inside the attempt's transaction, regardless of session defaults.
4. **Typed outcomes, SQLSTATE matching.** Budget overruns map `55P03`/`57014` to
   stable codes; automation never branches on error prose.
5. **A budget refusal is not a failure.** Budget and size errors are refusal inputs
   that become not-native-safe verdicts; today they are terminal, and once
   copy-and-swap lands they become routing signals.
6. **The limit must be runnable.** `Options.MaxTableSizeBytes` must be positive;
   misconfiguration is rejected at the front door, before any database work.
