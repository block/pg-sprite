# Use the CLI

Check DDL before it reaches a database, see the exact SQL a change will run,
and apply it without blocking the application. Start with one table; the same
commands manage every table in a schema from reviewed files.

| To do this | Go to |
|---|---|
| Try pg-sprite for the first time | [Install](../README.md#install), then [connect to a database](#connect-to-a-database) |
| Check a DDL file in CI, with no database | [Check DDL before you connect](#check-ddl-before-you-connect) |
| Find out whether pg-sprite supports a change | [Read the support matrix](#read-the-support-matrix) |
| Run an `ALTER TABLE` safely | [Run a schema change](#run-a-schema-change) |
| Change a column's type on a large table | [Rewrite a table online](#rewrite-a-table-online) |
| Keep tables in sync with `CREATE TABLE` files | [Manage tables from schema files](#manage-tables-from-schema-files) |
| Watch a long change while it runs | [Follow a running change](#follow-a-running-change) |
| Call pg-sprite from a script, a pipeline, or an agent | [Use the CLI from scripts and agents](#use-the-cli-from-scripts-and-agents) |
| Use pg-sprite with a Supabase project | [Using pg-sprite with Supabase](supabase.md) |

The examples use the `users` and `orders` tables from the demo tour's
[seed](../demo/seed.sql), on the local database `make db-up` starts
(PostgreSQL 16). Every output block was captured from a real session; long
ones are trimmed where marked. Substitute your own tables and connection.

## Connect to a database

Commands that read or change a database take a connection URL or a
`key=value` DSN, from `--url` or `PGSPRITE_URL`:

```sh
export PGSPRITE_URL='postgres://pgsprite:pgsprite@localhost:5432/pgsprite?sslmode=disable'
```

Every session pg-sprite opens runs under a bounded `lock_timeout` (3s) and
`statement_timeout` (30s); `--lock-timeout` and `--statement-timeout` change
them. For TLS, pass a CA bundle with `--ca-cert` (or `PGSPRITE_CA_CERT`): every
host in the URL is then verified against it, with no plaintext fallback.
RDS and Aurora endpoints are verified against an embedded CA bundle without
any flag.

The connecting role needs more than `SELECT` for most changes: ownership (or
membership in the owning role) for `ALTER TABLE`, and replication access for a
live table rewrite. [engine-role.md](engine-role.md) lists each grant, and a
refusal names the one that is missing. pg-sprite needs one PostgreSQL session
per connection; [session-affinity.md](session-affinity.md) says which poolers
work.

`lint`, `suggest`, `fmt`, and `capabilities` take no connection flags at all,
so they cannot reach a database by accident.

## Check DDL before you connect

### Find blocking statements

Put the statements you plan to run in a file:

```sql
CREATE INDEX orders_user_id_idx ON orders (user_id);
ALTER TABLE orders ADD CONSTRAINT orders_total_positive CHECK (total > 0);
```

Excerpt, the first of two findings:

```console
$ pg-sprite lint changes.sql
/home/dev/shop/changes.sql:1:1:
  CREATE INDEX orders_user_id_idx ON orders (user_id);

warning[blocking-idiom]:
  CREATE INDEX orders_user_id_idx — holds a blocking lock on the table for
  the whole operation — writes (and for some forms reads) wait until it
  finishes

help:
  a safer online form exists — not a semantic equivalent, and running it
  by hand forgoes the engine's execution-time guards:
  1. CREATE INDEX CONCURRENTLY orders_user_id_idx ON orders USING btree (user_id);

note:
  run each statement in its own transaction, never one block; after a
  failed CONCURRENTLY build, check pg_index.indisvalid and rebuild

docs:
  https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md#safer-idiom

lint:
  /home/dev/shop/changes.sql — 2 findings, 0 errors, 2 warnings
```

`lint` reads only the text, so it runs in any CI job. It exits 1 when a file
has error-severity findings, the statements pg-sprite would refuse; warnings
alone exit 0. Each code is listed in the [lint report contract](lint-report.md).

### Get the safer form

`suggest` maps each risky statement to the online form pg-sprite would run,
with the caveats of running it yourself. Excerpt, the second suggestion:

```console
$ pg-sprite suggest changes.sql
/home/dev/shop/changes.sql:2:1:
  ALTER TABLE orders ADD CONSTRAINT orders_total_positive CHECK (total > 0);

warning[safer-idiom]:
  ADD CONSTRAINT orders_total_positive — holds a blocking lock on the
  table for the whole operation — writes (and for some forms reads) wait
  until it finishes

help:
  a safer online form exists — not a semantic equivalent, and running it
  by hand forgoes the engine's execution-time guards:
  1. ALTER TABLE orders ADD CONSTRAINT orders_total_positive CHECK (total > 0) NOT VALID;
  2. ALTER TABLE "orders" VALIDATE CONSTRAINT "orders_total_positive";

note:
  caveats: separate-transactions, validation-scan,
  scaffold-constraint-on-failure
```

`suggest` is advisory and exits 0 on any file it can parse. The caveat codes
are explained in the [suggest report contract](suggest-report.md#caveats-caveats).

### Read the support matrix

Excerpt, the first rows:

```console
$ pg-sprite capabilities
AREA          OPERATION                     TIER BACKEND        FRONT DOORS    OWNER     STATUS
column_chang… `ADD COLUMN` (no default, a … t1   native_as_is   m:yes d:yes    —         ✅
column_chang… `ADD COLUMN` with volatile d… t2   copy_and_swap  m:ref d:ref    —         🟡
column_chang… `DROP COLUMN`                 t1   native_as_is   m:yes d:yes    —         ✅
column_chang… `ALTER COLUMN TYPE`, general… t1   copy_and_swap  m:yes d:yes    —         ✅
column_chang… `RENAME COLUMN` / `RENAME TA… t3   none           m:ref d:n/a    —         ❌
```

`m:` is `migrate` and `d:` is the declarative `diff`; `yes` runs, `ref` is
refused today. The full matrix, with the reason for every row, is
[capabilities.md](capabilities.md); `--json` prints it for automation.

## Run a schema change

### Preview the plan

```console
$ pg-sprite migrate --dry-run --alter "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)"
statement 1:
  ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);

warning[safer-idiom]:
  ADD CONSTRAINT users_email_key — holds a blocking lock on the table for
  the whole operation — writes (and for some forms reads) wait until it
  finishes

note:
  pg-sprite will run a safer online sequence instead:
  1. CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "users" ("email");
  2. ALTER TABLE "users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key";

note:
  each step commits on its own — not transactionally equivalent, and the
  sequence must not run inside a transaction block

docs:
  https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md#safer-idiom

plan:
  public.users (PostgreSQL 16.15) — 1 statement, 2 steps to run, 0 refused

dry-run:
  nothing was executed

apply:
  re-run without --dry-run
```

The dry run reads the live table, so the plan reflects its real size and
shape, and executes nothing. A plan pg-sprite would refuse exits 2, so a
pipeline can gate on the dry run without parsing it. Why the substituted
sequence is safer is worked through in [safer-sequences.md](safer-sequences.md).

### Apply it

```console
$ pg-sprite migrate --alter "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)"
executed natively
  table:     public.users
  statement: ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)
  detail:    the submitted form blocks; pg-sprite ran the safer native sequence instead — all 2 steps committed
  executed as:
    1. CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "public"."users" ("email")
    2. ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key"
```

Each step commits on its own. If a later step fails, the steps before it stay
committed and the verdict names where it stopped;
[execution-model.md](execution-model.md) says what each partial state means
and how to retry. A failed concurrent index build is covered by the
[invalid-index runbook](invalid-index-recovery.md).

### Understand a refusal

When no online form exists, nothing runs:

```console
$ pg-sprite migrate --dry-run --alter "ALTER TABLE orders ADD COLUMN ref text UNIQUE"
statement 1:
  ALTER TABLE orders ADD COLUMN ref text UNIQUE;

error[rewrite-required]:
  refused — blocks as written and no online replacement could be
  constructed; rewrite the change as separate online steps and dry-run
  each one

note[safer-idiom]:
  ADD COLUMN ref — holds a blocking lock on the table for the whole
  operation — writes (and for some forms reads) wait until it finishes

help[add-column-then-constraint]:
  add the plain column first, then build the constraint as a separate,
  named ADD CONSTRAINT with its online pattern
```

The `help` line is the fix: here, add the column, then add the constraint as
its own statement, which runs online. A refusal exits 2 and touches nothing.
Every refusal reason and what to do about it is in
[limitations.md](limitations.md); [refusal-classes.md](refusal-classes.md) is
the machine-readable version.

### Acknowledge a destructive change

A change that discards data runs only when you name its table:

```console
$ pg-sprite migrate --alter "ALTER TABLE orders DROP COLUMN note"
refused (destructive-change)
  table:     public.orders
  class:     by-design
  statement: ALTER TABLE public.orders DROP note
  detail:    the statement discards live data or structure (DROP COLUMN note); pg-sprite runs no destructive statement unless the destructive acknowledgement names its table — review what is lost, then rerun with --allow-destructive public.orders
```

Rerun with `--allow-destructive public.orders`. The value must name the
resolved table exactly, so an acknowledgement copied from another command
cannot apply here. The other acknowledgements work the same way:

- `--force SCHEMA.TABLE` runs the statement as written instead of the safer
  sequence, or past a `rewrite-required` refusal like the one above, still
  under the lock budgets and the table-size guard. It cannot lift a refusal
  where pg-sprite knows of no safe path, or a statement kind it does not
  support.
- `--accept-blocking SCHEMA.TABLE` runs a refused `DROP INDEX` or `REINDEX`
  as written under the lock budgets, and exits 3 rather than 0, since pg-sprite
  does not vouch for it as online. See [lock-budgeted-passthrough.md](lock-budgeted-passthrough.md).

## Rewrite a table online

Some changes, such as a general column retype, make PostgreSQL rewrite the
whole table under a lock that blocks reads and writes. pg-sprite routes them
to copy-and-swap: it copies the rows into a shadow table while streaming live
writes, verifies the copy, and swaps the tables under a brief lock.

```console
$ pg-sprite migrate --dry-run --alter "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
statement 1:
  ALTER TABLE orders ALTER COLUMN user_id TYPE bigint;

note[type-rewrite]:
  ALTER COLUMN user_id TYPE int8 — converting or checking every row for
  the column's type forces a full table rewrite under an exclusive lock
  that blocks reads and writes

note:
  pg-sprite will run it as a copy-and-swap: build a shadow table with the
  change applied, copy the rows into it while streaming live writes,
  verify, then swap the two under a brief lock (needs --copy-and-swap to
  open the preview route, --free-disk, and logical decoding on the server;
  --block-writes instead blocks the table's writes until the swap (offline
  mode))
```

The route is in preview, so the run must open it with `--copy-and-swap`, and
must say how much disk is free so the preflight can check the shadow fits:

```console
$ pg-sprite migrate --copy-and-swap --free-disk 10GiB --alter "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
executed via copy-and-swap
  table:     public.orders
  statement: ALTER TABLE public.orders ALTER COLUMN user_id TYPE bigint
  detail:    verified 5000 rows in 3 chunks; the retained source is dropped
```

Without `--copy-and-swap` the same command is refused with
`copy-and-swap-refused` and nothing runs. A live run needs `wal_level=logical`;
`--block-writes SCHEMA.TABLE` runs without it by blocking the table's writes
until the swap. A stopped run resumes when you run the same command again,
and `--restart SCHEMA.TABLE` starts it over. Choosing between live and
offline, what a run needs, and how to stop one are in
[copy-and-swap.md](copy-and-swap.md).

## Manage tables from schema files

Instead of writing `ALTER TABLE`, keep one `CREATE TABLE` file per table and
let pg-sprite work out the statements that make the live table match it.

### Export the live schema

```console
$ pg-sprite pull --out schema
PULLED  contacts -> /home/dev/shop/schema/contacts.sql
PULLED  orders -> /home/dev/shop/schema/orders.sql
PULLED  users -> /home/dev/shop/schema/users.sql
Summary: 3 pulled, 0 refused, 0 errors
```

```sql
CREATE TABLE "orders" (
    "id" bigint GENERATED BY DEFAULT AS IDENTITY NOT NULL,
    "user_id" bigint,
    "total" numeric(10,2),
    "note" text,
    CONSTRAINT "orders_pkey" PRIMARY KEY (id)
);

CREATE INDEX orders_total_idx ON orders USING btree (total);
```

`pull` never overwrites a file, and refuses a table it cannot render
faithfully. A `diff` against a freshly pulled file reports no changes, which
proves the baseline. [pull.md](pull.md) covers the refused shapes.
`pg-sprite fmt schema/orders.sql` prints a hand-edited file in canonical form,
without a database.

### Review the plan for an edited file

Add a column and an index to `schema/orders.sql`:

```sql
    "status" text NOT NULL DEFAULT 'new',
```

```sql
CREATE INDEX orders_user_id_idx ON orders USING btree (user_id);
```

Excerpt:

```console
$ pg-sprite diff --desired schema/orders.sql
statement 1:
  ALTER TABLE public.orders ADD COLUMN status text DEFAULT ('new'::text) NOT NULL;

note[fast-default]:
  ADD COLUMN status — the catalog stores the default (fast default); no
  table scan or rewrite

statement 2:
  CREATE INDEX orders_user_id_idx ON public.orders USING btree (user_id);

warning[safer-idiom]:
  CREATE INDEX orders_user_id_idx — holds a blocking lock on the table for
  the whole operation — writes (and for some forms reads) wait until it
  finishes

note:
  pg-sprite migrate will run a safer online sequence instead:
  1. CREATE INDEX CONCURRENTLY orders_user_id_idx ON public.orders USING btree (user_id);

plan:
  public.orders (PostgreSQL 16.15) — 2 statements, 2 steps to run, 0 refused
```

`diff` changes nothing: it builds the file in a scratch schema inside a
transaction it always rolls back, and compares that with the live table.
`--sql` prints the plan as a SQL script for review.
[execute-and-introspect.md](execute-and-introspect.md) explains why it asks
PostgreSQL rather than parsing the file itself.

### Apply the file

```console
$ pg-sprite migrate --desired schema/orders.sql
executed natively
  table:     public.orders
  statement: ALTER TABLE public.orders ADD COLUMN status text DEFAULT ('new'::text) NOT NULL
  detail:    committed within budgets (lock 3s, statement 30s): the change was effectively instant
executed natively
  table:     public.orders
  statement: CREATE INDEX orders_user_id_idx ON public.orders USING btree (user_id)
  detail:    the submitted form blocks; pg-sprite ran the safer native sequence instead — all 1 steps committed
  executed as:
    1. CREATE INDEX CONCURRENTLY orders_user_id_idx ON public.orders USING btree (user_id)
executed-natively: converged: all 2 planned statements committed and a fresh plan is empty
```

Each planned statement takes the same route `--alter` would give it, and the
run ends by planning again to confirm nothing is left. A plan that drops
something is refused: the declarative door takes no acknowledgements, so
destructive changes go through `--alter`.

- A file that also declares the table's row security policies is covered in
  [declarative-row-security.md](declarative-row-security.md).
- Extension types named without a schema, such as `citext` installed in
  `extensions`, need `--extension-schema extensions`.

## Follow a running change

`migrate` prints its verdict when the change finishes, and nothing before. To
watch a long change, open a second terminal against the same database.

### See which sessions are working

Excerpt, during a copy-and-swap of a 6-million-row `events` table:

```console
$ pg-sprite status
active pg-sprite sessions:
  pid=135 state=active wait=- running_for=00:00:05.470427 query="START_REPLICATION SLOT pgsprite_b32f988b LOGICAL 0/8FEDE448 (proto_version '1', "
  pid=136 state=active wait=- running_for=00:00:00.479193 query="INSERT INTO \"public\".\"_pgsprite_fdf61dc1827319af_new\" (\"id\", \"account_id\", \"payl"
  pid=137 state=active wait=- running_for=00:00:00.389244 query="INSERT INTO \"public\".\"_pgsprite_fdf61dc1827319af_new\" (\"id\", \"account_id\", \"payl"
  pid=128 state=active wait=IO/DataFileWrite running_for=00:00:00.314954 query="INSERT INTO \"public\".\"_pgsprite_fdf61dc1827319af_new\" (\"id\", \"account_id\", \"payl"
```

`status` lists every pg-sprite session on the connected database, from
`pg_stat_activity`, with what each is running and waiting on; `--json` prints
the same list. Here one session streams the table's writes from its
replication slot while the copy workers insert into the shadow table. A
session on a `Lock` wait event is waiting for another transaction, for at
most its lock budget (`--lock-timeout`) before that attempt gives up.

### Watch a concurrent index build

PostgreSQL reports a concurrent build's own progress. Filter its view to
pg-sprite's sessions:

```sql
SELECT p.pid, p.phase, p.blocks_done, p.blocks_total, p.tuples_done, p.tuples_total
FROM pg_stat_progress_create_index p
JOIN pg_stat_activity a USING (pid)
WHERE a.application_name = 'pg-sprite';
```

Two readings of the same build of `events_account_id_idx`:

```console
 pid |                 phase                  | blocks_done | blocks_total | tuples_done | tuples_total
-----+----------------------------------------+-------------+--------------+-------------+--------------
  96 | building index: loading tuples in tree |             |              |     2912419 |      6000000

 pid |              phase               | blocks_done | blocks_total | tuples_done | tuples_total
-----+----------------------------------+-------------+--------------+-------------+--------------
 117 | index validation: scanning index |       71942 |        77187 |           0 |            0
```

Each phase counts either blocks or tuples, so read the pair the phase is
filling. A build passes through several phases, the last of them waiting for
older transactions to finish; the
[PostgreSQL documentation](https://www.postgresql.org/docs/current/progress-reporting.html#CREATE-INDEX-PROGRESS-REPORTING)
lists them.

### Check how far a live copy-and-swap is behind

A live run holds a replication slot, and the slot keeps every byte of WAL the
run has not yet confirmed. How far it is behind is the number to watch:

```sql
SELECT slot_name,
       active,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)) AS behind
FROM pg_replication_slots
WHERE slot_name LIKE 'pgsprite_%';
```

```console
     slot_name     | active | behind
-------------------+--------+--------
 pgsprite_b32f988b | t      | 50 MB
```

`behind` grows while the copy runs and shrinks as the run catches up, before
it verifies and swaps. A slot with `active` false belongs to a stopped run: it
keeps WAL on the server until you resume the run or start it over, as
[copy-and-swap.md](copy-and-swap.md#stop-resume-and-start-over) describes.

A program that embeds pg-sprite as a Go library can poll a run's phase, step,
and row, checksum, and catch-up counters directly; that snapshot is the
[progress report contract](progress-report.md).

## Use the CLI from scripts and agents

`migrate`, `diff`, `status`, `lint`, `suggest`, and `capabilities` take
`--json`. stdout carries only the report; diagnostics go to stderr, and
`--debug` adds statement-level tracing there.

```console
$ pg-sprite migrate --dry-run --json --alter "ALTER TABLE orders ADD COLUMN ref text UNIQUE" > plan.json; echo $?
2
$ jq -c '{format_version, table, disposition}' plan.json
{"format_version":19,"table":"orders","disposition":"rewrite-required"}
```

The exit code answers the question a pipeline asks first, without parsing
anything:

| Exit | Meaning |
|---|---|
| 0 | Ran through an online-safe path, or a dry run, `diff`, or `pull` found nothing to refuse |
| 1 | Failed, or a usage or connection error; a safer sequence that stopped partway keeps its committed steps |
| 2 | Refused; nothing committed |
| 3 | Committed under `--accept-blocking`, without the online guarantee |

The full rules, including the one case where a commit's outcome is unknown,
are in the README's [exit codes](../README.md#exit-codes). Gate on non-zero.
Branch on the typed `reason`, `class`, and `code` fields, never on the text.
Each JSON shape is versioned:

- [plan-report.md](plan-report.md) for `migrate --dry-run` and `diff`
- [lint-report.md](lint-report.md) and [suggest-report.md](suggest-report.md)
- [capabilities-contract.md](capabilities-contract.md) for `capabilities`

[cli-output-examples.md](cli-output-examples.md) has one captured example of
every output, and an orchestrator that embeds the engine as a Go library should
start at [schemabot-integration.md](schemabot-integration.md).
