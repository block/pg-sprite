# Execution model: transactions, failure, and the committed prefix

**If a multi-step schema change fails halfway, what state is your table in?**
Every step before the failure has committed — permanently. The failing step
rolled back. Nothing after it ran. pg-sprite tells you exactly where that line
is. Everything in the committed prefix is documented, harmless to live
traffic, and a step *toward* the desired schema — not debris. A failed step
can also leave state of *its own*: a concurrent index build that dies
mid-build leaves an INVALID index that taxes every write until it is removed
— by the executor's proven recovery when the entry is provably abandoned, by
an operator otherwise ([invalid-index-recovery.md](invalid-index-recovery.md))
— the verdict's `code` names that outcome when it happens.

This page explains why the engine works this way and what the guarantees are.
The machine-readable contracts live in
[plan-report.md](plan-report.md#execution-contracts-execution-safer_sql_execution)
and [suggest-report.md](suggest-report.md#caveats-caveats).

## Table of contents

- [Why there is no wrapping transaction](#why-there-is-no-wrapping-transaction)
- [The committed prefix](#the-committed-prefix)
- [How a failure is reported](#how-a-failure-is-reported)
- [Why the prefix is safe to leave](#why-the-prefix-is-safe-to-leave)
- [Outcome codes](#outcome-codes)

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

- **Brief catalog steps (step kind `brief`) and `VALIDATE CONSTRAINT` (step
  kind `validate-constraint`)** each run as one short *explicit* transaction:
  `BEGIN` → `SET LOCAL lock_timeout` / `statement_timeout` → the statement →
  `COMMIT` (`pkg/executor`'s bounded runner). When the preflight proof
  carries a schema, the same `SET LOCAL` pins `search_path` to that schema
  then `public`, so the statement's unqualified secondary names — a
  column's type, an expression's function — resolve in the target schema,
  exactly as the introspection read path resolves them. The explicit
  `BEGIN` exists only because the settings are applied with `SET LOCAL`,
  which is scoped to that transaction — functionally it is still one
  statement, one transaction, committed immediately, rolled back
  atomically on failure.
- **`CREATE INDEX CONCURRENTLY` (step kind `concurrent-index-build`)** is
  true autocommit on a dedicated budgeted session: it refuses to run inside
  any transaction block and internally manages multiple transactions of its
  own. Its bound is either the session's overall `statement_timeout` or, in
  explicit caller-owned mode, the caller's cancellable context while
  `statement_timeout` is disabled.

Each step's class is the `kind` field of its step report in the JSON
verdict — the field retry logic branches on. A failed `brief` step means
something held a lock longer than the brief budget tolerates: retrying is
reasonable. A failed `validate-constraint` step means the validation scan
exceeded its own budget: retrying without raising it will fail the same
way. A failed `concurrent-index-build` carries its own invalid-index
verdict ([invalid-index-recovery.md](invalid-index-recovery.md)).

## The committed prefix

Execution is strictly ordered, so a run that stops at step N leaves a precise
shape: steps 1 through N−1 each committed, step N rolled back, steps N+1
onward never attempted. The leading run of completed steps is the **committed
prefix** — no holes, no in-limbo steps.

The running example is a single submitted statement:

```sql
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
```

Run as-is, PostgreSQL takes `ACCESS EXCLUSIVE` and scans every row to prove no
NULLs exist — blocking all reads and writes for the whole scan. The planner
substitutes a four-step native sequence that moves the scan off the exclusive
lock, exploiting the fact that PostgreSQL (12+) skips the `SET NOT NULL` scan
when a validated `CHECK` constraint already proves the invariant:

| # | Statement | What it does | Lock (duration) | Budget class |
|---|---|---|---|---|
| 1 | `ADD CONSTRAINT … CHECK (email IS NOT NULL) NOT VALID` | Installs the scaffold: enforced for new writes immediately; existing rows not yet checked | `ACCESS EXCLUSIVE` (brief — `NOT VALID` skips the scan) | brief |
| 2 | `VALIDATE CONSTRAINT …` | Scans the table to prove existing rows satisfy the invariant — the long part | `SHARE UPDATE EXCLUSIVE` (long, but reads and writes continue) | validate (own overall bound) |
| 3 | `ALTER COLUMN email SET NOT NULL` | The actual change — now a pure catalog flip, because the validated `CHECK` proves the invariant so no scan is needed | `ACCESS EXCLUSIVE` (brief) | brief |
| 4 | `DROP CONSTRAINT …` (the scaffold) | Removes the now-redundant scaffold constraint | `ACCESS EXCLUSIVE` (brief) | brief |

The exclusive locks are held only for instant catalog flips; the single
full-table scan runs under a lock that blocks neither reads nor writes.

The same sequence, failing at step 3:

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
  `*executor.SequenceStepError` (`Step`, `Total`, `SQL`, `Kind` — the
  execution class, the `(brief)` in the line below — and the underlying
  cause), with `executor.OutcomeCode` mapping any executor error to its
  stable code. The process error carries the same line for logs:
  `sequence step 3 of 4 (brief) failed; steps before it committed and their
  state remains: execution exceeded its lock budget (3s) after 3 bounded
  attempts`.
- **An empty `executed_sql`** on a failed verdict means no *earlier* step
  committed — not that nothing was left behind: `code` names the outcome
  and any state the failed step itself left (a concurrent index build that
  fails at step 1 leaves an INVALID index with an empty committed prefix).
  `failed_step` is the discriminator: `1` means a sequence stopped at its
  first step; absent means no sequence ran at all — a single bounded
  attempt failed, and a started bounded attempt rolls back.

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

   | Sequence | A failed step leaves | Retry path | Safe to automate? |
   | --- | --- | --- | --- |
   | `SET NOT NULL` (4 steps) | The NOT VALID CHECK scaffold | Resume at the failed step; a leftover scaffold is removed by running the DROP CONSTRAINT step alone | Yes — a committed step refuses to double-apply (`duplicate_object`) |
   | `ADD PRIMARY KEY` / `UNIQUE USING INDEX` (2 steps) | An INVALID index (`pg_index.indisvalid = false`) | `RebuildAbandonedIndex` removes the entry under proof and re-runs the build — see [invalid-index-recovery.md](invalid-index-recovery.md) | **Only as an explicit recovery call**: the sequence itself never drops; the recovery drops only what it has proven abandoned under the table lock, and refuses an in-flight or another table's entry |
   | `ADD CONSTRAINT ... NOT VALID` + `VALIDATE` | The NOT VALID constraint, enforcing for new writes | Re-run VALIDATE — it is safe to repeat | Yes — VALIDATE is idempotent |

   Nothing resumes automatically — deliberately. The engine never picks up
   another run's leftovers on its own; the retry column is what a human or
   an embedding orchestrator issues next. And since no verdict field names
   the sequence, identify the row from the statement you submitted, not
   from the verdict.

3. **The prefix is progress, not debris.** Steps move monotonically toward
   the desired state, so a retry resumes from the boundary rather than
   starting over — and never double-applies work, because re-running a
   committed step fails with a distinct SQLSTATE the contract names
   (`duplicate_object`, `duplicate_table`).

Finishing the step-3 failure from
[How a failure is reported](#how-a-failure-is-reported) is the two steps the
run never reached. The validated scaffold makes the catalog flip scan-free —
PostgreSQL proves non-nullness from the constraint — so the safe finish is
the submitted form itself, forced past the routing that would otherwise
re-derive the full sequence, then the scaffold drop (metadata-only, no force
needed):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN email SET NOT NULL' --force public.users
$ pg-sprite migrate --alter 'ALTER TABLE users DROP CONSTRAINT users_email_not_null'
```

Re-issuing the original statement *without* `--force` fails loudly at step 1
with `duplicate_object` instead of silently double-applying: that SQLSTATE
is the guard against repeating committed work, not a resume mechanism.

The same semantics scale up one level: anything that executes more than one
statement around the engine — an orchestrator converging a table onto its
desired state — must report the statements already committed, the one that
failed, and the ones never attempted. That is the strongest guarantee
available once PostgreSQL rules out atomicity for online DDL, and it is the
contract embedders should expose rather than paper over.

The engine implements that contract itself: `migrate.RunDesired` (in
[`pkg/migrate`](../pkg/migrate/desired.go)) derives the convergence plan for
a desired-state schema and executes each planned statement back through the
same pipeline — fresh introspection, classification, and routing per
statement — stopping at the first refusal or failure. Its result carries the
plan, one verdict per attempted statement, and a detail naming exactly which
planned statements committed and remain in effect: the committed prefix at
the plan level, statements instead of steps.

## Outcome codes

`executor.Codes()` enumerates the closed vocabulary below, and
`executor.OutcomeCode` maps any executor error to its entry — the same code
that reaches the JSON verdict's `code` field. Adapters render three facts
per failure — the outcome code, the failing step's position
(`SequenceStepError.Step` of `.Total`), and the failing step's SQL — and
log the raw error, whose text interpolates server prose and is not a
branching surface.

| Code | Meaning |
| --- | --- |
| `budget-lock-exceeded` | The lock was not granted within `lock_timeout`; nothing executed |
| `budget-statement-exceeded` | The statement ran past `statement_timeout` and was cancelled |
| `cancelled-externally` | The statement was cancelled from outside the executor before its budget elapsed |
| `invalid-index-own-leftover` | The failed build's own INVALID index remains; `RebuildAbandonedIndex` removes it under proof ([recovery runbook](invalid-index-recovery.md)) |
| `invalid-index-build-in-flight` | The INVALID index under the requested name is another backend's concurrent build still running; wait, never drop |
| `invalid-index-abandoned` | An INVALID index under the requested name sits on the target table with no backend building it; `RebuildAbandonedIndex` removes it under proof |
| `invalid-index-other-table` | An INVALID index under the requested name sits on a different table in the schema; this change never touches it |
| `invalid-index-builder-unobservable` | An INVALID index under the requested name sits on the target table and this role cannot see whether a backend is building it; `RebuildAbandonedIndex` decides under the table lock |
| `invalid-index-unproven` | An INVALID index may remain but the catalog state could not be proven, or a recovery could not carry its proof through to the removal |
| `empty-sequence` | The sequence had no steps to run |
| `unsupported-sequence-step` | A step is not a shape the sequence executor can run safely |
| `unsupported-partitioned-parent` | Partitioned-parent admission refusal |
| `not-concurrent-index-build` | The statement handed to the concurrent build executor is not a `CREATE INDEX CONCURRENTLY` |
| `unnamed-index` | The concurrent build does not name its index, so its outcome could not be verified |
| `unqualified-table` | The target table is not schema-qualified at the library boundary |
| `if-not-exists-unsupported` | `CREATE ... IF NOT EXISTS` cannot prove what its no-op would mean |
| `create-collision` | A name the create path needs is already taken on the server; re-diff the live catalog |
| `duplicate-create-name` | The desired set claims the same relation name twice; refused at admission |
| `partition-of-unsupported` | `CREATE TABLE PARTITION OF` locks the partitioned parent, which the absence proof does not cover |
| `unsupported-create-step` | A desired statement is not a shape the create path can run |
| `pool-too-small` | The pool cannot hold the build session and the verdict connection at once |
| `table-not-found` | The statement's qualified table does not exist |
| `invariant-violation` | A breach of the invariant registry; never a retry candidate |
| `execution-failed` | Fallback for a failure outside the typed set — an operational error to investigate, not a refusal to branch on |
