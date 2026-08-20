# Execution model: transactions, failure, and the committed prefix

**If a multi-step schema change fails halfway, what state is your table in?**
Every step before the failure has committed — permanently. The failing step
rolled back. Nothing after it ran. pg-sprite tells you exactly where that line
is, and every state it can leave behind is documented, harmless to live
traffic, and a step *toward* the desired schema — not debris.

This page explains why the engine works this way and what the guarantees are.
The machine-readable contracts live in
[plan-report.md](plan-report.md#execution-contracts-execution-safer_sql_execution)
and [suggest-report.md](suggest-report.md#caveats-caveats).

## Table of contents

- [Why there is no wrapping transaction](#why-there-is-no-wrapping-transaction)
- [The committed prefix](#the-committed-prefix)
- [How a failure is reported](#how-a-failure-is-reported)
- [Why the prefix is safe to leave](#why-the-prefix-is-safe-to-leave)

## Why there is no wrapping transaction

Safer sequences run under the **autocommit-each-step** contract: one statement
at a time, in order, each in its own implicit or bounded transaction, never
inside one enclosing block. That is not a design shortcut — PostgreSQL forces
it:

- `CREATE INDEX CONCURRENTLY` refuses to run inside a transaction block at
  all; it manages multiple transactions internally.
- `VALIDATE CONSTRAINT` is only online because it runs in a *separate*
  transaction from `ADD CONSTRAINT ... NOT VALID`. Fused into one block, the
  strong lock from the ADD is held across the full-table validation scan —
  exactly the blocking behavior the sequence exists to avoid.

```diagram
one enclosing block (impossible / self-defeating)   autocommit-each-step
┌─ BEGIN ────────────────────────────┐              ┌────────┐ ┌────────┐ ┌────────┐
│ step 1  ── locks held ──────────▶  │              │ step 1 │ │ step 2 │ │ step 3 │
│ step 2  ── ...across every ─────▶  │              │ commit │ │ commit │ │ commit │
│ step 3  ── later step ──────────▶  │              └────────┘ └────────┘ └────────┘
└─ COMMIT ───────────────────────────┘               each step's locks released
  CONCURRENTLY forms refuse outright                 before the next step starts
```

An "atomic online schema change" is therefore a contradiction in PostgreSQL:
the sequencing across transaction boundaries *is* the safety mechanism.

"Implicit or bounded" above is the mechanical detail behind the contract —
autocommit-each-step has two shapes in the executor:

- **Brief catalog steps and `VALIDATE CONSTRAINT`** each run as one short
  *explicit* transaction: `BEGIN` → `SET LOCAL lock_timeout` /
  `statement_timeout` → the statement → `COMMIT` (`pkg/executor`'s bounded
  runner). The explicit `BEGIN` exists only because the budgets are applied
  with `SET LOCAL`, which is scoped to that transaction — functionally it is
  still one statement, one transaction, committed immediately, rolled back
  atomically on failure.
- **`CREATE INDEX CONCURRENTLY`** is true autocommit on a dedicated budgeted
  session: it refuses to run inside any transaction block and internally
  manages multiple transactions of its own.

## The committed prefix

Execution is strictly ordered, so a run that stops at step N leaves a precise
shape: steps 1 through N−1 each committed, step N rolled back, steps N+1
onward never attempted. The leading run of completed steps is the **committed
prefix** — no holes, no in-limbo steps.

The four-step `SET NOT NULL` sequence, failing at step 3:

```diagram
step 1  ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID  ── committed ─┐ committed
step 2  VALIDATE CONSTRAINT                                   ── committed ─┘ prefix
step 3  ALTER COLUMN ... SET NOT NULL                         ── FAILED (rolled back)
step 4  DROP CONSTRAINT (the scaffold)                        ── never attempted
```

Because each step ran in its own transaction, steps 1–2 do not roll back when
step 3 fails — PostgreSQL already made them durable. What remains is a
validated CHECK constraint enforcing non-nullness: safe, non-blocking, and
harmless to keep until a retry finishes the job.

## How a failure is reported

You are never left guessing which side of the line a step landed on. Take
the scenario above: a competing lock outlasts step 3's bounded retries, so
the catalog flip exceeds its lock budget. The human verdict draws the line
explicitly (exit 1):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN email SET NOT NULL'
failed (budget-lock-exceeded)
  table:     public.users
  statement: ALTER TABLE public.users ALTER COLUMN email SET NOT NULL
  detail:    sequence step 3 of 4 failed; the 2 committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes
  failed at: step 3: ALTER TABLE "public"."users" ALTER COLUMN "email" SET NOT NULL
  committed before the failure (their state remains):
    1. ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_not_null" CHECK ("email" IS NOT NULL) NOT VALID
    2. ALTER TABLE "public"."users" VALIDATE CONSTRAINT "users_email_not_null"
```

With `--json`, the same verdict is the machine-readable twin — `executed_sql`
is the committed prefix, `failed_step`/`failed_step_sql` the boundary, and
everything after was never attempted:

```json
{
  "outcome": "failed",
  "code": "budget-lock-exceeded",
  "failed_step": 3,
  "failed_step_sql": "ALTER TABLE \"public\".\"users\" ALTER COLUMN \"email\" SET NOT NULL",
  "statement": "ALTER TABLE public.users ALTER COLUMN email SET NOT NULL",
  "table": "public.users",
  "detail": "sequence step 3 of 4 failed; the 2 committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes",
  "executed_sql": [
    "ALTER TABLE \"public\".\"users\" ADD CONSTRAINT \"users_email_not_null\" CHECK (\"email\" IS NOT NULL) NOT VALID",
    "ALTER TABLE \"public\".\"users\" VALIDATE CONSTRAINT \"users_email_not_null\""
  ]
}
```

Automation branches on `code` — the stable outcome vocabulary — never on
prose, which is free to change. Reading the three surfaces:

- **Exit codes** separate the cases: an execution failure exits 1 (as here);
  a refusal — where nothing was ever attempted — exits 2. See
  [cli-output-examples.md](cli-output-examples.md).
- **Library callers** get the same facts typed: `errors.As` to
  `*executor.SequenceStepError` (`Step`, `Total`, `SQL`, and the underlying
  cause), with `executor.OutcomeCode` mapping any executor error to its
  stable code. The process error carries the same line for logs:
  `sequence step 3 of 4 (brief) failed; steps before it committed and their
  state remains: execution exceeded its lock budget (3s) after 3 bounded
  attempts`.
- **An empty `executed_sql`** on a failed verdict means nothing committed —
  the failure hit step 1, so there is no partial state at all.

## Why the prefix is safe to leave

Three mechanisms turn non-atomicity from a hazard into a contract:

1. **Whole-sequence admission before step 1.** Every step is re-parsed,
   shape-classified, and verified against the preflighted table before
   anything executes. Refusals that are decidable up front (unsupported
   statement shapes, partitioned parents, a defective retry policy, a
   too-small pool) happen *before* the first commit, so they can never strand
   a prefix.
2. **Every reachable partial state is documented.** Each safer sequence
   carries a partial-failure contract; the typed caveat vocabulary in
   [suggest-report.md](suggest-report.md#caveats-caveats) names the same
   states for offline consumers:

   | Sequence | A failed step leaves | Retry path |
   | --- | --- | --- |
   | `SET NOT NULL` (4 steps) | The NOT VALID CHECK scaffold | Resume at VALIDATE; a leftover scaffold is removed by running the DROP CONSTRAINT step alone |
   | `ADD PRIMARY KEY` / `UNIQUE USING INDEX` (2 steps) | An INVALID index (`pg_index.indisvalid = false`) | Drop the invalid index, re-run the build — see [invalid-index-recovery.md](invalid-index-recovery.md); the engine detects and reports it but never drops it unprompted |
   | `ADD CONSTRAINT ... NOT VALID` + `VALIDATE` | The NOT VALID constraint, enforcing for new writes | Re-run VALIDATE — it is safe to repeat |

3. **The prefix is progress, not debris.** Steps move monotonically toward
   the desired state, so a retry resumes from the boundary rather than
   starting over — and never double-applies work, because re-running a
   committed step fails with a distinct SQLSTATE the contract names
   (`duplicate_object`, `duplicate_table`).

The same semantics scale up one level: anything that executes more than one
statement around the engine — an orchestrator converging a table onto its
desired state — must report the statements already committed, the one that
failed, and the ones never attempted. That is the strongest guarantee
available once PostgreSQL rules out atomicity for online DDL, and it is the
contract embedders should expose rather than paper over.
