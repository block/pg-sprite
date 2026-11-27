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

- **Brief catalog steps (step kind `brief`) and `VALIDATE CONSTRAINT`, alone or
  beside operations that take only `SHARE UPDATE EXCLUSIVE` (step kind
  `validate-constraint`)** each run as one short *explicit* transaction:
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

  Before a brief step first requests its lock, it checks `pg_locks` for a
  reader or writer that has held a conflicting lock on a table the step
  locks for a while. A step that queued behind one would hold every later
  query of the table behind itself, so it waits outside the lock queue
  instead, for up to 30 s (`RetryPolicy.HolderWait`), and reports the
  holder in progress as `lock_holder_wait`. Then it requests its lock as
  usual, whether or not the holder has finished, so the wait can delay a
  step but never fails one. It never ends another session. A step that
  takes only `SHARE UPDATE EXCLUSIVE` conflicts with no reader or writer,
  so it does not wait.
- **`CREATE INDEX CONCURRENTLY` (step kind `concurrent-index-build`)** is
  true autocommit on a dedicated budgeted session: it refuses to run inside
  any transaction block and internally manages multiple transactions of its
  own. Its bound is either the session's overall `statement_timeout` or, in
  explicit caller-owned mode, the caller's cancellable context while
  `statement_timeout` is disabled. In that mode the bound is the client
  call, not the server statement: cancelling the context ends the call and
  sends the server a cancel request, but a client that dies without
  cancelling leaves the build running on the server with no timeout. The
  stop path for a build the client no longer holds is `Tracker.CancelBuild`
  while the call is live, or an operator's `pg_cancel_backend` afterwards.
- **`ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY` alone (step kind
  `detach-partition-concurrently`)** also refuses a transaction block and
  commits two transactions of its own: the first marks the partition
  detach-pending under `SHARE UPDATE EXCLUSIVE` on the table and the
  partition, then the statement waits for every transaction that could
  still see the partition through the table, and the second takes `ACCESS
  EXCLUSIVE` on the partition alone to finish. It runs on a dedicated
  session under the brief budgets: `lock_timeout` bounds each lock wait,
  the wait for older transactions included, and `statement_timeout` the
  whole statement. Before it runs, a table with a `DEFAULT` partition, which
  PostgreSQL refuses a concurrent detach for, is refused
  (`detach-default-partition`), and a partition an earlier run left
  detach-pending is finished with `DETACH PARTITION ... FINALIZE` instead.
  PostgreSQL detaches one partition of a table at a time, so while another
  partition of the table is detach-pending the step is refused before it
  runs (`partition-detach-pending`, naming that partition and its
  `FINALIZE`). A lock budget exhausted before the first transaction commits
  is retried like a brief step's, and once the partition is detach-pending
  the retry runs `FINALIZE`. A step that stops with the partition still
  pending fails with `partition-detach-interrupted`. The rerun waits for
  older transactions the same way, under the same lock budget, so while a
  transaction older than that budget stays open, raise `--lock-timeout` or
  let the transaction end before the rerun. For library callers, the
  step's report sets `finished_pending_detach` (`StepReport.FinishedPendingDetach`)
  when a `FINALIZE` ended the detach, whether an earlier run or an earlier
  attempt of the same step left the partition pending; it is informational,
  and the verdict does not carry it.

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
  server:    canceling statement due to lock timeout (SQLSTATE 55P03)
  committed before the failure (their state remains):
    1. ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_not_null_check" CHECK ("email" IS NOT NULL) NOT VALID
    2. ALTER TABLE "public"."users" VALIDATE CONSTRAINT "users_email_not_null_check"
```

With `--json`, the same verdict is the machine-readable twin — `executed_sql`
is the committed prefix, `failed_step`/`failed_step_sql` the boundary, and
everything after was never attempted:

```json
{
  "outcome": "failed",
  "code": "budget-lock-exceeded",
  "sqlstate": "55P03",
  "server_error": "canceling statement due to lock timeout",
  "failed_step": 3,
  "failed_step_sql": "ALTER TABLE \"public\".\"users\" ALTER COLUMN \"email\" SET NOT NULL",
  "statement": "ALTER TABLE public.users ALTER COLUMN email SET NOT NULL",
  "table": "public.users",
  "detail": "sequence step 3 of 4 failed; the 2 committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes",
  "executed_sql": [
    "ALTER TABLE \"public\".\"users\" ADD CONSTRAINT \"users_email_not_null_check\" CHECK (\"email\" IS NOT NULL) NOT VALID",
    "ALTER TABLE \"public\".\"users\" VALIDATE CONSTRAINT \"users_email_not_null_check\""
  ]
}
```

Automation branches on `code` — the stable outcome vocabulary — never on
prose, which is free to change. When PostgreSQL itself rejected the step, the
verdict also carries its `sqlstate`, which automation can branch on too, and
`server_error`, the error's primary message for display. A lock budget ends
in the server's lock timeout, so the verdict above carries `55P03`; a failure
pg-sprite raised itself, such as a created table whose names do not match,
carries neither. Reading the three surfaces:

- **Exit codes** separate the cases: an execution failure exits 1 (as here);
  a refusal — where nothing committed, even if an optimistic attempt ran and
  was rolled back — exits 2; a change the operator explicitly accepted as
  blocking, which committed without an online-safety guarantee, exits 3.
  Exit 0 is exclusive to online-safe execution. The full ladder is in
  [cli-output-examples.md](cli-output-examples.md#exit-codes).
- **Library callers** get the same facts typed: `errors.As` to
  `*executor.SequenceStepError` (`Step`, `Total`, `SQL`, `Kind` — the
  execution class, the `(brief)` in the line below — and the underlying
  cause), with `executor.OutcomeCode` mapping any executor error to its
  stable code. The process error carries the same line for logs:
  `sequence step 3 of 4 (brief) failed; steps before it committed and their
  state remains: execution exceeded its lock budget (3s) after 3 bounded
  attempts`.
- **An empty `executed_sql`** on a failed verdict means no *earlier* step
  is known to have committed — not that nothing was left behind: `code` names the outcome
  and any state the failed step itself left (a concurrent index build that
  fails at step 1 leaves an INVALID index with an empty committed prefix).
  `failed_step` is the discriminator: `1` means a sequence stopped at its
  first step; absent means no sequence ran at all — a single bounded
  attempt failed, and a started bounded attempt that fails before its
  `COMMIT` rolls back. The exception is `commit-outcome-unknown`: the
  `COMMIT` itself did not return success and may have landed, so the failed
  step (or the single statement) is neither committed nor rolled back as far
  as the client can tell — `executed_sql` lists only the steps before it, the
  verdict says the outcome is unknown, and the catalog decides. A greenfield
  create that fails at step 1 with `create-name-mismatch` or
  `create-names-unverified` is the other case of a failed first step
  leaving state: its `CREATE TABLE` committed and stays — the code names
  the outcome, and the executor never drops a table it has just created.

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
   | `ADD PRIMARY KEY` / `UNIQUE USING INDEX`, failed at the attach (step 2) | A valid unique index; the attach failed, for example on a null in the key column (SQLSTATE `23502`) or its lock budget | Fix the cause, then attach the existing index with `ADD CONSTRAINT <name> PRIMARY KEY USING INDEX <name>` (or `UNIQUE USING INDEX`). Re-running the original statement fails at step 1, because the index already exists | Yes — the attach is a single statement, and it fails rather than doubles if the constraint already exists |
   | `ADD CONSTRAINT ... NOT VALID` + `VALIDATE` | The NOT VALID constraint, enforcing for new writes | Re-run VALIDATE — it is safe to repeat | Yes — VALIDATE is idempotent |
   | `DETACH PARTITION ... CONCURRENTLY` (1 step), failed with `partition-detach-interrupted` | A detach-pending partition: reads through the table leave it out, and rows in its range are refused | Run the same statement again, which finishes the detach with `DETACH PARTITION ... FINALIZE`; raise the lock budget first while a transaction older than it stays open | Yes, while the code is `partition-detach-interrupted`. A rerun after the detach has finished on the server fails with `execution-failed`, since the partition is no longer the table's |

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
$ pg-sprite migrate --alter 'ALTER TABLE users DROP CONSTRAINT users_email_not_null_check'
```

Re-issuing the original statement *without* `--force` fails loudly at step 1
with `duplicate_object` instead of silently double-applying: that SQLSTATE
is the guard against repeating committed work, not a resume mechanism.

That guard only matches a scaffold of the current name. Releases through
v0.3.3 named it `<table>_<column>_not_null`, the name PostgreSQL 18 gives
the NOT NULL constraint itself, so a scaffold such a release left behind is
not caught by step 1: a re-issue adds a second scaffold, drops only that
one, and on PostgreSQL 18 records the NOT NULL constraint as
`<table>_<column>_not_null1`. Before finishing a partial failure an older
release left, drop the old scaffold first, after confirming it is the CHECK
and not the server's own NOT NULL constraint (`contype = 'c'` in
`pg_constraint`):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users DROP CONSTRAINT users_email_not_null'
```

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

Committing every planned statement is not proof the table converged: a
concurrent session, an event trigger, or a planner gap can leave the table
still differing from the desired schema after the last commit. So on an
existing table `RunDesired` derives the plan again after the last commit and
reports executed only when that fresh plan is empty. When it is not, the
result is failed with `migrate.ErrNotConverged`, every verdict still reads
executed, and the result's `convergence` field reports `not-converged` with the
fresh plan as `remaining`, what a rerun would execute. A fresh plan that cannot
be derived reports `unverifiable`, and a proven run reports `converged`, so a
`--json` consumer tells the three apart without reading `detail`. A greenfield create
does not re-plan, since deriving a plan builds the desired side in a scratch
schema and needs `CREATE` on the database; it verifies instead that the new
table owns every relation name the desired file claimed.

## Outcome codes

`executor.Codes()` enumerates the closed vocabulary below, and
`executor.OutcomeCode` maps any executor error to its entry — the same code
that reaches the JSON verdict's `code` field. Adapters render three facts
per failure — the outcome code, the failing step's position
(`SequenceStepError.Step` of `.Total`), and the failing step's SQL — and
log the raw error, whose text interpolates server prose and is not a
branching surface.

The `Permanent` column is `Code.Permanent()`: the outcome is decided by the
statement, the caller's configuration, or the standing catalog, so retrying
the same call unchanged reproduces it and no executor entry point changes
it — an author or operator has to act first. It is the floor an adapter's
retry policy stands on, not its ceiling: an adapter may decline to retry a
code that is not permanent (a statement budget it sized as a lease, for
one), but a permanent code it keeps retrying loops for ever. The
invalid-index family splits on this line — an entry `RebuildAbandonedIndex`
can prove abandoned, a build to wait out, or a proof to re-take is not
permanent; an entry on another table or one the server will not drop
concurrently is.

| Code | Permanent | Meaning |
| --- | --- | --- |
| `budget-lock-exceeded` | no | The lock was not granted within `lock_timeout`; nothing executed |
| `budget-statement-exceeded` | no | The statement ran past `statement_timeout` and was cancelled |
| `row-security-plan-changed` | yes | Preview again and review the new ordered SQL before retrying; no target RLS statements ran |
| `row-security-refused` | yes | Change the declaration, unsupported target shape, or privileges before retrying |
| `row-security-outcome-unknown` | no | The atomic RLS commit response is uncertain; inspect the catalog before retrying |
| `blocking-outcome-unknown` | no | The accepted blocking transaction reached an ambiguous client boundary; inspect the catalog before retrying |
| `commit-outcome-unknown` | no | A bounded attempt's `COMMIT` did not return success — a dropped connection, a failover, or the caller's context ending mid-commit — so the change may have landed; inspect the catalog before retrying, and on a sequence do not assume a retry resumes from the failed step |
| `drop-outcome-unknown` | no | A reviewed index drop completed, but the lookup that confirms it dropped the reviewed index failed, so which index was dropped is unknown. The message names the checked index's OID: if it is gone from the catalog, the reviewed index was dropped. Inspect the table's indexes before retrying; a rerun alone reports `dropped-index-drift` once the name is gone, whichever index was dropped |
| `invalid-blocking-budget` | yes | An accepted blocking bound is disabled, cannot be represented by PostgreSQL, or the statement bound is not longer than the lock bound |
| `unsupported-accepted-blocking` | yes | The statement is outside the accepted blocking executor's narrow index-maintenance set, or the server will not run it inside the engine-owned transaction (`REINDEX` on a partitioned relation, SQLSTATE `25001`) |
| `cancelled-by-caller` | no | The caller's own context ended while the statement ran — or, on the accepted blocking path, while the engine's table lock request was still waiting, so nothing was submitted — and the budget had not elapsed; in caller-owned mode this is the build's ordinary exit |
| `cancelled-externally` | no | The statement was cancelled from outside the executor — not by its caller and not by its budget; an operator's `pg_cancel_backend` or `Tracker.CancelBuild` |
| `invalid-index-own-leftover` | no | The failed build's own INVALID index remains; `RebuildAbandonedIndex` removes it under proof ([recovery runbook](invalid-index-recovery.md)). A build that failed on its rows reports `data-fault` instead |
| `invalid-index-build-in-flight` | no | The INVALID index under the requested name is another backend's concurrent build still running; wait, never drop |
| `invalid-index-abandoned` | no | An INVALID index under the requested name sits on the target table with no backend building it; `RebuildAbandonedIndex` removes it under proof |
| `invalid-index-other-table` | yes | An INVALID index under the requested name sits on a different table in the schema; this change never touches it |
| `invalid-index-not-droppable` | yes | The INVALID index under the requested name is one `DROP INDEX CONCURRENTLY` cannot remove — a partitioned table's index, an index partition, or a constraint's index — so it is not a failed concurrent build's debris; an operator resolves it |
| `invalid-index-builder-unobservable` | no | An INVALID index under the requested name sits on the target table and this role cannot see whether a backend is building it; `RebuildAbandonedIndex` decides under the table lock |
| `invalid-index-unproven` | no | An INVALID index may remain but the catalog state could not be proven, or a recovery could not carry its proof through to the removal |
| `not-create-index` | yes | The statement handed to the partitioned index build is not a single `CREATE INDEX` |
| `not-partitioned-table` | yes | The target of a partitioned index build is not a partitioned table; a plain table's index is the concurrent build's job |
| `partitioned-index-name-taken` | yes | A name the partitioned index build needs is held by a relation that is not the requested index: another kind of relation, an index of another definition, or an index on another table, under the parent index's name or a partition index's derived name. The occupant is left for an operator |
| `foreign-table-partition` | yes | The partition tree holds a foreign table, which cannot carry an index, so the parent's index could never become valid; refused before anything is created |
| `unique-index-partition-key` | yes | A unique index does not contain, among its plain key columns, the partition key of the parent and of every sub-partitioned partition. PostgreSQL checks each partitioned table only when it gets its index, so the build checks every level first; refused before anything is created, as an `unsupported-statement`, `by-design` refusal, in the dry run too; `UniqueIndexPartitionKeyError.Missing` names each table and the columns its key lacks. Add the missing partition key columns to the index's key |
| `table-lock-unconfirmed` | yes | The pool's role cannot see when the session holding the partitioned index build's table lock started, so the build cannot confirm the lock is its own session's rather than one with the same PID on another server; refused before anything is created. Connect the pool as the role the connection target uses, or grant the pool's role `pg_read_all_stats` |
| `partitioned-foreign-key-name-taken` | yes | The name the partitioned foreign key build derives for a partition's foreign key is held by another constraint of that partition. The occupant is left for an operator, and nothing is added to the partition |
| `partitioned-foreign-key-conflict` | yes | A partition already has foreign keys of the requested definition that the parent's could not adopt as the only one: two or more of them, so which one the parent adopts would be the server's choice, or one already adopted by another parent foreign key. Nothing is added to the partition |
| `partitioned-foreign-key-referenced-table` | yes | The foreign key references a table the partitioned foreign key build does not take: a partitioned table, a foreign table, a view, or a partition of the table the foreign key is added to; nothing is created |
| `not-add-foreign-key` | yes | The statement handed to the partitioned foreign key build is not one `ALTER TABLE` with a single `ADD FOREIGN KEY` subcommand |
| `partitioned-foreign-key-unsupported-shape` | yes | The partitioned foreign key statement names no constraint, or carries a clause the build cannot carry to every partition's foreign key: an unqualified referenced table, no referenced column list, an `ON DELETE` column list, `NOT VALID`, `ONLY`, a repeated column, or fewer referenced columns than referencing ones. A name is required because each partition's foreign key takes a name derived from it, and a rerun finds it by that name; nothing is created |
| `partitioned-foreign-key-parent-conflict` | yes | The partitioned table already has a constraint under the requested name that is not the requested validated foreign key, or already has the requested foreign key under another name; nothing is created |
| `partitioned-foreign-key-sub-partitioned` | yes | A partition of the target is itself partitioned; the partitioned foreign key build takes only leaf partitions directly under the target, and nothing is created |
| `partitioned-foreign-key-not-valid-supported` | yes | The server is PostgreSQL 18 or later, which accepts `ADD FOREIGN KEY ... NOT VALID` on a partitioned table; add the foreign key with the ordinary online route, `NOT VALID` then `VALIDATE CONSTRAINT`. Nothing is created |
| `partitioned-foreign-key-unsettled` | no | Partitions kept appearing without the foreign key until the build ran out of passes; every partition's validated foreign key stays, and a rerun resumes and adopts them |
| `partitioned-foreign-key-leaf-changed` | no | A partition's foreign key was dropped, replaced, or joined by another of the same definition between its leaf step and the parent's step, which rolled back before the parent's statement; every partition's foreign key stays, and a rerun decides each partition again |
| `partition-detach-pending` | yes | The partition tree holds a partition whose `DETACH PARTITION ... CONCURRENTLY` did not finish. The server leaves that partition out when it attaches an index, so the parent's index could never become valid, and out of a parent's column drop, so it would keep its copy of the column, and a table detaches one partition at a time; refused before anything is created when a partitioned index build or a partitioned key build first reads the tree, before a reviewed column drop runs, and before a concurrent detach of another of the table's partitions runs. Finish the detach with `ALTER TABLE ... DETACH PARTITION ... FINALIZE`, then rerun |
| `detach-default-partition` | yes | A `DETACH PARTITION ... CONCURRENTLY` step's table has a `DEFAULT` partition, and PostgreSQL refuses a concurrent detach there in every supported version; refused before any step runs, as an `unsupported-statement`, `by-design` refusal, in the dry run too. The only detach the table takes is the plain one, which holds `ACCESS EXCLUSIVE` on the table and the partition while it runs: submit it under the force and destructive acknowledgements, or detach the `DEFAULT` partition first |
| `partition-detach-interrupted` | no | A `DETACH PARTITION ... CONCURRENTLY` step stopped after its first transaction committed: a budget, a cancellation, or a server error ended its wait for older transactions or its second transaction. The partition is detach-pending: queries of the table no longer read it and no row is routed into its range, but it stays in `pg_inherits` (`inhdetachpending`). Running the same statement again finishes it with `DETACH PARTITION ... FINALIZE`, as does running that statement directly; the finish waits for older transactions under the lock budget too, so raise the lock budget while a transaction older than it stays open |
| `partitioned-index-tablespace` | yes | The partitioned index statement has a `TABLESPACE` clause. A partition's index is built from the definition the server prints for the parent's index, which never names a tablespace, so the clause would place the parent's index and leave every partition's index in the default tablespace; refused before anything is created |
| `partitioned-index-unsettled` | no | Partitions kept appearing without an index until the build ran out of passes; everything finished stays attached and a rerun resumes |
| `partitioned-key-name-taken` | yes | The name a partitioned table's `PRIMARY KEY` or `UNIQUE` constraint needs on a partition, for its index and its constraint, is held by something that is not the requested key: another kind of relation, an index of another definition or on another table, an index already attached or backing a constraint, or another constraint. The occupant is left for an operator |
| `partitioned-key-conflict` | yes | A partition already has a key constraint that conflicts with the requested one: two that each match it, one on the same columns of another kind or definition or `DEFERRABLE`, one already attached to a parent table's constraint, or, for a primary key, a primary key on other columns. Nothing is added to the partition |
| `partitioned-key-nullable-column` | yes | A primary key was requested over a column a partition allows to be NULL, including one whose not-null constraint is `NOT VALID` (PostgreSQL 18). Adding it would set `NOT NULL`, which scans the partition under `ACCESS EXCLUSIVE`, so nothing is built; make the columns `NOT NULL` first |
| `not-add-key-constraint` | yes | The statement handed to the partitioned key build is not one `ALTER TABLE` with a single `ADD PRIMARY KEY` or `ADD UNIQUE` subcommand |
| `partitioned-key-unsupported-shape` | yes | The partitioned key statement names no constraint, or carries a clause the build cannot carry to every partition's constraint: `INCLUDE`, `NULLS NOT DISTINCT`, `DEFERRABLE`, `INITIALLY DEFERRED`, `WITH (...)`, `USING INDEX TABLESPACE`, `USING INDEX`, `NOT VALID`, `ONLY`, or a repeated column. A name is required because each partition's index and constraint take names derived from it, and a rerun finds them by those names; nothing is created |
| `partitioned-key-parent-conflict` | yes | The partitioned table already has a constraint under the requested name that is not the requested key, a relation in its schema already holds the name the parent's index would take, or a primary key is requested and the table already has one; nothing is created |
| `partitioned-key-sub-partitioned` | yes | A partition of the target is itself partitioned; the partitioned key build takes only leaf partitions directly under the target, and nothing is created |
| `partitioned-key-unsettled` | no | Partitions kept appearing without the key constraint until the build ran out of passes; every partition's finished constraint stays, and a rerun resumes and adopts them |
| `empty-sequence` | yes | The sequence had no steps to run |
| `unsupported-sequence-step` | yes | A step is not a shape the sequence executor can run safely |
| `unsupported-partitioned-parent` | yes | Partitioned-parent admission refusal |
| `not-concurrent-index-build` | yes | The statement handed to the concurrent build executor is not a `CREATE INDEX CONCURRENTLY` |
| `unnamed-index` | yes | The concurrent build does not name its index, so its outcome could not be verified |
| `unqualified-table` | yes | The target table is not schema-qualified at the library boundary |
| `if-not-exists-unsupported` | yes | `CREATE ... IF NOT EXISTS` cannot prove what its no-op would mean |
| `create-collision` | yes | A name the create path needs is already taken on the server; re-diff the live catalog |
| `create-name-mismatch` | yes | The CREATE TABLE committed but the table does not own a first-choice constraint-index or sequence name the desired file claimed — an occupant took it inside the probe's window and the server chose a suffixed name; the table remains for an operator to rename the relation or drop, then re-diff |
| `create-names-unverified` | no | The CREATE TABLE committed but the read of the relation names the table owns did not complete — a cancelled context, a lost connection, a table no longer at its name — so whether every first-choice claim was honoured is unknown; the read's own failure is the cause in the error text, and the table remains for an operator to compare its names against the desired file, then re-diff |
| `create-owner-mismatch` | yes | The CREATE TABLE committed but the table's catalog owner is not the create owner the proof named; the table remains as committed for an operator to inspect, transfer, or drop — the executor never repairs ownership with `ALTER ... OWNER TO` |
| `create-owner-unverified` | no | The CREATE TABLE committed but the read of its catalog owner did not complete — a cancelled context, a lost connection, a table no longer at its name — so whether it was born under the named owner is unknown; the read's own failure is the cause in the error text, and the table remains for an operator to compare its owner against the configured one |
| `duplicate-create-name` | yes | The desired set claims the same relation name twice; refused at admission |
| `partition-of-unsupported` | yes | `CREATE TABLE PARTITION OF` locks the partitioned parent, which the absence proof does not cover |
| `unsupported-create-step` | yes | A desired statement is not a shape the create path can run |
| `pool-too-small` | yes | The pool cannot hold every session the operation needs at once: a build's session and verdict connection, a recovery's own session beside those, or a copy-and-swap run's copy workers plus one connection for the catch-up's flushes and one for checkpoint saves and progress reads (`schemachange.MinPoolConns`, which `migrate.CopySwapMinConns` returns), refused by `Runner.Run` and by both front doors before the run creates anything |
| `replica-identity-drop` | yes | A native drop would remove what a replica identity index is built on, read again under the table's lock after planning; nothing was dropped. Point `REPLICA IDENTITY` at a unique index the drop leaves in place (`USING INDEX`), at a primary key it leaves in place (`DEFAULT`), or at the whole row (`FULL`) first, then re-plan |
| `adopted-index-drift` | yes | The index a planned `ADD CONSTRAINT ... USING INDEX` takes over was dropped, replaced under the same name, or left invalid after planning, read again under the table's lock; nothing was attached. Re-plan against the index as it stands |
| `dropped-index-drift` | yes | The index a reviewed plan drops with `DROP INDEX CONCURRENTLY` is no longer the plain, valid index of the plan's table, with the definition the plan carried, that the plan named, read again on the session that drops it: it is gone, belongs to another table, is invalid, is a partitioned table's index or an index partition, now backs a constraint, or is another index that took over its name; nothing was dropped. Re-plan against the catalog as it stands and review the new plan |
| `dropped-index-replaced` | yes | A reviewed index drop passed its read and then dropped an index the plan did not review: another session gave the name to a different index in the round trip between the read and the drop, and the index that was read is still in the catalog. Unlike `dropped-index-drift`, something was dropped. The message names the dropped index and the checked index's OID; inspect the table's indexes and recreate the dropped one if it should stay. The check cannot see a swap that dropped the checked index as well |
| `dropped-object-drift` | yes | The column, constraint, or NOT NULL a reviewed plan drops no longer has the definition the plan carried for it, read again under the table's lock: it is gone, or its name now stands for another object (a column dropped and added again, a column whose type, collation, or default changed, a constraint replaced under its name), or the column has a dependent the plan did not list (an index, constraint, owned or identity sequence, or other object added after review, which the drop would remove with it); nothing was dropped. A listed dependent that has since gone does not refuse. Re-plan against the table as it stands and review the new plan For a reviewed `DROP TABLE` (`ExecuteDropTable`), the table's definition (columns, constraints, indexes, triggers and other objects that go with it, row security and policies, partitioning, inheritance, and the foreign keys that reference it) differs from the reviewed one, or its name now stands for a view or another kind of relation |
| `dropped-object-has-descendants` | yes | A reviewed column, constraint, or NOT NULL drop names a table that has inheritance children, read under the table's lock, or a NOT NULL drop (or a drop that says `ONLY`) names a partitioned table that has partitions. The drop would also reach those tables: their copies of the column and anything on them, their copies of the constraint, or their NOT NULL. None of that was reviewed, so nothing was dropped. A reviewed plan cannot make such a drop; make it with `migrate --alter` after reviewing what it removes from each table below. A column or constraint drop on a partitioned table whose every descendant is a declarative partition is not refused with this code: a partition's columns and inherited constraints are the parent's, and the parent's drop removes them from every partition. A reviewed `DROP TABLE` is refused the same way when the table is partitioned or has inheritance children, which the drop would remove with it |
| `dropped-column-partition-key` | yes | A reviewed column drop on a partitioned table names a column in the partition key of the table or of a sub-partitioned partition below it, as a key column or inside a key expression, read under the locks of the whole tree. PostgreSQL never drops a partition key column, so nothing was dropped. The planner refuses such a drop with `partition-key` when it plans it; this is the executor's own check, for a caller that runs the drop directly |
| `dropped-column-partition-dependent` | yes | A reviewed column drop on a partitioned table would also remove an object a partition carries of its own on its copy of the column — an index, a CHECK or foreign key, a statistics object, an owned sequence, or any other object with a dependency on the column — read under the locks of the whole tree. Copies of the parent's objects (indexes attached under the parent's, inherited or cloned constraints, cloned triggers) go with the parent's reviewed objects and do not refuse; the column's own default and NOT NULL do not either. The plan reviewed only the parent's column and its dependents, so nothing was dropped. The message names up to five objects and counts the rest; drop or change them first, then re-plan |
| `dropped-table-has-parent` | yes | A reviewed `DROP TABLE` names a partition or an inheritance child, read under the table's lock. Its rows are also rows of the table above it, so dropping it changes that table, which the plan did not review. Nothing was dropped |
| `dropped-table-has-dependents` | yes | PostgreSQL refused a reviewed `DROP TABLE` (SQLSTATE `2BP01`) because a view, another table's foreign key, or another object depends on the table. The drop never says `CASCADE`, so nothing was dropped. Remove or change the dependent objects first |
| `validated-constraint-drift` | yes | The constraint a planned `VALIDATE CONSTRAINT` validates was dropped or replaced under the same name after planning, read again under the validation's `SHARE UPDATE EXCLUSIVE` lock; nothing was validated. Re-plan against the constraint as it stands |
| `renamed-index-drift` | yes | The relation an `ALTER INDEX ... RENAME TO` names is no longer a plain index on the preflighted table, read again under the table's `SHARE UPDATE EXCLUSIVE` lock: it is gone, it is not an index (PostgreSQL's `ALTER INDEX ... RENAME` also renames a table), it belongs to another table, or it now backs a primary-key, unique, or exclusion constraint; or another session gave the name to a different relation between that check and the rename, confirmed before the commit; nothing was renamed. Re-plan against the catalog as it stands |
| `enum-type-drift` | yes | An `ALTER TYPE ... ADD VALUE` reached a type other than the enum type the engine resolved and checked: another session gave the name to a different type before the statement ran, confirmed by identity before the commit, since `ALTER TYPE ... RENAME` does not wait for the lock the statement takes; nothing was added. Plan again against the catalog as it stands |
| `enum-label-exists` | yes | The enum type the engine checked already carries the label an `ALTER TYPE ... ADD VALUE` without `IF NOT EXISTS` adds: another change added it after planning. The server's `duplicate_object` names no type, so the label is read on the checked type by OID before this code is returned; when that type lacks it, the statement reached another type and the outcome is `enum-type-drift`; when the read itself fails, the outcome is `execution-failed`. Nothing was added. Plan again against the catalog as it stands |
| `enum-neighbor-missing` | yes | The enum type the engine checked no longer carries the label an `ALTER TYPE ... ADD VALUE` places its value `BEFORE` or `AFTER`: another change renamed or removed it after planning. The server's `invalid_parameter_value` names no type, so the label is read on the checked type by OID before this code is returned; when that type carries it, the statement reached another type and the outcome is `enum-type-drift`; when the read itself fails, the outcome is `execution-failed`. Nothing was added. Plan again against the catalog as it stands |
| `table-not-found` | yes | The statement's qualified table does not exist |
| `data-fault` | yes | The server rejected the table's rows as the statement would shape them (SQLSTATE class 22 or 23). The code names the fault, not the cleanup: a concurrent index build that failed on its rows (duplicate values under a unique index, an index expression raising on a row) returns an `*InvalidIndexError` whose `Recoverable()` is false and whose `Cleanup` says what it left — its own invalid index occupies the name, so correct the rows, drop it with `DROP INDEX CONCURRENTLY`, and run the change again ([runbook](invalid-index-recovery.md#a-build-that-failed-on-its-rows-data-fault)); a partitioned table's index build that stopped on one partition's rows names that partition and the invalid index it left there when the run proved that index its own, and once the rows are corrected the same statement run again drops that index itself and resumes; the copy-and-swap run abandoned itself when the verdict says `abandoned: true` (the error is a `*schemachange.AbandonedError`; match it with `errors.As`), so correct the data or the statement and run it again from the beginning; otherwise the abandon did not finish, the hold on the source and the checkpoint row stay, and the detail says to correct the data or the statement and start the run over with `--restart SCHEMA.TABLE` |
| `invariant-violation` | yes | A breach of the invariant registry; never a retry candidate. It outranks every other code: an error that carries the breach maps here whichever typed failure wraps it, and the typed errors' own `Code()` methods agree |
| `execution-failed` | no | Fallback for a failure outside the typed set — an operational error to investigate, not a refusal to branch on |

### Create-shape causes

`executor.CreateShapeCauses()` enumerates the closed vocabulary of reasons the create
path refuses a desired statement by its shape; `executor.CreateShapeCauseOf(err)` reads
the cause off a refusal. Like outcome codes, a cause is a stable identity for automation
to branch on, never prose.

| Cause | Meaning |
| --- | --- |
| `partition-of` | Attaching a partition locks a parent the absence proof does not cover |
| `inherits` | `INHERITS` binds to an existing parent the absence proof does not cover |
| `like` | `LIKE` reads an existing source table the absence proof does not cover |
| `of-type` | `OF` binds to an existing composite type the absence proof does not cover |
| `if-not-exists` | A name-only no-op cannot prove the existing relation has the requested shape or is valid |
| `concurrently` | A table born this run needs no concurrent index build |
| `duplicate-name` | The desired set claims the same relation name twice |
| `multiple-operations` | The statement and operation parse boundaries disagree about the operation count |
| `unsupported-kind` | The statement is not a create kind the create path can run |

`concurrently`, `multiple-operations`, and `unsupported-kind` re-verify preconditions
`statement.ParseDesired` already enforces — a desired file that passed admission cannot
produce them. They are published so the vocabulary is closed, not because automation should
expect them; a consumer seeing one has a desired schema that bypassed admission.

The three cancellation codes partition one server signal, SQLSTATE `57014`,
in a fixed precedence. `budget-statement-exceeded` wins when the server's
cancel arrives at or past the overall budget, even if the caller's own
context ended at the same instant: `statement_timeout` fired at the limit
the caller sized, and reading it as the caller's cancellation would hide
the exhaustion. Below the budget, an ended caller context makes the cancel
`cancelled-by-caller` in whichever form it reached the executor — the
server's `57014` or the client's own context error. A `57014` under a live
caller context below the budget is `cancelled-externally`.
