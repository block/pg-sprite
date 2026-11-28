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

Every session pg-sprite opens runs under a bounded `lock_timeout` (3s) and,
for ordinary statements, a `statement_timeout` (30s); `--lock-timeout` and
`--statement-timeout` change them. The two long steps have budgets of their
own, which `--statement-timeout` does not extend: a concurrent index build
(`--index-build-timeout`, 30m) and a `VALIDATE CONSTRAINT`
(`--validate-timeout`, 30m). For TLS, pass a CA bundle with `--ca-cert` (or
`PGSPRITE_CA_CERT`): every host in the URL is then verified against it, with no plaintext fallback.
RDS and Aurora endpoints are verified against an embedded CA bundle without
any flag.

The connecting role needs more than `SELECT` for most changes: ownership (or
membership in the owning role) for `ALTER TABLE`, and replication access for a
live table rewrite. [engine-role.md](engine-role.md) lists each grant, and a
refusal names the one that is missing. pg-sprite needs one PostgreSQL session
per connection; [session-affinity.md](session-affinity.md) says which poolers
work.

A `search_path` that lists `pg_catalog` after a user schema has that entry
removed, so the schema no longer shadows the catalog; every other entry is left
as configured. The connection pool holds at least six connections unless the URL
sets `pool_max_conns`: one for each of a copy-and-swap run's four copy workers,
and two more so its catch-up's flushes and its checkpoint saves do not wait
behind the copy. A `pool_max_conns` in the URL is kept as given, and a
copy-and-swap run refuses a pool below six before it creates anything
(`code: pool-too-small`).

Only `migrate` without `--dry-run` ever commits a schema change. Two other
commands write to the database, and neither touches table data: `reap` without
`--dry-run` drops only the engine's orphan replication slots and their
publications, and `discard` drops what a stopped copy-and-swap run left on one
table (its shadow table, write fence or replication slot and publication, and
checkpoint row).

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
alone exit 0. A desired schema file is not a script: check it with
`lint --desired` (see [Manage tables from schema files](#manage-tables-from-schema-files)). Each code is listed in the [lint report contract](lint-report.md).

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
AREA          OPERATION                     TIER MATURITY BACKEND        FRONT DOORS    OWNER     STATUS
column_chang… `ADD COLUMN` (no default, a … t1   ga       native_as_is   m:yes d:yes    —         ✅
column_chang… `ADD COLUMN` with volatile d… t2   —        copy_and_swap  m:ref d:ref    —         🟡
column_chang… `DROP COLUMN`                 t1   ga       native_as_is   m:yes d:yes    —         ✅
column_chang… `ALTER COLUMN TYPE`, general… t1   preview  copy_and_swap  m:yes d:yes    —         ✅
column_chang… `RENAME COLUMN` / `RENAME TA… t3   —        none           m:ref d:n/a    —         ❌
```

`m:` is `migrate` and `d:` is the declarative `diff`; `yes` runs, `ref` is
refused today. `MATURITY` is set on supported rows only: `ga` is generally
available, and `preview` runs only when the run opts in, such as
`--copy-and-swap`. The full matrix, with the reason for every row, is
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
  does not vouch for it as online. It also needs an explicit
  `--statement-timeout` longer than `--lock-timeout`, and a `DROP INDEX` also
  needs `--allow-destructive` naming the same table:

  ```sh
  pg-sprite migrate --alter 'DROP INDEX public.users_email_idx' \
    --accept-blocking public.users --allow-destructive public.users \
    --lock-timeout 3s --statement-timeout 10m
  ```

  With `--desired` it lifts one refusal only, a reviewed plan's drop of a
  partitioned table's index, and needs `--expect-fingerprint` and
  `--allow-destructive` too, all naming the desired file's table.

  See [lock-budgeted-passthrough.md](lock-budgeted-passthrough.md).

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
`--restart SCHEMA.TABLE` starts it over, and
[`discard SCHEMA.TABLE`](#abandon-a-stopped-copy-and-swap-run) abandons it
without running the change. A run whose table you changed by hand since it
started — a column or an index added, say — cannot resume: its cutover stops
with `cutover-schema-drift`, or running the same command again is refused with
`resume-source-changed` (exit 2), and the detail names the shadow, the slot or
fence, and the checkpoint row the run leaves, and both ways past them:

```console
$ pg-sprite migrate --copy-and-swap --free-disk 10GiB --alter "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
refused (copy-and-swap-refused)
  table:     public.orders
  class:     environmental
  statement: ALTER TABLE public.orders ALTER COLUMN user_id TYPE bigint
  detail:    public.orders changed since this run built its shadow (for example a column or index added by hand); the stopped run cannot resume, and it leaves shadow table public._pgsprite_c74da4a21f58f903_new, replication slot pgsprite_a725d037, which retains WAL on the server until it is dropped, publication pgsprite_a725d037, and the checkpoint row in phase verifying; start over by running the same pg-sprite migrate command with --restart public.orders, or abandon the run without running the change with pg-sprite discard public.orders
```

Choosing between live and offline, what a run needs, and how to stop one are in
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
without a database. `pg-sprite lint --desired schema/orders.sql` runs, also
without a database, the file admission `diff` and `migrate --desired` share: a
file it refuses, such as a domain or partitions file, prints the same refusal
verdict and exits 2. Passing it does not mean planning will succeed: `diff`
can still refuse a table shape or a difference from the live table that `lint`
does not inspect.

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

fingerprint:
  sha256:6f0c1d52e9b3a7a48d1e5c0b92f4a7d3e8c6b1a09d2f7e4c3b5a8d6e1f0c9b2a
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
run ends by planning again to confirm nothing is left.

- A file that also declares the table's row security policies is covered in
  [declarative-row-security.md](declarative-row-security.md).
- A file can declare one enum type instead of a table. The type must already
  exist; `diff` plans an `ALTER TYPE ... ADD VALUE` for each label the file
  lists that the live type lacks, `BEFORE` the next live label when the file
  inserts it ahead of one, and `migrate --desired` runs them. A file that
  drops, renames, or reorders a live label is refused, since PostgreSQL cannot
  remove or move one.
- Extension types named without a schema, such as `citext` installed in
  `extensions`, need `--extension-schema extensions`.

### Apply a reviewed drop

A plan that drops live structure (a column, a constraint, an index, or a
`NOT NULL`) is refused whole, and nothing runs, until you review it and say so
twice: pin the plan you reviewed, and name the table. Here the file no longer
has the `note` column or the `orders_user_id_idx` index.

```console
$ pg-sprite diff --desired schema/orders.sql
statement 1:
  DROP INDEX public.orders_user_id_idx;

warning[destructive]:
  this change discards live data or structure

note:
  pg-sprite migrate will run a safer online sequence instead:
  1. DROP INDEX CONCURRENTLY public.orders_user_id_idx;

statement 2:
  ALTER TABLE public.orders DROP note;

warning[destructive]:
  this change discards live data or structure

plan:
  public.orders (PostgreSQL 16.15) — 2 statements, 2 steps to run, 0 refused

fingerprint:
  sha256:6f50cb50959a807f68554bb5529b93b96c4a770fb5e5e5e62f61e024dbeade4f
```

`migrate --desired` without the flags refuses with `destructive-change` and
prints the two flags to add. Read the plan, then rerun with its fingerprint and
the table:

```console
$ pg-sprite migrate --desired schema/orders.sql \
    --expect-fingerprint sha256:6f50cb50959a807f68554bb5529b93b96c4a770fb5e5e5e62f61e024dbeade4f \
    --allow-destructive public.orders
executed natively
  table:     public.orders
  statement: DROP INDEX public.orders_user_id_idx
  detail:    dropped with DROP INDEX CONCURRENTLY in 0s: public.orders was not locked against reads or writes for the drop
  allowed:   the statement's data loss was acknowledged (allow-destructive)
  executed as:
    1. DROP INDEX CONCURRENTLY public.orders_user_id_idx
executed natively
  table:     public.orders
  statement: ALTER TABLE public.orders DROP note
  detail:    committed within budgets (lock 3s, statement 30s): the change was effectively instant
  allowed:   the statement's data loss was acknowledged (allow-destructive)
executed-natively: converged: all 2 planned statements committed and a fresh plan is empty
```

A dry run checks the pin the same way, so a stale pin is caught before the
run. Here the `note` column was dropped and added back as `varchar(40)` after
the review. The name is the same, the definition is not, so the plan is a
different one:

```console
$ pg-sprite migrate --desired schema/orders.sql --dry-run \
    --expect-fingerprint sha256:6f50cb50959a807f68554bb5529b93b96c4a770fb5e5e5e62f61e024dbeade4f \
    --allow-destructive public.orders
refused (plan-fingerprint-mismatch)
  class:  environmental
  detail: the plan derived at execution time (fingerprint sha256:aba27004d3d8aa2a04b9dfa1141603305833dfdbda03ab9ba2c5a92c45c39a83) is not the pinned plan (fingerprint sha256:6f50cb50959a807f68554bb5529b93b96c4a770fb5e5e5e62f61e024dbeade4f); the live table or the desired schema changed since the plan was reviewed — re-review the new plan
```

- `--allow-destructive` is accepted with `--desired` only together with
  `--expect-fingerprint`; without it the command fails before it connects. The
  run refuses, and executes nothing, when the plan derived at run time has a
  different fingerprint (`plan-fingerprint-mismatch`): the table or the file
  changed since the review, so review the new plan.
- The acknowledgement lifts only the plan's destructive statements. Every
  statement except an index drop still takes the route, preflight, and budgets
  `--alter` would give it. A planned index drop runs as
  `DROP INDEX CONCURRENTLY` under its own gates instead: the pin, the
  acknowledgement, the audit, the verdict, and a re-read of the index's
  definition right before the drop. It skips `--alter`'s size guard, privilege
  preflight, and partition check.
- The fingerprint covers what each drop removes, not only its name, so a column
  dropped and added back with another type, or an index replaced under the same
  name, is a different plan and the run refuses with `plan-fingerprint-mismatch`.
- An index drop never blocks the table. Immediately before each drop, the run
  reads the live object again and refuses, dropping nothing, if it no longer
  matches the plan (`dropped-index-drift`, `dropped-object-drift`). The drop
  of a changed index's old definition also reads the index built to replace
  it and refuses if it is gone, invalid, or rebuilt with another definition
  (`replacement-index-drift`); see
  [execution-model.md](execution-model.md) for these codes and for
  `dropped-index-replaced`.
- A `--allow-destructive` table that is not the resolved table is a usage
  error in a dry run as well, whatever the plan holds, before the pin is
  compared or anything is printed.
- The plan runs in order, and a drop comes before an add. A change that
  replaces a unique constraint or index leaves a window without the old
  guarantee. To add first and drop after, run the add and the drop as two
  `--alter` statements.
- A plan that fails part-way keeps the statements that already committed. Run
  `diff --desired` again to review what is left.
- `--dry-run` prints the fingerprint to pin and executes nothing; with both
  flags it previews the plan the run would execute. Some plans stay refused
  with both flags, such as a drop beside a copy-and-swap statement, or a drop
  of a partitioned table's index, which a run also needs `--accept-blocking`
  for and a dry run cannot carry. For those, the dry run prints the refusal
  the pinned run gives instead of the flags. In `--json` output on a
  destructive plan, `fingerprint` covers the refused plan and cannot be
  pinned: pin `pinnable_fingerprint`, the same value the text output prints.
  A dry run compares `--expect-fingerprint` as the run does and refuses with
  `plan-fingerprint-mismatch` on a difference, for a table, a row security
  file, an enum type file, and a non-destructive plan alike.
- A policy a row security file no longer lists is dropped without this
  acknowledgement, and that change takes neither flag. The flags apply to a
  change to the table definition alone
  ([declarative-row-security.md](declarative-row-security.md)). A file that
  declares an enum type takes `--expect-fingerprint` but not
  `--allow-destructive`, since a label cannot be dropped.

## Follow a running change

`migrate` prints its verdict on stdout when the change finishes. While it runs,
`--progress` controls a progress line on stderr showing the step, operation,
percent, and ETA:

| `--progress` | What stderr shows |
|---|---|
| `auto` (default) | The line, redrawn in place, only when stderr is a terminal (`TERM=dumb` turns it off) |
| `always` | The line on any stderr; when stderr is not a terminal, such as a log file, a plain line per change of step, operation, or ten-percent mark |
| `never` | No progress line |

`--progress=always` cannot be combined with `--json` or `--dry-run`: the
machine-readable run shows no progress line, and a dry run executes nothing.
stdout carries only the verdict in every mode.

To see what the sessions themselves are doing, open a second terminal against
the same database.

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

### Drop orphan slots

A slot no checkpoint row in a non-terminal phase names is an orphan: a run
that ended before its row landed, or one whose cleanup did not finish. It
keeps WAL like any other slot. `reap` drops the orphans of the connected
database, publications included; `--dry-run` shows what it would drop first:

```console
$ pg-sprite reap --dry-run
would drop (orphans, with any publication of the engine's shape): 1
  pgsprite_deadbeef
live (kept: a checkpoint row in a non-terminal phase names it): 1
  pgsprite_cafebabe
active (kept: a process holds it): 0
foreign (kept: wears the pgsprite_ prefix without the engine's name shape or output plugin): 1
  pgsprite_backup
dry run: nothing was dropped
```

A slot a live row names, a slot a process holds, and a `pgsprite_` slot the
engine did not make (another name shape, or an output plugin other than
`pgoutput`) are kept; slots of other databases are never read. The pass is
safe while a schema change runs: it reads each orphan's row again right
before the drop, under the lock a run takes to claim its slot. A dry run that
would drop opens one replication connection, as each drop does, so a role
without the `REPLICATION` attribute or a URL that cannot open one fails the
dry run with exit 1, after the report.

The slot is dropped before its publication, so a slot whose same-named
publication `reap` will not drop (one that is not the single-table
publication pg-sprite makes, such as a `FOR ALL TABLES` one) is gone: the
pass stops there, lists it last under `dropped`, as `failed`, and under
`publication left` (`publication_left` in JSON), exits 1, and the error
says why the publication of that name was left in place for you to look
at.

A database without the checkpoint table has no row that can show a slot is
an orphan, so `reap` examines no slot there, drops nothing, and exits 0. A
`pgsprite_` slot in such a database still keeps WAL; check
`pg_replication_slots` for one.

`--json` prints the [reap report](#reap-report).

#### Reap report

`reap --json` prints one object, `format_version` 2. A consumer must reject a
version it does not know; the version is bumped whenever a field is added,
removed, or changes meaning.

| Field | Meaning |
|---|---|
| `format_version` | The report's shape, 2 |
| `dry_run` | True for `--dry-run` |
| `checkpoint_table_exists` | False when the database has no checkpoint table: no slot was examined and every list is empty |
| `stopped` | True when the pass failed at a slot; the lists hold what was sorted before it, and the command exits 1 |
| `failed` | The slot the pass stopped at, `""` when it did not stop. Its slot may already be gone: the drop removes the slot before its publication, and a slot the drop reports gone with its publication left is also the last entry of `dropped` and the entry of `publication_left` |
| `dropped` | Orphans the run dropped, publications included except for a slot in `publication_left`; empty in a dry run |
| `publication_left` | The slot of `dropped` whose same-named publication the drop refused or failed to drop, left in place; at most the `failed` slot, empty in a dry run |
| `would_drop` | Orphans a real run would drop now; empty in a real run |
| `live` | Kept: a checkpoint row in a non-terminal phase names each |
| `active` | Kept: a process holds each |
| `foreign` | Kept: wears the `pgsprite_` prefix without the engine's name shape or output plugin |

Every list is always an array, never `null`. stdout is empty, with exit 1,
only when the pass failed before it reached any slot, such as a connection
or catalog read failure.

### Abandon a stopped copy-and-swap run

A stopped copy-and-swap run keeps its shadow table, its hold on the table (the
write fence of an offline run, or the replication slot and publication of a
live one, which keeps WAL), and its checkpoint row until you resume it or
start it over. When you decide not to make the change at all, or the volume
is filling and you cannot run it now, `discard` drops all of it without
running the change:

```console
$ pg-sprite discard public.orders
discarded the stopped copy-and-swap run on public.orders; its change was not run
  dropped shadow table public._pgsprite_c74da4a21f58f903_new
  dropped replication slot pgsprite_a725d037
  dropped publication pgsprite_a725d037
  deleted the checkpoint row (phase catching_up)
```

Name the table exactly as `--restart` does: schema-qualified, an unquoted
name in lower case. The table keeps its shape and its rows; an offline run's
table takes writes again. `discard` drops in the order a restart does (the
shadow, then the fence or the slot, then the row), so a discard that fails
partway leaves a row that still records what stands; it prints what it
dropped, exits 1, and a second `discard` finishes the job. One stop names a
leftover it could not drop: when no replication slot of the run's name
remains but the publication of that name was not dropped, the report names
it under `publication_left`. A second `discard` drops it if the first
failed for a passing reason; one that leaves it again has met a publication
that is not of the engine's shape (it publishes other tables, say), which
`discard` never drops: check that nothing uses it, drop it by hand with
`DROP PUBLICATION`, and run `discard` again. Dropping a live
run's slot waits for a server process still holding it, such as the stream
of a run whose host died, which the server ends after `wal_sender_timeout`;
the whole discard gives up after five minutes.

It is refused, with exit 2 and nothing dropped, when:

- a run on the table is in progress (`table-lock-held`): stop it with Ctrl-C
  or `SIGTERM` first;
- the run's swap already committed (`restart-swap-committed`): the change is
  live, so run the same `migrate` command again to finish it;
- the change completed (`resume-row-terminal`);
- the checkpoint row names a slot other than the table's own
  (`resume-row-mismatch`);
- the role lacks the copy-and-swap grants (`insufficient-privileges`),
  including replication access when the run holds a slot: the `detail`
  names the grant that gives it, so grant it, or connect as the role that
  ran the change, and run `discard` again;
- the table no longer has a shape copy-and-swap takes (the preflight's own
  cause), such as a user trigger, a foreign key, or a dependent view added
  since the run stopped: `discard` proves the table's shape as a run does,
  so it cannot discard this run. Remove the run by hand instead, as the
  [integration guide](schemabot-integration.md#stopping-resuming-and-starting-over)
  describes under "Discarding without running the change": delete the
  table's checkpoint row, drop the shadow and any write fence, and then
  `reap` drops the slot no row names, or drop it by hand as
  [replication-slots.md](replication-slots.md#free-the-wal-resume-drop-the-slot-or-start-over)
  shows.

With no checkpoint row, `discard` still drops a shadow table, a write fence,
and a slot and publication under the table's derived names, the slot only as
`reap` would drop it: a slot another table's run owns (`resume-slot-foreign`)
and a slot a process holds (`slot-active`) are left. The shadow and the fence
go first, so when either was dropped the outcome is `stopped` with that cause
and exit 1; otherwise it is `refused` with exit 2.

A table with no stopped run and nothing under its run's names reports
`nothing to discard` and exits 0. `discard` creates the checkpoint table if
the database lacks it, as a copy-and-swap run does. It has no `--dry-run`:
what it would drop depends on reads it makes under the table's lock, and the
report names everything it dropped. `--json` prints the
[discard report](#discard-report).

#### Discard report

`discard --json` prints one object, `format_version` 1. A consumer must reject
a version it does not know; the version is bumped whenever a field is added,
removed, or changes meaning.

| Field | Meaning |
|---|---|
| `format_version` | The report's shape, 1 |
| `table` | The table as given |
| `outcome` | `discarded`, `nothing-to-discard`, `refused` (exit 2, nothing dropped), or `stopped` (exit 1, the fields below name what was dropped before the failure) |
| `cause` | The refusal cause of a `refused` or `stopped` discard; `""` otherwise, and for a stop that was not a refusal. It is one of the causes the command names, `table-lock-held`, `slot-active`, and `insufficient-privileges`; a copy-and-swap shape cause from the preflight ([`CopySwapRefusalCause`](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause)); or a shadow operation cause from the engine ([`RefusalCause`](refusal-classes.md#shadow-operation-refusals-keyed-on-refusalcause)), of which a discard meets `restart-swap-committed`, `resume-row-terminal`, `resume-row-mismatch`, and `resume-slot-foreign` as above; `shadow-foreign-relation` and `shadow-owner-mismatch` when a relation under the shadow's name, or the table itself, is not the one the engine's run made or proved; and `shadow-lock-lost`, `shadow-lock-held-elsewhere`, and `shadow-lock-unconfirmed` when the table's lock is lost or cannot be confirmed mid-discard. A script that meets a cause it does not know treats it as a refusal to read by hand |
| `detail` | The error behind a `refused` or `stopped` discard, `""` otherwise |
| `row_phase` | The phase of the checkpoint row deleted, `""` when none was |
| `shadow` | The shadow table dropped, in the table's schema, `""` when none was |
| `slot` | The replication slot dropped, `""` when none was |
| `publication` | The publication dropped, `""` when none was |
| `write_fence` | True when the write fence was taken off the table |
| `publication_left` | On a `stopped` discard, the publication left in place after no replication slot of its name remained (the slot is gone, whether this discard dropped it or it was gone already); `""` otherwise |

stdout is empty, with exit 1, when the command failed before it dropped
anything and was not refused, such as a connection failure or a table name
no table bears.

A program that embeds pg-sprite as a Go library can poll a run's phase, step,
and row, checksum, and catch-up counters directly; that snapshot is the
[progress report contract](progress-report.md).

## Use the CLI from scripts and agents

`migrate`, `diff`, `status`, `reap`, `discard`, `lint`, `suggest`, and
`capabilities` take `--json`. stdout carries only the report; diagnostics go to stderr, and
`--debug` adds statement-level tracing there.

```console
$ pg-sprite migrate --dry-run --json --alter "ALTER TABLE orders ADD COLUMN ref text UNIQUE" > plan.json; echo $?
2
$ jq -c '{format_version, table, disposition}' plan.json
{"format_version": 32,"table":"orders","disposition":"rewrite-required"}
```

The exit code answers the question a pipeline asks first, without parsing
anything:

| Exit | Meaning |
|---|---|
| 0 | Ran through an online-safe path, or a dry run, `diff`, or `pull` found nothing to refuse |
| 1 | Failed, or a usage or connection error; a safer sequence that stopped partway keeps its committed steps |
| 2 | Refused; nothing committed, except a `migrate --desired` plan refused partway, which keeps the statements before the refused one (their verdicts are in `verdicts`); plan again before retrying |
| 3 | Committed under `--accept-blocking`, without the online guarantee |

The full rules, including the one case where a commit's outcome is unknown,
are in the README's [exit codes](../README.md#exit-codes). Gate on non-zero.
Branch on the typed `reason`, `class`, and `code` fields, never on the text.
Each JSON shape is versioned:

- [plan-report.md](plan-report.md) for `migrate --dry-run` and `diff`
- [lint-report.md](lint-report.md) and [suggest-report.md](suggest-report.md)
- [capabilities-contract.md](capabilities-contract.md) for `capabilities`
- [Reap report](#reap-report) above for `reap`
- [Discard report](#discard-report) above for `discard`

[cli-output-examples.md](cli-output-examples.md) has one captured example of
every output, and an orchestrator that embeds the engine as a Go library should
start at [schemabot-integration.md](schemabot-integration.md).
