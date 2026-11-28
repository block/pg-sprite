# CLI output examples

Representative, real outputs for every shape the CLI produces: the plan
report for each dry-run disposition, the execution verdict, the linter,
pull, diff, and the offline capabilities matrix. Every human text rendering is display only and unpinned —
including `pull`'s, which has no JSON form yet — see the animated demos in
[demos/](demos/) for how it reads; where JSON exists it is the machine
contract. The text reports color
their diagnostic labels when stdout is a terminal (`--color=auto|always|never`;
[`NO_COLOR`](https://no-color.org) and `TERM=dumb` disable auto-detection);
the JSON and `diff --sql` outputs are never colored. All were captured
verbatim from a real session against the compose database (`make db-up`,
PostgreSQL 16) —
`$` marks the command, everything after it is the tool's output — with
this schema:

```sql
CREATE TABLE users (id bigint PRIMARY KEY, email text);
CREATE TABLE events (id bigint, created date) PARTITION BY RANGE (created);
```

`migrate` exit codes follow the four-code ladder under [Exit codes](#exit-codes):
the dry run exits 0 when every statement is executable and 2 when any would be
refused — including a target table that does not exist, so a typo'd name cannot
gate green — as defined with the
[diagnostic codes](postgres-online-ddl-reference.md#dry-run-diagnostic-codes);
CI can gate on the exit code without parsing JSON. A destructive change (`DROP
COLUMN`, `DROP CONSTRAINT`, `DROP NOT NULL`) is one of those refusals until
`--allow-destructive` names the table; the acknowledged plan still marks it
`.statements[].destructive` in the JSON report. A desired-state plan that drops
anything is refused whole in the same way, and runs only with `--allow-destructive`
and `--expect-fingerprint` together (see [Apply a reviewed
drop](cli.md#apply-a-reviewed-drop)).
Exit 2 covers every refusal cause; the exit code answers *should this
proceed*, the report's typed `reason` and disposition answer *why not*. The contract is
`migrate`'s: `lint` exits 1 when a script has error-severity findings
(warnings alone exit 0) and `lint --desired` exits 2 when it refuses a desired file, and `diff` prints the plan and exits 2 when it
contains a statement execution would refuse — a missing table is diff's
greenfield case (the plan creates the table), not a refusal, unlike the
dry run. Statement kinds `migrate` does not support (`DROP INDEX`,
`REINDEX`, `CREATE TABLE`, and anything that is not `ALTER TABLE` or
`CREATE INDEX`) emit the same refusal verdict on `--dry-run` as on apply —
a verdict, not a plan report — and exit 2. So does an unacknowledged
destructive statement, and so do statements whose routed
steps the executor refuses from their shape alone before running anything
(an unnamed or `IF NOT EXISTS` index build), or from the catalog (a
`DETACH PARTITION` beside a `DEFAULT` partition). The dry run
does not evaluate the apply's size guard or privilege preflight; those need
live facts the dry run does not gather, and the apply runs them first. So a
shape refusal on `--dry-run` is the apply's verdict when no earlier apply
check refuses first: a role without the grant dry-runs to the shape refusal
and applies to `insufficient-privileges`, both exit 2. The JSON report schema is
[plan-report.md](plan-report.md).

- [Exit codes](#exit-codes)
- [Codes used in these examples](#codes-used-in-these-examples)
- [Refusal reasons](#refusal-reasons)
- [Migrate](#migrate)
  - [Runs as written (`metadata-only`) — exit 0](#runs-as-written-metadata-only--exit-0)
  - [Safer-sequence substitution (`safer-idiom`) — exit 0](#safer-sequence-substitution-safer-idiom--exit-0)
  - [Real execution — exit 0](#real-execution--exit-0)
  - [Accepted blocking refusal (`index-statement`) — exit 3](#accepted-blocking-refusal-index-statement--exit-3)
  - [Failed mid-sequence (`execution-failed`) — exit 1](#failed-mid-sequence-execution-failed--exit-1)
  - [Data fault in a copy-and-swap run (`data-fault`) — exit 1](#data-fault-in-a-copy-and-swap-run-data-fault--exit-1)
  - [Refused: no online rewrite exists (`rewrite-required`) — exit 2](#refused-no-online-rewrite-exists-rewrite-required--exit-2)
  - [Routed to copy-and-swap (`type-rewrite`) — exit 0](#routed-to-copy-and-swap-type-rewrite--exit-0)
  - [Refused: the engine does not carry the operation yet (`backend-unavailable`) — exit 2](#refused-the-engine-does-not-carry-the-operation-yet-backend-unavailable--exit-2)
  - [Routed to the partitioned index build (`partitioned-index-build`) — exit 0](#routed-to-the-partitioned-index-build-partitioned-index-build--exit-0)
  - [Refused: target facts forbid the plan (`unsupported-partitioned-parent`) — exit 2](#refused-target-facts-forbid-the-plan-unsupported-partitioned-parent--exit-2)
  - [Refused until acknowledged: a destructive change (`destructive-change`) — exit 2](#refused-until-acknowledged-a-destructive-change-destructive-change--exit-2)
- [Lint](#lint)
  - [Blocking idioms flagged (`blocking-idiom`) — exit 0](#blocking-idioms-flagged-blocking-idiom--exit-0)
- [Pull](#pull)
  - [Export a desired-state file per table — exit 0](#export-a-desired-state-file-per-table--exit-0)
- [Diff](#diff)
  - [Converge to the desired state (`metadata-only`) — exit 0](#converge-to-the-desired-state-metadata-only--exit-0)
- [Capabilities](#capabilities)
  - [Embedded support matrix with the binary version — exit 0](#embedded-support-matrix-with-the-binary-version--exit-0)

## Exit codes

The process status is the part of the contract a shell reads without JSON. Each
code answers whether anything committed and whether the engine vouches for it
as online-safe; the section headings below name the code each example exits with.
This table is the canonical statement of the ladder; the
[README](../README.md#exit-codes) restates it in short, and a wording fix lands
here first.

| Exit | Meaning | Anything committed? | Online-safe? |
|---|---|---|---|
| 0 | Executed through an online-safe path; or a dry run, `diff`, or `pull` found nothing to refuse | yes (dry run, `diff`, `pull`: nothing runs) | yes |
| 1 | Failed: a PostgreSQL error after execution started (rolled back), or an operational or usage error before it — bad flags, an unreachable database, a mismatched `--force` or `--allow-destructive` acknowledgement | no; a safer sequence that stopped mid-flight keeps its committed prefix, named in `executed_sql`; unknown when `code` is `commit-outcome-unknown` — the failed step's `COMMIT` may have landed, so inspect the catalog before retrying | not applicable |
| 2 | Refused: no online-safe path; the verdict names the typed `reason` and `class` | no, except a `migrate --desired` plan refused partway, which keeps the statements before it, listed in `verdicts` | not applicable |
| 3 | Committed through the accepted-blocking passthrough, under bounded budgets, without an online-safety guarantee | yes | no |

A usage error exits 1 and writes the usage and the error to stderr; stdout
carries only the command's output, including under `--json`.

Refusals from every command — `migrate`, its dry run, `diff`, and `pull` — share
exit 2, so a gate branches on the status without caring which subcommand produced
it; exit 3 is `migrate`'s alone, because no other command executes DDL, and only
`migrate --accept-blocking` reaches the passthrough that produces it.
Exit 2 means nothing *committed*: an optimistic attempt that exceeded its statement
budget did run and was rolled back, and still exits 2. A gate that treats every
non-zero status as failure is fail-closed for all three non-zero codes; a caller
that deliberately permits the passthrough allows 3 explicitly. The rationale is in
[lock-budgeted-passthrough.md](lock-budgeted-passthrough.md#exit-codes).

`lint --desired` refuses with exit 2 as well. The offline `lint` exits 1 when a
script has error-severity findings (warnings alone exit 0), and `suggest` exits 0
on any script it can parse — its findings never gate; only an unreadable or
unparsable script exits 1.

Two more cases a caller must handle:

- **A lost `COMMIT` is exit 1, not 0, 2, or 3.** When the connection drops or
  the caller's context ends while a bounded attempt's `COMMIT` is in flight,
  the change may or may not have landed. Exit 0 and 3 would claim it did and
  exit 2 would claim it did not; the failed exit says only that the engine
  cannot vouch for the run, and the verdict's `code` (`commit-outcome-unknown`)
  and detail tell the operator to inspect the catalog.
- **Library callers get the same facts typed, not as a status.** An
  orchestrator that imports `pkg/executor` branches on the verdict's
  `outcome`, `errors.As` to the executor's typed errors, and
  `executor.OutcomeCode` — the exit code is the CLI's rendering of those
  facts, not a surface the library exposes. Check
  `errors.As(err, new(*executor.CommitOutcomeUnknownError))` or `OutcomeCode`
  before testing for context errors: a context cancelled during `COMMIT`
  stays in the error chain, but the outcome is unknown, not a cancellation. The typed contract is in
  [execution-model.md](execution-model.md#how-a-failure-is-reported).

Exit 3 comes from exactly one invocation shape: `migrate --accept-blocking
SCHEMA.TABLE --statement-timeout …` on a refused plain `DROP INDEX`,
`REINDEX INDEX`, or `REINDEX TABLE`, where the value names the table whose
lock you accept. Why the code is non-zero, and why a
statement-budget cancellation on that path is exit 1 rather than 2, is in
[lock-budgeted-passthrough.md](lock-budgeted-passthrough.md#exit-codes);
every code's JSON shape is below.

## Codes used in these examples

Every diagnostic in the examples below carries one of these typed codes. Each
is a one-line summary; the linked reference entry is authoritative.

| Code | Meaning |
|---|---|
| [`metadata-only`](postgres-online-ddl-reference.md#metadata-only) | A brief catalog-only change: at most a short `ACCESS EXCLUSIVE` lock, no table scan or rewrite. Runs as written. |
| [`safer-idiom`](postgres-online-ddl-reference.md#safer-idiom) | The statement blocks as written but an equivalent online sequence exists; pg-sprite substitutes it. Each step commits on its own — the sequence is not transactionally equivalent to the original. |
| [`type-rewrite`](postgres-online-ddl-reference.md#type-rewrite) | The column type change is not binary-coercible, so PostgreSQL rewrites the whole table under `ACCESS EXCLUSIVE`. Routed to the copy-and-swap backend, which builds the changed shape as a shadow table, fills and verifies it, and swaps it in under the source's name. Executes. |
| [`rewrite-required`](postgres-online-ddl-reference.md#rewrite-required) | The statement blocks as written and no online replacement could be constructed. Refused; split the change into separate online steps. The statement's `guidance` field names the typed manual path ([Guidance vocabulary](suggest-report.md#guidance-guidance)), and a refused verdict carries the same `guidance`. |
| [`backend-unavailable`](postgres-online-ddl-reference.md#backend-unavailable) | The plan routes to the copy-and-swap backend for an operation the engine does not carry yet (a volatile column default, a stored generated column, a relocation). Refused; nothing executes. |
| [`unsupported-partitioned-parent`](postgres-online-ddl-reference.md#unsupported-partitioned-parent) | The target is a partitioned parent and the statement is a shape PostgreSQL cannot run online there, such as adopting an index as a constraint. Refused. |
| [`unsupported-statement`](postgres-online-ddl-reference.md#unsupported-statement) | The planner knows no safe path for the statement (for example `SET UNLOGGED`, `CLUSTER ON`). Refused — the same typed reason the run path's refusal verdict carries. |
| [`table-not-found`](postgres-online-ddl-reference.md#table-not-found) | The target table does not exist, so classification fell back to zero facts; running without `--dry-run` would fail. The dry run exits 2 and the report carries `table_exists: false`. |
| [`destructive`](postgres-online-ddl-reference.md#destructive) | The change discards live data or structure — a dropped column, constraint, index, or `NOT NULL` (dropping a `DEFAULT` is not destructive). A warning alongside the routing decision on a plan `--allow-destructive` acknowledged; without it `migrate` refuses the statement as `destructive-change`; `DROP TABLE` never reaches classification, it refuses as `unsupported-statement`. |
| [`blocking-idiom`](lint-report.md#codes-code) | Lint-only code: the submitted form blocks readers or writers and a safer native form exists; the finding's `suggestion` carries the safer SQL when the linter can construct it. |

## Refusal reasons

Every refusal verdict (`"outcome": "refused"`, exit 2) carries exactly one of
these typed `reason` tokens — the value automation switches on; prose belongs
in `detail`. The set is closed and pinned by test (`verdict.Reasons()`).

| Reason | Meaning |
|---|---|
| `unsupported-statement` | No safe path is known for the statement — only `ALTER TABLE` and `CREATE INDEX` reach classification — or a greenfield create plan carries a shape the create path refuses (`PARTITION OF`, `INHERITS`, `LIKE`, `OF`, `IF NOT EXISTS`, or a duplicate claimed relation name; the [plan statement's `cause`](plan-report.md#causes-cause-greenfield-create-shape-refusals-only) names which — a create-shape vocabulary distinct from the verdict's budget `cause` below). These greenfield shapes refuse in the plan and are re-checked at apply. |
| `index-statement` | Index maintenance (`DROP INDEX`, `REINDEX`) has a native safe idiom (`CONCURRENTLY`) and is never attempted; the verdict's `safer_idiom` names it. |
| `not-native-safe-table-too-large` | The size guard skipped the optimistic attempt: the table exceeds the configured bound and the change is not proven catalog-only. |
| `insufficient-privileges` | The connected role lacks the access the change needs; `detail` names the missing grant for preflight checks (see [engine-role.md](engine-role.md)). RLS admission names the required table ownership or database CREATE privilege and its remedy; permission errors reported by PostgreSQL retain the server diagnostic, which may not identify an exact GRANT. |
| `unsupported-partitioned-parent` | The statement is a shape PostgreSQL cannot run online on a partitioned parent: adopting an index as a constraint, a `NOT VALID` foreign key before PostgreSQL 18, or a blocking index build. |
| `not-native-safe-budget-exceeded` | The optimistic attempt exceeded its lock or statement budget and was cancelled; the verdict's `cause` narrows which budget fired. The same `cause` field carries the partitioned-parent shape under `unsupported-partitioned-parent`; both are refusal causes, unrelated to the plan statement's create-shape `cause`. |
| `not-native-safe-rewrite-required` | The submitted form blocks and must run as a safer native sequence, but none could be constructed. |
| `backend-unavailable` | The change routes to copy-and-swap for an operation the engine does not carry yet (a volatile column default, a stored generated column, a relocation, an added column whose type rewrites); a type rewrite of an existing column (`ALTER COLUMN TYPE`) executes through the route instead. |
| `destructive-change` | The change discards live data or structure — a dropped column, constraint, index, or `NOT NULL`. `migrate --alter` runs it only when `--allow-destructive` names the table, and a refused statement that drops a column and adds it back carries the `alter-column-type-in-place` guidance; a desired-state plan that drops anything is refused whole until the run pins the reviewed plan with `--expect-fingerprint` and names its table with `--allow-destructive` ([Apply a reviewed drop](cli.md#apply-a-reviewed-drop), [execution model](execution-model.md)). |
| `plan-fingerprint-mismatch` | The plan recomputed at execution time does not carry the pinned fingerprint: the plan a reviewer approved is not the plan that would execute, so nothing runs ([execution model](execution-model.md)). |
| `declarative-foreign-key` | The desired-state plan drops a foreign key into another schema, which a desired file cannot declare. The plan still shows the statement, and `migrate --alter` runs it. |
| `copy-and-swap-refused` | The change routes to copy-and-swap and the route's own gates declined it: the run did not open the route (the route is in preview; cause `copy-and-swap-not-enabled`, class `capability-boundary`, raised before the route's preflight runs or anything is written — pass `--copy-and-swap`), a copy-and-swap run stopped on the table keeps any other statement from running (cause `copy-and-swap-run-stopped`, class `environmental` — run the stopped statement again to finish it), the preflight found a table shape or an environment it cannot run against, or a shadow operation refused mid-run to keep an invariant. The verdict's `cause` carries the route's own refusal token (`copy-and-swap-…` from the gate and the preflight, `shadow-…` from the shadow operations) and `class` its routing class; [refusal-classes.md](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause) lists every cause. A table refused with `copy-and-swap-pk-unsupported` because it has no primary key also carries `"guidance": "add-integer-primary-key"`, and its `detail` links the [steps to add one online](primary-keys.md#add-a-primary-key-online). |
| `create-collision` | The greenfield create plan's table name or a claimed index, constraint-index, or sequence name is occupied. Nothing runs; re-derive the plan against the live catalog to see what holds the name, then drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column — re-planning alone reproduces the refusal. Catalog absence checks handle existing occupants. Duplicate-name SQLSTATEs backstop races for explicit names; for server-chosen names, the probe narrows the race to the time-of-check window, and after the `CREATE TABLE` commits the executor reads the constraint-index and sequence names the table actually owns and compares them against the claimed first-choice names — a name taken inside the window makes the server pick a suffixed replacement, which surfaces as a typed `create-name-mismatch` failure at step 1 with the born table left in place for an operator to rename the relation or drop, then re-diff. |

Two codes that look like refusals are not: `create-name-mismatch` and
`create-names-unverified` are executor failure codes on a `failed` verdict (exit 1,
`failed_step` 1), because the `CREATE TABLE` has already committed when they arise.
The first means a claimed first-choice constraint-index or sequence name went to an
occupant inside the probe's window and the server suffixed it; the second means the
read of the names the table owns did not complete, so the claims are unproven, not
failed. In both the born table is left in place — compare its names against the
desired file, rename or drop, then re-diff.

## Migrate

The imperative front door: submit one DDL statement; pg-sprite classifies
it, routes it, and either runs it, substitutes a safer online sequence, or
refuses. `--dry-run --json` emits the plan report without executing.

### Runs as written (`metadata-only`) — exit 0

A safe submitted form executes unchanged: `exec_sql` is the statement
itself.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN note text' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:653b46e2647e787478db2feb7115b8fe440e392115d465278fec0cfd33892484",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD COLUMN note text",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD COLUMN note",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only",
          "proven_catalog_only": true
        }
      ],
      "exec_sql": [
        "ALTER TABLE users ADD COLUMN note text"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

### Safer-sequence substitution (`safer-idiom`) — exit 0

The submitted `ADD CONSTRAINT ... UNIQUE` blocks as written, so pg-sprite
plans the safer online sequence instead: the decision carries it in
`safer_sql`, and `exec_sql` is what `migrate` would run.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:e2b2a51466547f4469754c9296524844f868eb5a967aba1e290ed8aa7ee63996",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD CONSTRAINT users_email_key",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom",
          "safer_sql": [
            "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
            "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
          ],
          "safer_sql_execution": "autocommit-each-step"
        }
      ],
      "exec_sql": [
        "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
        "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

### Real execution — exit 0

The same statement without `--dry-run` runs the substituted sequence for
real. The verdict names what was executed and every step that committed
(exit 0):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --json
{
  "outcome": "executed-natively",
  "statement": "ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)",
  "table": "public.users",
  "detail": "the submitted form blocks; pg-sprite ran the safer native sequence instead — all 2 steps committed",
  "executed_sql": [
    "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"public\".\"users\" (\"email\")",
    "ALTER TABLE \"public\".\"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
  ]
}
```

### Accepted blocking refusal (`index-statement`) — exit 3

An operator-accepted blocking refusal has a distinct marked outcome and exit
3; exit 0 remains exclusive to online-safe execution. `--accept-blocking`
takes the schema-qualified table whose lock the operator accepts, must match
the index's owning table exactly (a mismatch executes nothing and exits 1),
and requires an explicit `--statement-timeout`; it cannot be combined with
`--force` or `--dry-run`. A `DROP INDEX` also discards the index, so it needs
`--allow-destructive` naming the same table; without it the drop is refused
with `destructive-change` (exit 2) and nothing runs. The text rendering marks
the outcome as a warning:

```text
executed without online safety (accepted blocking refusal)
  table:     public.users
  refusal:   by-design / index-statement
  statement: DROP INDEX public.users_email_idx
  safer:     DROP INDEX CONCURRENTLY
  budgets:   lock 3s, statement 10m
  allowed:   the statement's data loss was acknowledged (allow-destructive)
```

```console
$ pg-sprite migrate --alter 'DROP INDEX public.users_email_idx' --accept-blocking public.users --allow-destructive public.users --lock-timeout 3s --statement-timeout 10m --json
{
  "outcome": "executed-without-online-safety",
  "reason": "index-statement",
  "class": "by-design",
  "statement": "DROP INDEX public.users_email_idx",
  "table": "public.users",
  "safer_idiom": "DROP INDEX CONCURRENTLY",
  "allowed_destructive": true,
  "blocking_passthrough": true,
  "lock_timeout": "3s",
  "statement_timeout": "10m"
}
```

### Failed mid-sequence (`execution-failed`) — exit 1

Exit 1 means execution started and PostgreSQL refused a step, or the command
stopped before anything ran. Here the safer sequence for a `CHECK` constraint
adds it `NOT VALID`, which commits, and then fails to validate it because a
row has an upper-case email. The verdict on stdout names the failed step, the
committed prefix that remains (`executed_sql`), and the PostgreSQL error that
stopped it (`sqlstate` and `server_error`, the error's primary message). The
DETAIL line is left out, but the primary message of a data fault (SQLSTATE
class 22) can still name a row's value. stderr carries the same error
with the step it failed. The constraint now exists `NOT VALID`, so
fix the rows and then run the failed step (`failed_step_sql`) to finish. Do
not rerun the original `ADD CONSTRAINT`: the constraint already exists, so it
fails with `duplicate_object` (see the
[retry paths](execution-model.md#the-committed-prefix)):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_lower CHECK (email = lower(email))' --json
{
  "outcome": "failed",
  "code": "execution-failed",
  "sqlstate": "23514",
  "server_error": "check constraint \"users_email_lower\" of relation \"users\" is violated by some row",
  "failed_step": 2,
  "failed_step_sql": "ALTER TABLE \"public\".\"users\" VALIDATE CONSTRAINT \"users_email_lower\"",
  "statement": "ALTER TABLE public.users ADD CONSTRAINT users_email_lower CHECK (email = lower(email))",
  "table": "public.users",
  "detail": "sequence step 2 of 2 failed; the 1 committed steps' state remains — the planner sequence's partial-failure contract says how a retry resumes",
  "executed_sql": [
    "ALTER TABLE public.users ADD CONSTRAINT users_email_lower CHECK (email = lower(email)) NOT VALID"
  ]
}
```

```text
pg-sprite: error: run schema change on public.users: sequence step 2 of 2 (validate-constraint) failed; steps before it committed and their state remains: optimistic attempt: ERROR: check constraint "users_email_lower" of relation "users" is violated by some row (SQLSTATE 23514)
```

When `code` is `commit-outcome-unknown`, the failed step's `COMMIT` may have
landed; inspect the catalog before retrying.

The other exit 1 is an error before anything runs — bad flags, an unreachable
database, or an acknowledgement that names the wrong table. It prints no
verdict, even with `--json`; stdout is empty and the error is on stderr:

```console
$ pg-sprite migrate --alter 'ALTER TABLE users DROP COLUMN email' --allow-destructive public.orders
pg-sprite: error: the destructive acknowledgement must name the resolved target table "public.users", got "public.orders"; nothing was executed
$ echo $?
1
```

### Data fault in a copy-and-swap run (`data-fault`) — exit 1

A copy-and-swap run whose rows the server rejects under the new shape stops with
`code: data-fault`. Here one `orders.note` is longer than the narrowed type
holds, so a chunk's insert into the shadow fails with SQLSTATE `22001`. The same
statement would fail the same way on every resume, so the run abandons itself
before returning. It drops the shadow, drops its replication slot (or takes the
write fence off the source), and deletes the checkpoint row, and the verdict
says so with `"abandoned": true`. Fix the data or the statement, then run it
again; the run starts from the beginning. When the discard itself does not
finish, the verdict carries the same `code: data-fault` without `abandoned`,
its hold and checkpoint row remain, and the detail says so: fix the data or the
statement and rerun with `--restart SCHEMA.TABLE`. A consumer branches on the
`abandoned` field, not on the detail's wording
([execution model](execution-model.md)):

```console
$ pg-sprite migrate --alter 'ALTER TABLE orders ALTER COLUMN note TYPE varchar(32)' --copy-and-swap --free-disk 2GiB --json
{
  "outcome": "failed",
  "code": "data-fault",
  "statement": "ALTER TABLE public.orders ALTER COLUMN note TYPE varchar(32)",
  "table": "public.orders",
  "detail": "the copy-and-swap run stopped: run public.orders: copy-and-swap run on public.orders abandoned on a data fault: copy chunk [2001, 4000] of public.orders into _pgsprite_c74da4a21f58f903_new: ERROR: value too long for type character varying(32) (SQLSTATE 22001); the server rejected the rows as the statement would shape them, so the run was abandoned: the shadow is dropped, its replication slot is dropped, and the checkpoint row is deleted; correct the data or the statement, then run it again, which starts from the beginning",
  "abandoned": true
}
```

```text
pg-sprite: error: run public.orders: copy-and-swap run on public.orders abandoned on a data fault: copy chunk [2001, 4000] of public.orders into _pgsprite_c74da4a21f58f903_new: ERROR: value too long for type character varying(32) (SQLSTATE 22001)
```

A live copy-and-swap run that its replication slot's lag ceiling stopped fails
the same way, with `"code": "slot-lag-exceeded"` and a `cause` naming the
measure that crossed the ceiling: `stream-behind`, `wal-retained`, or
`wal-retained-hard-limit`. The checkpoint row and the slot stay; the detail
says how to resume, and a consumer branches on `code` and `cause`, not on the
detail's wording ([replication slots](replication-slots.md#the-slot-lag-ceiling)).

### Refused: no online rewrite exists (`rewrite-required`) — exit 2

The column and its constraint arrive in one statement, so no online
substitution can be constructed; the change must be rewritten as separate
online steps. No `exec_sql` is offered. The `guidance` field names the
typed manual path — here `add-column-then-constraint`: add the plain
column first, then build the constraint as a separate, named
`ADD CONSTRAINT` with its online pattern (see the
[suggest report's Guidance vocabulary](suggest-report.md#guidance-guidance)).

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN nickname text UNIQUE' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "rewrite-required",
  "fingerprint": "sha256:9773ff32c62b04e97bacb0ae85cf0ad528164c7baafc33db66d8adf20d7a5674",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD COLUMN nickname text UNIQUE",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "rewrite-required",
      "decisions": [
        {
          "operation": "ADD COLUMN nickname",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom"
        }
      ],
      "guidance": "add-column-then-constraint"
    }
  ]
}
```

### Routed to copy-and-swap (`type-rewrite`) — exit 0

A column type change PostgreSQL would run as a whole-table rewrite under
`ACCESS EXCLUSIVE` routes to the copy-and-swap backend and executes: the
plan's disposition is `execute`, the statement's `route` and `backend` are
`copy-and-swap`, and the decision's `reason` says why the native path was
not taken. No `exec_sql` is offered — the backend derives the shadow
table's shape by running the statement against an empty copy, so there is
no native SQL to show.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN id TYPE text' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:be3fffa9b771b7fb9cc0cb024b041caf5ea08db2d28e1860d4d81f4e8773f44e",
  "statements": [
    {
      "sql": "ALTER TABLE users ALTER COLUMN id TYPE text",
      "destructive": false,
      "route": "copy-and-swap",
      "backend": "copy-and-swap",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ALTER COLUMN id TYPE text",
          "destructive": false,
          "route": "copy-and-swap",
          "reason": "type-rewrite"
        }
      ]
    }
  ]
}
```

Without `--dry-run` the run needs `--copy-and-swap`, which opens the route for
the run (the route is in preview; without the flag the front door refuses with
`copy-and-swap-refused` / cause `copy-and-swap-not-enabled` before the route's
preflight runs or anything is written), `--free-disk` (the preflight refuses with `copy-and-swap-refused` /
cause `copy-and-swap-disk-headroom` when the shadow and its indexes would not
fit the declared space, or, for a live run, would not fit beside the slot lag
ceiling), and either logical decoding on the server or
`--block-writes SCHEMA.TABLE`, which names the table whose writes it blocks
until the swap. The verdict's outcome is distinct
from `executed-natively`, `detail` says what the verification pass
covered and what became of the retained source, and `swap_lsn` is the
server's WAL position read inside the swap transaction, under its lock,
right before it committed. A write to the table that committed below that
position is in the retained source and was carried into the new table;
writes after it exist only in the new table. Compare it with another
position as `pg_lsn`. A run that resumed after its swap had committed, or
learned of the commit from the catalog after losing its connection, has no
position from inside the swap and leaves `swap_lsn` out. The field is for
programs that line the run up with other WAL positions, so only `--json`
carries it; the text verdict does not print it. `verification` is the
verification pass's figures, for a program to read without parsing
`detail`, which states only the rows, chunks, and settled chunks: `rows` hashed, `chunks` compared,
`settled` (chunks a live pass found differing and settled key by key
against the stream), `unsettled` (chunks it could settle neither way), and
`carried` (how many keys of those unsettled chunks it left for the swap to
compare under its lock — the count, never the keys). Every figure is
present, zero included. The object is absent when the pass compared no
chunk — a run resumed after its swap had committed makes no pass, and an
empty table gives it nothing to compare — and on every other outcome; the
text verdict does not print it either. A non-zero `unsettled` or `carried`
is a record, not a warning: the swap compared those keys under its lock and
would have refused had any differed. A pg-sprite build older than the
object never writes it, so a program that must tell "no pass" from "not
reported" records the binary's version beside the verdict:

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN id TYPE text' --copy-and-swap --free-disk 20GiB --json
{
  "outcome": "executed-via-copy-and-swap",
  "statement": "ALTER TABLE users ALTER COLUMN id TYPE text",
  "table": "public.users",
  "detail": "verified 1000 rows in 2 chunks; the retained source is dropped",
  "swap_lsn": "0/1A2B3C8",
  "verification": {
    "rows": 1000,
    "chunks": 2,
    "settled": 0,
    "unsettled": 0,
    "carried": 0
  }
}
```

With `--block-writes SCHEMA.TABLE`, the run fences writes to the table instead of streaming
them through a replication slot, needs no logical decoding, and `detail` says writes to the
table were blocked. When to choose it, and an example of its output, are in
[copy-and-swap.md](copy-and-swap.md#live-or-offline).

### Refused: the engine does not carry the operation yet (`backend-unavailable`) — exit 2

Other rewrite-class operations also route to copy-and-swap, but the engine
does not carry them yet — a volatile column default, a stored generated
column, a relocation — so the plan is `unavailable` and nothing executes.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN joined timestamptz DEFAULT clock_timestamp()' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "unavailable",
  "fingerprint": "sha256:f94d3738f00b2f2bb7f5c05e72720c1e650c191e8a6e79f9aa5087b9491928cf",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD COLUMN joined timestamptz DEFAULT clock_timestamp()",
      "destructive": false,
      "route": "copy-and-swap",
      "backend": "copy-and-swap",
      "disposition": "unavailable",
      "decisions": [
        {
          "operation": "ADD COLUMN joined",
          "destructive": false,
          "route": "copy-and-swap",
          "reason": "volatile-default"
        }
      ]
    }
  ]
}
```

### Routed to the partitioned index build (`partitioned-index-build`) — exit 0

PostgreSQL cannot build an index concurrently on a partitioned table, so a
`CREATE INDEX` on one is not substituted with a concurrent build. The one step
is the statement itself, and its `execution` says the run builds the index
partition by partition. The partitions are not in the plan: the run lists them
when it starts.

```console
$ pg-sprite migrate --alter 'CREATE INDEX events_created_idx ON events (created)' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "events",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:03e7dbb585c7fbbc683daf2ecc6bce704b6a83f95755e04b38f2128ee9831219",
  "statements": [
    {
      "sql": "CREATE INDEX events_created_idx ON events USING btree (created)",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "CREATE INDEX events_created_idx",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom",
          "safer_sql": [
            "CREATE INDEX events_created_idx ON events USING btree (created)"
          ],
          "safer_sql_execution": "partitioned-index-build"
        }
      ],
      "exec_sql": [
        "CREATE INDEX events_created_idx ON events USING btree (created)"
      ],
      "execution": "partitioned-index-build"
    }
  ]
}
```

The text dry-run says the same in words:

```console
$ pg-sprite migrate --alter 'CREATE INDEX events_created_idx ON events (created)' --dry-run
statement 1:
  CREATE INDEX events_created_idx ON events USING btree (created);

warning[safer-idiom]:
  CREATE INDEX events_created_idx — holds a blocking lock on the table for
  the whole operation — writes (and for some forms reads) wait until it
  finishes

note:
  pg-sprite will build the index partition by partition instead of running
  the statement: create the parent's index with ON ONLY, build each
  partition's index concurrently and attach it, and the parent's index
  turns valid with the last attach

note:
  the partitions are listed when the run starts, each step commits on its
  own, and an interrupted run is resumed by running the same statement
  again

docs:
  https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md#safer-idiom

plan:
  public.events (PostgreSQL 16.14) — 1 statement, 1 step to run, 0 refused

dry-run:
  nothing was executed

apply:
  re-run without --dry-run
```

Without `--dry-run` the run builds the index. The verdict names how many of
the table's partitions the run gave an index; running the same statement again
after an interruption adopts the indexes already built and gives an index only
to the partitions still missing one (exit 0):

```console
$ pg-sprite migrate --alter 'CREATE INDEX events_created_idx ON events (created)' --json
{
  "outcome": "executed-natively",
  "statement": "CREATE INDEX events_created_idx ON public.events USING btree (created)",
  "table": "public.events",
  "detail": "the table is partitioned, so pg-sprite built index events_created_idx partition by partition and attached each one: 2 partitions, 2 given an index by this run; the index is valid"
}
```

### Refused: target facts forbid the plan (`unsupported-partitioned-parent`) — exit 2

PostgreSQL does not support adopting an existing index as a constraint on a
partitioned parent, so the plan is refused before anything runs. The refusal
cause is the report-level `reason`.

```console
$ pg-sprite migrate --alter 'ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_id_idx' --dry-run --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "events",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "refuse",
  "reason": "unsupported-partitioned-parent",
  "class": "by-design",
  "fingerprint": "sha256:1fa77b44b9b9180b6dbc54391c3d3c32d93deddba6399a51a58f1cc4c289dd6e",
  "statements": [
    {
      "sql": "ALTER TABLE events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_id_idx",
      "destructive": false,
      "route": "native",
      "disposition": "refuse",
      "reason": "unsupported-partitioned-parent",
      "class": "by-design",
      "blocking_passthrough_eligible": false,
      "decisions": [
        {
          "operation": "ADD CONSTRAINT events_pkey",
          "destructive": false,
          "route": "native",
          "reason": "online-idiom"
        }
      ]
    }
  ]
}
```

### Refused until acknowledged: a destructive change (`destructive-change`) — exit 2

A `DROP COLUMN` discards every value the column holds, so `migrate` refuses it
before any route runs it — and the dry run ends in that same refusal verdict,
not a plan report — until `--allow-destructive` names the table. The verdict's
`reason` and `class` are the ones desired-state execution uses for a
destructive plan; on `migrate --alter` that reason always means the
acknowledgement would lift it. `DROP CONSTRAINT` and `ALTER COLUMN ... DROP NOT
NULL` are refused the same way. A statement that drops a column and adds it back
under the same name also carries `"guidance": "alter-column-type-in-place"`:
change the column with `ALTER COLUMN ... TYPE ... USING ...` to keep its values.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users DROP COLUMN email' --dry-run --json
{
  "outcome": "refused",
  "reason": "destructive-change",
  "class": "by-design",
  "statement": "ALTER TABLE public.users DROP email",
  "table": "public.users",
  "detail": "the statement discards live data or structure (DROP COLUMN email); pg-sprite runs no destructive statement unless the destructive acknowledgement names its table — review what is lost, then rerun with --allow-destructive public.users"
}
```

With the acknowledgement the dry run reports the plan the acknowledged apply
takes — still flagged `destructive` for a reviewer or orchestrator to gate on —
and exits 0; the apply's verdict then carries `"allowed_destructive": true`,
the machine-readable audit record, and the run logs one warning naming the
table. A mismatched acknowledgement executes nothing and exits 1.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users DROP COLUMN email' --dry-run --allow-destructive public.users --json
{
  "format_version": 31,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:77caa837ce1750b23eaaf9f988ede933f624c8c4c6de1dc2fee7780bf64a4c1f",
  "statements": [
    {
      "sql": "ALTER TABLE users DROP email",
      "destructive": true,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "DROP COLUMN email",
          "destructive": true,
          "route": "native",
          "reason": "metadata-only",
          "proven_catalog_only": true
        }
      ],
      "exec_sql": [
        "ALTER TABLE users DROP email"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

### Refused until acknowledged: a desired file that drops structure (`destructive-change`) — exit 2

A desired file that no longer has the `note` column makes a destructive plan.
`migrate --desired --dry-run --json` ends in the refusal, and its `fingerprint`
covers the refused plan, which cannot be pinned. `pinnable_fingerprint` is the
plan the run executes once the table is acknowledged: pass it to
`--expect-fingerprint`. Each destructive statement carries `drops`, the live
object it removes, and the fingerprint covers it.

```console
$ pg-sprite migrate --desired orders.sql --dry-run --json
{
  "format_version": 31,
  "source": "diff",
  "schema": "public",
  "table": "orders",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "refuse",
  "reason": "destructive-change",
  "class": "by-design",
  "fingerprint": "sha256:c13fbf2570e6fe10022386a4067d84e479865db6401d3c51933b6989de44f156",
  "pinnable_fingerprint": "sha256:95c0640054358fac2b09dd0e73fd530e09ec2c13f1e0d39823d53d441e09933e",
  "statements": [
    {
      "sql": "ALTER TABLE public.orders DROP note",
      "kind": "drop-column",
      "drops": {
        "kind": "column",
        "name": "note",
        "definition": "attnum 3: text"
      },
      "destructive": true,
      "route": "native",
      "disposition": "refuse",
      "reason": "destructive-change",
      "class": "by-design",
      "blocking_passthrough_eligible": false,
      "decisions": [
        {
          "operation": "DROP COLUMN note",
          "destructive": true,
          "route": "native",
          "reason": "metadata-only",
          "proven_catalog_only": true
        }
      ]
    }
  ]
}
```

A dry run compares `--expect-fingerprint` as the run does. A pin that is not the
plan the run would execute ends in `plan-fingerprint-mismatch`, with the
previewed plan in `plan` (shortened here):

```console
$ pg-sprite migrate --desired orders.sql --dry-run --allow-destructive public.orders \
    --expect-fingerprint sha256:0000000000000000000000000000000000000000000000000000000000000000 --json
{
  "plan": { "…": "the plan the run would execute" },
  "outcome": "refused",
  "reason": "plan-fingerprint-mismatch",
  "class": "environmental",
  "detail": "the plan derived at execution time (fingerprint sha256:95c0640054358fac2b09dd0e73fd530e09ec2c13f1e0d39823d53d441e09933e) is not the pinned plan (fingerprint sha256:0000000000000000000000000000000000000000000000000000000000000000); the live table or the desired schema changed since the plan was reviewed — re-review the new plan"
}
```

## Lint

The offline linter needs no database: it flags blocking idioms in a DDL
file (or stdin) with the safer form suggested for each finding (schema:
[lint-report.md](lint-report.md)). Given:

```sql
-- /tmp/changes.sql
CREATE INDEX users_email_idx ON users (email);
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
```

### Blocking idioms flagged (`blocking-idiom`) — exit 0

```console
$ pg-sprite lint /tmp/changes.sql --json
{
  "format_version": 10,
  "postgres_versions": "14-18",
  "findings": [
    {
      "statement": 1,
      "line": 1,
      "column": 1,
      "sql": "CREATE INDEX users_email_idx ON users (email)",
      "operation": "CREATE INDEX users_email_idx",
      "code": "blocking-idiom",
      "severity": "warning",
      "reason": "safer-idiom",
      "suggestion": [
        "CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email)"
      ],
      "suggestion_execution": "autocommit-each-step"
    },
    {
      "statement": 2,
      "line": 2,
      "column": 1,
      "sql": "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
      "operation": "ADD CONSTRAINT users_email_key",
      "code": "blocking-idiom",
      "severity": "warning",
      "reason": "safer-idiom",
      "suggestion": [
        "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
        "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
      ],
      "suggestion_execution": "autocommit-each-step"
    }
  ],
  "errors": 0,
  "warnings": 2
}
```

### A desired file refused offline (`lint --desired`) — exit 2

`lint --desired` checks a desired file instead of a script: it runs the file
admission `diff` and `migrate --desired` share before they connect, with no
database. A file it refuses prints the same refusal verdict and exits 2; an
admitted file is clean and exits 0. Planning can still refuse what admission
does not inspect: a table shape built on the server, or a difference from the
live table.

```sql
-- /tmp/events.sql
CREATE TABLE events (id bigint NOT NULL, at date NOT NULL) PARTITION BY RANGE (at);
CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
```

```console
$ pg-sprite lint --desired /tmp/events.sql --json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "class": "capability-boundary",
  "statement": "CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
  "detail": "a desired file holds exactly one CREATE TABLE, and the declarative model does not plan partitions; nothing was planned or executed"
}
```

## Pull

`pull` exports every renderable table in a schema as a separate desired-state
file. It currently emits a text-only per-table report rather than JSON. This
output was captured from the compose database after loading `demo/seed.sql`:

### Export a desired-state file per table — exit 0

```console
$ pg-sprite pull --schema public --out /tmp/pg-sprite-pull-example
PULLED  contacts -> /tmp/pg-sprite-pull-example/contacts.sql
PULLED  orders -> /tmp/pg-sprite-pull-example/orders.sql
PULLED  users -> /tmp/pg-sprite-pull-example/users.sql
Summary: 3 pulled, 0 refused, 0 errors
```

See [pull.md](pull.md) for the refusal and exit-code contract and the
zero-change `diff` verification loop.

## Diff

The declarative front door: point `diff` at a reviewed desired-state file
and it emits the classified statements that converge the live table onto
it, as the same plan report (`"source": "diff"`). Given the live `users`
table above (with the unique constraint from the execution example) and:

```sql
-- /tmp/users.sql
CREATE TABLE users (
    id bigint PRIMARY KEY,
    email text,
    nickname text,
    CONSTRAINT users_email_key UNIQUE (email)
);
```

### Converge to the desired state (`metadata-only`) — exit 0

```console
$ pg-sprite diff --desired /tmp/users.sql --json
{
  "format_version": 31,
  "source": "diff",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:4c349e89a66fed63dd07f693dae62e356695f2a586306cfd5a153e6c1efcd9f9",
  "statements": [
    {
      "sql": "ALTER TABLE public.users ADD COLUMN nickname text",
      "kind": "add-column",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD COLUMN nickname",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "ALTER TABLE public.users ADD COLUMN nickname text"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

### Refused before planning: a file that is not one table or one enum type (`unsupported-statement`) — exit 2

A desired file holds one `CREATE TABLE`, its indexes, and its row security,
or one `CREATE TYPE ... AS ENUM`. A file that holds anything else is refused
before `diff` connects or plans, with the refusal verdict instead of a plan
report. The class comes from the refused statement's kind, as the imperative
front door would class it: a domain goes to the operator who runs catalog
work, a data change to the data change runner. A second table, or a table in
an enum type file, is refused by design, and partitions of a table as a
capability boundary. `migrate --desired` refuses the same files the same way.

```sql
-- /tmp/account_email.sql
CREATE DOMAIN account_email AS text;
```

```console
$ pg-sprite diff --desired /tmp/account_email.sql --json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "class": "no-online-safety-problem",
  "owner": "direct-operator",
  "statement": "CREATE DOMAIN account_email AS text",
  "detail": "a desired file holds one CREATE TABLE, its indexes, and its row security, or one enum type, so this statement is not allowed in it; nothing was planned or executed"
}
```

### Refused: a difference the diff does not converge (`unsupported-statement`) — exit 2

Some differences between a live table and its desired file are compared but
never converged: storage parameters, persistence, a column's collation, an
identity or its sequence, a generated column. `diff` refuses the file with
the refusal verdict instead of a plan report, classed by what differs. A
tuned storage parameter is the owner's to change directly, so it names
`direct-operator`; a persistence or collation change needs a rewrite the
planner does not construct, so it is a capability boundary.
`migrate --desired` refuses the same file the same way, and nothing runs.

```sql
-- live: CREATE TABLE app.accounts (...) WITH (fillfactor = 70);
-- /tmp/accounts.sql
CREATE TABLE accounts (
  id bigint PRIMARY KEY,
  status text NOT NULL
);
```

```console
$ pg-sprite diff --schema app --desired /tmp/accounts.sql --json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "class": "no-online-safety-problem",
  "owner": "direct-operator",
  "statement": "CREATE TABLE accounts (id bigint PRIMARY KEY, status text NOT NULL)",
  "detail": "table storage parameters differ between live (fillfactor=70) and desired (none); declare the live parameters in the desired file: unsupported schema change; nothing was planned or executed"
}
```

## Capabilities

`capabilities` needs no database: it prints the embedded support matrix
that generates [capabilities.md](capabilities.md), stamped with the same
release version `pg-sprite --version` reports, so a consumer can pin a
build and query the matrix with `jq` (recipes in
[capabilities-contract.md](capabilities-contract.md#cli-json-and-query-recipes)).
The text form is a display-only table; the JSON is the contract. The
output is long, so this example shows its opening lines:

### Embedded support matrix with the binary version — exit 0

```console
$ pg-sprite capabilities --json | head
{
  "version": "dev",
  "capabilities": [
    {
      "id": "add-column-no-default-or-constant-default",
      "area": "column_changes",
      "operation": "`ADD COLUMN` (no default, a constant default, or a non-volatile default such as `now()`)",
      "tier": "t1",
      "status_mark": "✅",
      "engine_path": "native_as_is",
```
