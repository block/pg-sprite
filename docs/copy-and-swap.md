# Running a copy-and-swap

Some schema changes cannot run in place without blocking the table, most commonly a column
type change such as `integer` to `numeric(12,2)`. pg-sprite routes these to **copy-and-swap**:
it builds the new shape as a shadow table, copies the rows into it in chunks, verifies the
copy, and swaps it in under the table's name with a short, bounded lock.

This page is the operator's guide: the two ways to run it, how to choose, what each needs,
and how to stop, resume, and start over. The design is in
[copy-and-swap-design.md](copy-and-swap-design.md); every refusal is in
[limitations.md](limitations.md) and [refusal-classes.md](refusal-classes.md).

## Contents

- [Live or quiesced](#live-or-quiesced)
- [What each run needs](#what-each-run-needs)
- [Free disk space](#free-disk-space)
- [Run it](#run-it)
- [Stop, resume, and start over](#stop-resume-and-start-over)

## Live or quiesced

| | Live (default) | Quiesced (`--quiesced`) |
| --- | --- | --- |
| Application writes during the run | Keep working. pg-sprite reads them from the WAL through a logical replication slot and replays them onto the shadow. | Refused. pg-sprite fences the table with a trigger, and every write fails with SQLSTATE `55006` until the swap commits. |
| Server requirements | `wal_level = logical`, a free replication slot, and two free WAL senders | None beyond the copy itself |
| Holds on the server while it runs | A replication slot, which keeps WAL until pg-sprite consumes it | A trigger on the table |
| Run time | Copy, then catch up with the writes made meanwhile, verify, swap | Copy, verify, swap |

**Choose live** for a table the application writes to in normal operation, on a server with
logical decoding enabled.

**Choose quiesced** when writes to the table are already stopped or can be stopped for the run:

- archive, history, or reference tables nothing writes to;
- a maintenance window, or a feature flag that pauses the table's writer;
- a server without logical decoding. On Aurora and RDS that needs
  `rds.logical_replication = 1`, a parameter that takes a reboot.

`--quiesced` is your declaration that nothing writes the table. pg-sprite does not take it on
trust: the fence refuses any write that does arrive, so nothing is lost, but the application
sees errors for the whole run. The fence and why it also covers replication apply workers are
described in [engine-role.md](engine-role.md#the-write-fence).

## What each run needs

Both:

- a table shape copy-and-swap carries: a usable primary key, and none of the shapes the
  [copy-and-swap row in limitations.md](limitations.md) lists, such as foreign keys,
  triggers, dependent views, or partitioning. The preflight refuses anything else with a
  typed `copy-and-swap-refused` cause before it builds anything;
- the privileges in [engine-role.md](engine-role.md) for tier 3;
- `--free-disk`, described next.

Live runs also need logical decoding. Without it, a live run is refused before anything is built:

```console
$ pg-sprite migrate --free-disk 1GiB --json \
    --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
{
  "outcome": "refused",
  "reason": "copy-and-swap-refused",
  "class": "environmental",
  "cause": "copy-and-swap-logical-decoding-unavailable",
  "statement": "ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)",
  "table": "public.order_archive",
  "detail": "copy-and-swap refuses the environment (copy-and-swap-logical-decoding-unavailable): wal_level is replica; set wal_level = logical (a restart-only setting)"
}
```

Either enable logical decoding, or run with `--quiesced` if the table can take no writes.

## Free disk space

The shadow table is a full second copy of the table and its indexes, so the preflight requires
free space of at least twice the table's heap, indexes, and TOAST. PostgreSQL cannot report the
free space on its own volume over SQL, so you pass it with `--free-disk` (binary units: `MiB`,
`GiB`, `TiB`). A run without it is refused with cause `copy-and-swap-disk-headroom`.

- **Self-managed PostgreSQL:** run `df -h` on the volume holding the data directory.
- **Amazon RDS for PostgreSQL:** read the instance's `FreeStorageSpace` metric in CloudWatch.
- **Aurora PostgreSQL:** you do not need to measure anything. The cluster volume grows
  automatically, so the shadow cannot fill it. Pass any value of at least twice the table's
  size.

We would like to remove this flag, by reading the free space where the platform exposes it and
skipping the check where storage grows on its own. Until then it stays required.

## Run it

```sh
export PGSPRITE_URL='postgres://app_owner:…@db-primary.example.com:5432/shop?sslmode=verify-full'

# Preview: route the statement and print the plan, run nothing
pg-sprite migrate --dry-run \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Live: the application keeps writing
pg-sprite migrate --free-disk 50GiB \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Quiesced: nothing writes the table for the length of the run
pg-sprite migrate --free-disk 50GiB --quiesced \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

Point `PGSPRITE_URL` directly at the primary, not at a connection pooler.

A successful run prints one verdict, `executed-via-copy-and-swap`, and exits 0. With `--json`:

```json
{
  "outcome": "executed-via-copy-and-swap",
  "statement": "ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)",
  "table": "public.order_archive",
  "detail": "verified 5000 rows in 3 chunks; the retained source is dropped; the table was quiesced: writes were fenced for the length of the run"
}
```

The old table is dropped once the swap commits. Add `--keep-old` to keep it under its `_old`
name for a rollback window; you drop it yourself when the window ends.

## Stop, resume, and start over

| You want to | Do this |
| --- | --- |
| Stop a running change | Press Ctrl-C, or send `SIGTERM`, once. The run saves where its copy reached and exits. The table keeps its fence (quiesced) or its replication slot (live) until you resume or start over. |
| Resume it | Run the same command again. It continues from where it stopped; nothing is copied twice. |
| Start over | Run the same command with `--restart SCHEMA.TABLE`, naming the table exactly. pg-sprite discards the shadow, the fence or slot, and the saved progress, then runs from the beginning. |
| Recover from bad data | Nothing to discard: when the server rejects a row under the new type or a constraint, the run abandons itself and reports `code: data-fault`. Fix the data or the statement and run it again. |

```sh
pg-sprite migrate --free-disk 50GiB --quiesced --restart public.order_archive \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

A stopped live run keeps its replication slot, and the slot keeps WAL on the server. Resume it
or start it over promptly rather than leaving it.
