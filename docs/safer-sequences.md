# Safer-sequence substitution (the improve path)

The planner recognizes a submitted statement that is **native but blocking** —
PostgreSQL can do the work without a table rewrite, but the form as written
holds `ACCESS EXCLUSIVE` while it does real work — and substitutes the
**safer native sequence**: an ordered set of statements that reaches the same
declared end state while confining every exclusive lock to a brief,
metadata-only catalog flip. The classification is `safer-idiom` in the
[plan report](plan-report.md); `migrate` executes the substitution,
`migrate --dry-run` and [`suggest`](suggest-report.md) show it, and
[`lint`](lint-report.md) flags the blocking form offline. Watch the whole
flow in [demos/improve.gif](demos/improve.gif).

## Table of contents

- [A worked example: `ADD CONSTRAINT … UNIQUE`](#a-worked-example-add-constraint--unique)
- [Same end state, different path](#same-end-state-different-path)
- [What the engine adds over running the sequence by hand](#what-the-engine-adds-over-running-the-sequence-by-hand)
- [The substitutions the planner makes today](#the-substitutions-the-planner-makes-today)
- [Caveats are typed, not prose](#caveats-are-typed-not-prose)

## A worked example: `ADD CONSTRAINT … UNIQUE`

The submitted change:

```sql
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
```

Run as written, PostgreSQL builds the backing unique index **under
`ACCESS EXCLUSIVE`** — every read and write on `users` queues behind the
lock for the full table scan and sort. On a large or hot table that is an
outage, not a schema change.

The planner substitutes the two-step form:

```sql
CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "public"."users" ("email");
ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key";
```

Step 1 builds the index under `SHARE UPDATE EXCLUSIVE` — reads and writes
proceed throughout. Step 2 adopts the pre-built index as the constraint's
implementation under a brief, metadata-only `ACCESS EXCLUSIVE`. The engine
names the index after the constraint up front (your name is used as-is; a
generated name is built to fit PostgreSQL's identifier limit), so no rename
happens at adoption time.

## Same end state, different path

Catalog-wise the two forms are indistinguishable afterwards: both leave a
`pg_constraint` row of type `u` backed by a unique index of the same name,
and both validate **all existing rows** — neither grandfathers duplicates.
Everything that differs is on the way there:

|  | Submitted form (one statement) | Safer sequence (two steps) |
|---|---|---|
| Lock during the index build | `ACCESS EXCLUSIVE` for the whole scan + sort — blocks all reads and writes | `SHARE UPDATE EXCLUSIVE` — reads and writes proceed |
| Lock to attach the constraint | (included above) | `ACCESS EXCLUSIVE`, brief and metadata-only |
| A duplicate is found | Statement fails and rolls back cleanly — nothing left behind | The concurrent build fails and leaves an **INVALID index** that must be dropped before retrying — and an invalid *unique* index still enforces uniqueness against new writes until it is dropped |
| Transactionality | Can run inside a transaction block | `CREATE INDEX CONCURRENTLY` cannot — the sequence runs [autocommit-each-step](execution-model.md) |
| Cost | One table scan | Roughly two table scans plus waits for concurrent transactions to drain — slower in wall-clock time, cheaper in blocking |

That third row is the trade in miniature: the safer sequence converts
*blocking* risk into *leftover-state* risk. pg-sprite takes that trade
deliberately — blocking is paid by every query on the table, leftover state
by one operator with a [documented recovery path](invalid-index-recovery.md)
— and reports the boundary precisely when it happens
([execution-model.md](execution-model.md)).

## What the engine adds over running the sequence by hand

The two-step form is a well-known idiom; the engine's job is everything
around it:

- **Budgets on every step.** Brief catalog steps run under `SET LOCAL
  lock_timeout` / `statement_timeout` in their own short transaction; the
  concurrent build runs on a dedicated budgeted session. A lock pileup
  cancels the step instead of queueing behind (and blocking everything
  behind) a long-running query — the failure the raw `ADD CONSTRAINT …
  USING INDEX` invites when run without a `lock_timeout`.
- **A typed verdict, not a scrollback.** Success or failure, the outcome
  carries the committed prefix, the failed step, and a stable `code` —
  the [execution model](execution-model.md) is the contract.
- **The substitution is visible before it runs.** `migrate --dry-run`
  and `diff` print the sequence as compiler-style diagnostics;
  the [plan report](plan-report.md) carries it as `safer_sql` with a typed
  execution contract, so automation branches on fields, never on prose.

## The substitutions the planner makes today

| Submitted (blocking) form | Safer sequence |
|---|---|
| `ALTER COLUMN … SET NOT NULL` | 4 steps: `NOT VALID` CHECK → `VALIDATE` → `SET NOT NULL` (catalog flip) → drop the scaffold — [worked through step by step](execution-model.md#the-committed-prefix) |
| `ADD PRIMARY KEY` / `ADD UNIQUE` (direct) | 2 steps: `CREATE UNIQUE INDEX CONCURRENTLY` → `ADD CONSTRAINT … USING INDEX` (this page's example) |
| `ADD CHECK` / `ADD FOREIGN KEY` (direct) | 2 steps: `ADD CONSTRAINT … NOT VALID` → `VALIDATE CONSTRAINT` — the validation scan runs under a lock that lets reads and writes proceed |
| `CREATE INDEX` (non-concurrent) | 1 statement: the same build with `CONCURRENTLY` |
| `DETACH PARTITION` (non-concurrent) | 1 statement: `DETACH PARTITION … CONCURRENTLY` |

Statements already in the safe form (`… USING INDEX`, `… NOT VALID`,
`CONCURRENTLY`) are recognized as the online idiom and run as submitted.
The full operation → lock → substitution matrix is
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md); the
machine-readable advisory shape is [suggest-report.md](suggest-report.md).

## Caveats are typed, not prose

A safer sequence is a *different way to run the change*, not a free
upgrade, so each substitution carries typed caveats in the
[suggest report](suggest-report.md#caveats-caveats) — `non-transactional`,
`separate-transactions`, `invalid-index-on-failure`, `validation-scan`,
`detach-finalize-on-failure` — that automation can branch on. The
`USING INDEX` form has structural limits of its own: the adopted index
must be a plain unique B-tree (not partial, not an expression index), and
`CREATE INDEX CONCURRENTLY` is not supported on partitioned tables — the
planner refuses with a typed reason
([`unsupported-partitioned-parent`](postgres-online-ddl-reference.md#unsupported-partitioned-parent))
rather than substituting a sequence it cannot run online.
