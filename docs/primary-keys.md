# Choosing a primary key

**Give every table a single `bigint` primary key.** pg-sprite runs most schema changes on any
table, but the ones that rewrite a table run through
[copy-and-swap](copy-and-swap.md), which copies rows in ranges of one integer primary key.
A table without that key keeps every native change and loses only copy-and-swap. This page
covers the key to choose for a new table, what PostgreSQL makes the key cost, and how to add
a key to a table that has none.

Copy-and-swap is a preview route: a run opens it with `--copy-and-swap`. The key is necessary
for it but not sufficient. The preflight also refuses a table with foreign keys in either
direction, triggers, dependent views, partitions, or the other shapes
[What each run needs](copy-and-swap.md#what-each-run-needs) lists. The console output on this
page was captured on PostgreSQL 16.

## Contents

- [The default for a new table](#the-default-for-a-new-table)
- [What pg-sprite needs from the key](#what-pg-sprite-needs-from-the-key)
- [What the key costs in PostgreSQL](#what-the-key-costs-in-postgresql)
- [Tables without a primary key](#tables-without-a-primary-key)
- [Add a primary key online](#add-a-primary-key-online)
- [Tables with a `uuid`, text, composite, or deferrable key](#tables-with-a-uuid-text-composite-or-deferrable-key)

## The default for a new table

Use a `bigint` identity column as the primary key, and keep the application's public token
or UUID in a separate column with a `UNIQUE` constraint. The public identifier stays the same
while the key the table is copied by stays compact and ordered:

```sql
CREATE TABLE public.customers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id uuid NOT NULL UNIQUE,
    name text NOT NULL
);
```

Start at `bigint` rather than `integer`. Widening an `integer` key later is a full table
rewrite. Copy-and-swap can do it online only when no other table's foreign key references the
key, which is rare for a key worth widening. A key that starts at `bigint` never needs it.

A natural or composite key can still be the right model for a table, for example a join
table keyed on its two references. Weigh what the table needs online before choosing one:
that table runs native changes as normal, and its rewrites are refused.

## What pg-sprite needs from the key

| Route | What it needs from the key |
|---|---|
| Native, as-is (`ADD COLUMN`, a fast default, `SET DEFAULT`, …) | Nothing: no key needed |
| Native, safer sequence (`CREATE INDEX CONCURRENTLY`, the four-step `SET NOT NULL`, `ADD PRIMARY KEY` / `ADD UNIQUE` via `USING INDEX`, …) | Nothing: no key needed |
| Copy-and-swap, live or offline (`--block-writes`) | Exactly one primary-key column of type `smallint`, `integer`, or `bigint`, not `DEFERRABLE`, with replica identity `DEFAULT` or `FULL` |

Copy-and-swap needs that key for three things: the copier cuts the table into ranges of it,
a stopped run resumes from a position in it, and a live run's catch-up merges the captured
changes into one entry per key. A `NOT NULL` `UNIQUE` column does not stand in for a primary
key. The full design is
[D4 in copy-and-swap-design.md](copy-and-swap-design.md#d4--restrict-the-chunk-key-to-one-integer-family-primary-key).

## What the key costs in PostgreSQL

PostgreSQL stores rows in a heap, not in key order, and secondary indexes point at the row's
physical location rather than at its key. So a wide key does not repeat in every index the
way it does in MySQL's InnoDB. The width still costs space in the key's own index and in
every foreign key that references it, and in the indexes on those referencing columns.

Insert order matters more. A random key, such as a version 4 UUID, sends each insert to a
random page of the key's index, so more pages are read, split, and written in full to WAL
after each checkpoint. An increasing key keeps inserts at the right edge of the index. A
time-ordered UUID (version 7; `uuidv7()` from PostgreSQL 18) restores that order for a UUID,
but it is still not a key copy-and-swap can range over.

## Tables without a primary key

Native changes run on a keyless table exactly as on any other:

```console
$ pg-sprite migrate --alter 'CREATE INDEX events_kind_idx ON public.events (kind)'
executed natively
  table:     public.events
  statement: CREATE INDEX events_kind_idx ON public.events (kind)
  detail:    the submitted form blocks; pg-sprite ran the safer native sequence instead — all 1 steps committed
  executed as:
    1. CREATE INDEX CONCURRENTLY events_kind_idx ON public.events USING btree (kind)
```

A change that needs copy-and-swap is refused with cause `copy-and-swap-pk-unsupported`
(exit 2), before the run creates a shadow table, a replication slot, or a publication:

```console
$ pg-sprite migrate --copy-and-swap --free-disk 1GiB --json \
    --alter 'ALTER TABLE public.events ALTER COLUMN amount TYPE bigint'
{
  "outcome": "refused",
  "reason": "copy-and-swap-refused",
  "class": "capability-boundary",
  "cause": "copy-and-swap-pk-unsupported",
  "statement": "ALTER TABLE public.events ALTER COLUMN amount TYPE bigint",
  "table": "public.events",
  "detail": "copy-and-swap refuses the table shape (copy-and-swap-pk-unsupported): the primary key has 0 columns; exactly one is required; to run it through copy-and-swap, add a single-column integer primary key online — over a column that already identifies each row, or over a new bigint column filled from a sequence — then run the change again; the steps are at https://github.com/block/pg-sprite/blob/main/docs/primary-keys.md#add-a-primary-key-online (guidance add-integer-primary-key)",
  "guidance": "add-integer-primary-key"
}
```

The `guidance` field is the typed pointer to the next section, for a host that does not
show `detail`. A table that has a key of the wrong kind gets no `guidance`, because adding a
key does not apply to it ([below](#tables-with-a-uuid-text-composite-or-deferrable-key)).

Refusing before the publication matters on a keyless table: under the default replica
identity, a table published for updates without a key fails every `UPDATE` and `DELETE`
the application sends. `--block-writes` does not help either, because the offline run copies
by the same key.

You have three ways forward:

- **Add a primary key first**, as the next section shows, then run the change. This is the
  usual answer: the key costs one concurrent index build, and a later rewrite can then run
  through copy-and-swap if the table passes the route's other shape checks.
- **For a small table, force the rewrite in place.** `--force SCHEMA.TABLE` runs the
  statement as written under the lock and statement budgets (3s and 30s unless you change
  them), holding `ACCESS EXCLUSIVE` for the whole rewrite. Reads and writes wait for it, and a
  table too large to rewrite within the statement budget fails and rolls back. A forced run is
  still size-guarded: a table larger than `--max-table-size` (1 GiB by default) is refused
  `not-native-safe-table-too-large` before anything runs, until you raise the threshold:

  ```console
  $ pg-sprite migrate --force public.events --alter 'ALTER TABLE public.events ALTER COLUMN amount TYPE bigint'
  executed natively
    table:     public.events
    statement: ALTER TABLE public.events ALTER COLUMN amount TYPE bigint
    detail:    forced: the submitted form ran as-is under budgets (lock 3s, statement 30s), overriding the engine's routing; the statistics of amount were refreshed after the type change
    forced:    the submitted form ran as-is (force acknowledged)
  ```

- **For a large table you cannot key, run the change yourself** in a window where the table
  can be locked for as long as the rewrite takes.

## Add a primary key online

**When a column already identifies each row**, add the key over it. pg-sprite runs
`ADD PRIMARY KEY` as a concurrent unique index build and then attaches the index as the key.
Make the column `NOT NULL` first: attaching makes the key's columns `NOT NULL`, and on a
column that allows nulls, PostgreSQL checks every row while it holds the attach's
`ACCESS EXCLUSIVE` lock. pg-sprite runs `SET NOT NULL` as a four-step sequence that checks the
rows under a weaker lock, so the attach then has nothing left to check. If the column still
holds a null, the attach fails with SQLSTATE `23502` and the valid unique index from the
first step stays. Running the same statement again then fails at that first step, because
the index already exists. Instead, fix the nulls, make the column `NOT NULL`, and attach the
index that is already there:
`ALTER TABLE public.events ADD CONSTRAINT events_pkey PRIMARY KEY USING INDEX events_pkey`.
[execution-model.md](execution-model.md) has the retry path for each sequence's partial
failure. The `SET NOT NULL` output is shown in step 4 below:

```console
$ pg-sprite migrate --alter 'ALTER TABLE public.events ALTER COLUMN id SET NOT NULL'
$ pg-sprite migrate --alter 'ALTER TABLE public.events ADD CONSTRAINT events_pkey PRIMARY KEY (id)'
executed natively
  table:     public.events
  statement: ALTER TABLE public.events ADD CONSTRAINT events_pkey PRIMARY KEY (id)
  detail:    the submitted form blocks; pg-sprite ran the safer native sequence instead — all 2 steps committed
  executed as:
    1. CREATE UNIQUE INDEX CONCURRENTLY "events_pkey" ON "public"."events" ("id")
    2. ALTER TABLE "public"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey"
```

**When no column identifies each row**, add one, fill it, then key on it. The example
keys `public.audit_log` on a new `id`. The statements run through `pg-sprite migrate` are
those it can bound; the sequence and the backfill run directly against the database:

1. Create a sequence for the new column. This takes no lock on the table:

   ```sql
   CREATE SEQUENCE public.audit_log_id_seq AS bigint;
   ```

2. Add the column, with no default, and then the default. Both are catalog changes:

   ```console
   $ pg-sprite migrate --alter 'ALTER TABLE public.audit_log ADD COLUMN id bigint'
   executed natively
     table:     public.audit_log
     statement: ALTER TABLE public.audit_log ADD COLUMN id bigint
     detail:    committed within budgets (lock 3s, statement 30s): the change was effectively instant
   $ pg-sprite migrate --alter "ALTER TABLE public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq')"
   executed natively
     table:     public.audit_log
     statement: ALTER TABLE public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq')
     detail:    committed within budgets (lock 3s, statement 30s): the change was effectively instant
   ```

   New rows now get an `id`. Adding the column with the default in one statement would
   instead be a rewrite with a volatile default, which pg-sprite refuses.

3. Fill the existing rows in small batches. First build a partial index over the rows still
   to fill. Without it, each batch scans the heap from the start past every row already
   filled, so the backfill slows quadratically on a large table. With it, each batch reads
   only the remaining rows, and the index shrinks as they are filled:

   ```sql
   CREATE INDEX CONCURRENTLY audit_log_id_backfill_idx ON public.audit_log (id) WHERE id IS NULL;
   ```

   Then run the batch repeatedly, each in its own transaction, until it updates no row:

   ```sql
   UPDATE public.audit_log SET id = nextval('public.audit_log_id_seq')
   WHERE ctid IN (SELECT ctid FROM public.audit_log WHERE id IS NULL LIMIT 10000);
   ```

   `LIMIT` bounds the rows a batch touches, not how long it runs or waits. A batch that meets a
   row another transaction has locked waits for it, so run each batch in its own transaction
   under a short `lock_timeout` and `statement_timeout`, and retry the one that times out.
   Each batch writes WAL for every row it updates. Pause between batches and watch replica
   and slot lag (`pg_stat_replication`, `pg_replication_slots`) on a server with replicas or
   subscribers.

   When the batch updates no row, drop the partial index:

   ```sql
   DROP INDEX CONCURRENTLY public.audit_log_id_backfill_idx;
   ```

   Every update writes a new version of the row, so after the backfill the table holds up to
   twice its rows until vacuum reclaims the dead ones. Run `VACUUM public.audit_log` (or let
   autovacuum catch up) before you size `--free-disk` for the copy-and-swap that follows.

4. Make the column `NOT NULL`, then make it the key:

   ```console
   $ pg-sprite migrate --alter 'ALTER TABLE public.audit_log ALTER COLUMN id SET NOT NULL'
   executed natively
     table:     public.audit_log
     statement: ALTER TABLE public.audit_log ALTER COLUMN id SET NOT NULL
     detail:    the submitted form blocks; pg-sprite ran the safer native sequence instead — all 4 steps committed
     executed as:
       1. ALTER TABLE "public"."audit_log" ADD CONSTRAINT "audit_log_id_not_null_check" CHECK ("id" IS NOT NULL) NOT VALID
       2. ALTER TABLE "public"."audit_log" VALIDATE CONSTRAINT "audit_log_id_not_null_check"
       3. ALTER TABLE "public"."audit_log" ALTER COLUMN "id" SET NOT NULL
       4. ALTER TABLE "public"."audit_log" DROP CONSTRAINT "audit_log_id_not_null_check"
   $ pg-sprite migrate --alter 'ALTER TABLE public.audit_log ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id)'
   ```

   Name the constraint as shown to get the usual `<table>_pkey`. pg-sprite names the index it
   builds for an unnamed `ADD PRIMARY KEY (id)`, and the key takes that name,
   `audit_log_id_pkey`; plain PostgreSQL would name it `audit_log_pkey`.

5. Tie the sequence to the column, so the sequence is dropped with the column, and `pull`
   exports the column as `bigserial` and `diff` compares it that way. `pull` refuses a table
   whose default calls a sequence the column does not own, and it recognizes the serial form
   only by the serial-style name `<table>_<column>_seq`, which is why step 1 uses that name.
   Copy-and-swap carries the sequence either way. `pg-sprite migrate` takes only `ALTER TABLE`
   and index statements, so run this directly:

   ```sql
   ALTER SEQUENCE public.audit_log_id_seq OWNED BY public.audit_log.id;
   ```

The column then behaves like a `bigserial` key, and a rewrite runs through copy-and-swap when
the table passes the route's other shape checks:

```console
$ pg-sprite migrate --copy-and-swap --free-disk 2GiB \
    --alter 'ALTER TABLE public.audit_log ALTER COLUMN note TYPE varchar(64)'
executed via copy-and-swap
  table:     public.audit_log
  statement: ALTER TABLE public.audit_log ALTER COLUMN note TYPE varchar(64)
  detail:    verified 1000 rows in 2 chunks; the retained source is dropped
```

Adding the key as an identity column instead (`ADD COLUMN id bigint GENERATED ALWAYS AS
IDENTITY`) fills every row in one rewrite of the table, and pg-sprite refuses it with
`backend-unavailable`.

## Tables with a `uuid`, text, composite, or deferrable key

These tables run native changes as normal. Copy-and-swap refuses them with the same cause,
and the detail says what about the key it cannot use:

```text
copy-and-swap refuses the table shape (copy-and-swap-pk-unsupported): primary-key column id has type uuid; smallint, integer, or bigint is required
copy-and-swap refuses the table shape (copy-and-swap-pk-unsupported): the primary key has 2 columns; exactly one is required
copy-and-swap refuses the table shape (copy-and-swap-pk-unsupported): the primary key is DEFERRABLE; a non-deferrable key is required
```

The add-a-key recipes above do not apply: a table has only one primary key. pg-sprite has no
online route today that replaces a key or makes a `DEFERRABLE` one non-deferrable. Both drop
the existing constraint, and every foreign key that references it has to move with it.
Reserve that for a table whose rewrites you need online. Otherwise, run its rare rewrites in
a maintenance window. For a new table, use the [default above](#the-default-for-a-new-table)
and keep the UUID as a unique column.
