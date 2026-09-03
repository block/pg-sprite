# Safer-sequence substitution (the improve path)

The planner recognizes a submitted statement that is **native but blocking** —
PostgreSQL can do the work without a table rewrite, but the form as written
holds `ACCESS EXCLUSIVE` while it does real work — and substitutes the
**safer native sequence**: an ordered set of statements that reaches the same
declared end state while confining every exclusive lock to a brief,
metadata-only catalog flip (the exception — `ADD PRIMARY KEY` on a nullable
column — and the other boundaries are in
[When the substitution does not help](#when-the-substitution-does-not-help)).
The classification is `safer-idiom` in the
[plan report](plan-report.md); `migrate` executes the substitution,
`migrate --dry-run` and [`suggest`](suggest-report.md) show it, and
[`lint`](lint-report.md) flags the blocking form offline. Watch the whole
flow in [demos/improve.gif](demos/improve.gif).

## Table of contents

- [A worked example: `ADD CONSTRAINT … UNIQUE`](#a-worked-example-add-constraint--unique)
- [Same end state, different path](#same-end-state-different-path)
- [What the engine adds over running the sequence by hand](#what-the-engine-adds-over-running-the-sequence-by-hand)
- [The substitutions the planner makes today](#the-substitutions-the-planner-makes-today)
- [When the substitution does not help](#when-the-substitution-does-not-help)
- [Check the claims yourself](#check-the-claims-yourself)
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

An at-a-glance summary — the authoritative, per-statement answer comes from
the tool itself (see below):

| Submitted (blocking) form | Safer sequence |
|---|---|
| `ALTER COLUMN … SET NOT NULL` | 4 steps: `NOT VALID` CHECK → `VALIDATE` → `SET NOT NULL` (catalog flip) → drop the scaffold — [worked through step by step](execution-model.md#the-committed-prefix) |
| `ADD UNIQUE` (direct) | 2 steps: `CREATE UNIQUE INDEX CONCURRENTLY` → `ADD CONSTRAINT … USING INDEX` (this page's example) |
| `ADD PRIMARY KEY` (direct) | The same 2 steps — **on a column already `NOT NULL`**. On a nullable column, adopting the index must also set `NOT NULL`, and PostgreSQL validates that by scanning the heap under `ACCESS EXCLUSIVE` — the blocking work the substitution exists to avoid, and a scan the brief step budget cancels. Reach `NOT NULL` first via the 4-step sequence above, then add the primary key |
| `ADD CHECK` / `ADD FOREIGN KEY` (direct) | 2 steps: `ADD CONSTRAINT … NOT VALID` → `VALIDATE CONSTRAINT` — the validation scan runs under a lock that lets reads and writes proceed |
| `CREATE INDEX` (non-concurrent) | 1 statement: the same build with `CONCURRENTLY` — on a live table only; an index on a table that does not exist yet (`diff` greenfield) is `metadata-only` and runs as written |
| `DETACH PARTITION` (non-concurrent) | 1 statement: `DETACH PARTITION … CONCURRENTLY` — shown by `--dry-run` and `suggest`, but **execution refuses this step today**: a cancelled concurrent detach leaves a detach-pending partition state the executor does not own recovering |

Statements already in the safe form (`… USING INDEX`, `… NOT VALID`,
`CONCURRENTLY`) are recognized as the online idiom and run as submitted.
The full operation → lock → substitution matrix is
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md).

The table is a summary; the tool is the source of truth. For any statement,
ask it directly — offline, nothing executes:

```console
$ echo 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);' \
    | pg-sprite suggest --json
```

```json
{
  "format_version": 2,
  "suggestions": [
    {
      "statement": 1,
      "line": 1,
      "column": 1,
      "original": "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
      "operation": "ADD CONSTRAINT users_email_key",
      "reason": "safer-idiom",
      "recommended": [
        "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
        "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
      ],
      "execution": "autocommit-each-step",
      "caveats": ["non-transactional", "separate-transactions", "invalid-index-on-failure"]
    }
  ]
}
```

`recommended` carries the substituted sequence, `caveats` the typed
conditions of running it — branch on those fields. (Offline, names render
as submitted; execution resolves them, which is why the worked example
above reads `"public"."users"`.) The versioned shape is the
[suggest report contract](suggest-report.md), so a new substitution cannot
ship with a stale doc row as its only description.

## When the substitution does not help

The substitution covers statements that are native but blocking as written.
It does not cover:

- **Operations that need a table rewrite** — a column type change that
  PostgreSQL cannot convert in place, for example. No safer native sequence
  exists; that is copy-and-swap's job. The per-operation routing is in
  [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md).
- **`ADD PRIMARY KEY` on a nullable column.** Adopting the pre-built index
  must also set `NOT NULL`, which PostgreSQL validates by scanning the heap
  under `ACCESS EXCLUSIVE` — so the adoption step is not the brief catalog
  flip it is everywhere else, and the brief step budget cancels it. Reach
  `NOT NULL` first via its own 4-step sequence, then add the primary key.
- **The `USING INDEX` structural limits.** The adopted index must be a
  plain unique B-tree — not partial, not an expression index. And
  `CREATE INDEX CONCURRENTLY` is not supported on partitioned tables —
  pg-sprite refuses with a typed reason
  ([`unsupported-partitioned-parent`](postgres-online-ddl-reference.md#unsupported-partitioned-parent))
  rather than substituting a sequence it cannot run online.
- **`DETACH PARTITION … CONCURRENTLY` at execution.** The planner
  substitutes it and `--dry-run`/`suggest` show it, but the executor
  refuses to run the step: a cancelled concurrent detach leaves a
  detach-pending partition state it does not own recovering.

## Check the claims yourself

Every lock and duration this page claims is observable on your own table.
While a sequence runs, watch the locks from another session — during step 1
and again during step 2:

```sql
SELECT mode, granted FROM pg_locks WHERE relation = 'users'::regclass;
```

and time the steps (`\timing on` in psql), or watch who waits on whom:

```sql
SELECT query, state, wait_event_type FROM pg_stat_activity
WHERE query LIKE '%users%';
```

Run the same checks against the submitted form on a scratch copy and the
comparison table above reproduces itself: the one-statement form holds
`ACCESS EXCLUSIVE` for the whole build; the two-step form never holds it
longer than a catalog flip.

## Caveats are typed, not prose

A safer sequence is a *different way to run the change*, not a free
upgrade, so each substitution carries typed caveats in the
[suggest report](suggest-report.md#caveats-caveats) — `non-transactional`,
`separate-transactions`, `invalid-index-on-failure`, `validation-scan`,
`detach-finalize-on-failure` — that automation can branch on.
