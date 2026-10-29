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

- [Live or offline](#live-or-offline)
- [What each run needs](#what-each-run-needs)
- [Free disk space](#free-disk-space)
- [Run it](#run-it)
- [Stop, resume, and start over](#stop-resume-and-start-over)

## Live or offline

| | Live (default) | Offline (`--block-writes SCHEMA.TABLE`) |
| --- | --- | --- |
| Application writes during the run | Keep working. pg-sprite reads them from the WAL through a logical replication slot and replays them onto the shadow. | Refused. pg-sprite fences the table with a trigger, and every write fails with SQLSTATE `55006` until the swap commits. |
| Server requirements | `wal_level = logical`, a free replication slot, and two free WAL senders | None beyond the copy itself |
| Holds on the server while it runs | A replication slot, which keeps WAL until pg-sprite consumes it | A trigger on the table |
| Run time | Copy, then catch up with the writes made meanwhile, verify, swap | Copy, verify, swap |

**Choose live** for a table the application writes to in normal operation, on a server with
logical decoding enabled.

**Choose offline** when writes to the table are already stopped or can be stopped for the run:

- archive, history, or reference tables nothing writes to;
- a maintenance window, or a feature flag that pauses the table's writer;
- a server without logical decoding. On Aurora and RDS that needs
  `rds.logical_replication = 1`, a parameter that takes a reboot.

`--block-writes` is your declaration that nothing needs to write the table. Its value must name
the table the statement changes, exactly as `SCHEMA.TABLE`; a value naming any other table
stops the run before anything executes, so a flag left in a script cannot take the next
table offline. It applies to `migrate --alter` only: `migrate --desired` rejects it, because
there the derived plan, not you, decides how many statements take a copy and so how long
writes fail. Run each statement with `migrate --alter --block-writes` instead. pg-sprite does not take the declaration on trust: the fence refuses any write
that does arrive, so nothing is lost, but the application sees errors for the whole run.
The fence and why it also covers replication apply workers are described in
[engine-role.md](engine-role.md#the-write-fence).

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

Either enable logical decoding, or run with `--block-writes SCHEMA.TABLE` if the table can go
without writes for the run.

## Free disk space

The shadow table is a full second copy of the table and its indexes, so the preflight requires
free space of at least twice the table's heap, indexes, and TOAST. PostgreSQL cannot report the
free space on its own volume over SQL, so you pass it with `--free-disk` (binary units: `MiB`,
`GiB`, `TiB`). A run without it is refused with cause `copy-and-swap-disk-headroom`.

- **Self-managed PostgreSQL:** run `df -h` on the volume holding the data directory.
- **Amazon RDS for PostgreSQL:** read the instance's `FreeStorageSpace` metric in CloudWatch.
- **Aurora PostgreSQL:** the cluster volume grows on demand, but only up to a maximum. Read the
  cluster's `AuroraVolumeBytesLeftTotal` metric in CloudWatch, the space left before that
  maximum, and pass the lower of it and any storage quota you set.

Never pass a made-up large value: the check is the only thing between the copy and a full
volume. We would like to remove this flag by reading the free space where the platform exposes
it. Until then it stays required.

## Run it

```sh
export PGSPRITE_URL='postgres://app_owner:…@db-primary.example.com:5432/shop?sslmode=verify-full'

# Preview: route the statement and print the plan, run nothing
pg-sprite migrate --dry-run \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Live: the application keeps writing
pg-sprite migrate --free-disk 50GiB \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Offline: the table's writes are blocked for the length of the run
pg-sprite migrate --free-disk 50GiB --block-writes public.order_archive \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

Point `PGSPRITE_URL` directly at the primary, not at a connection pooler.

A successful run prints one verdict, `executed-via-copy-and-swap`, and exits 0. With `--json`:

```json
{
  "outcome": "executed-via-copy-and-swap",
  "statement": "ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)",
  "table": "public.order_archive",
  "detail": "verified 5000 rows in 3 chunks; the retained source is dropped; writes to the table were blocked for the length of the run"
}
```

The old table is dropped once the swap commits. Add `--keep-old` to keep it under its `_old`
name for a rollback window; you drop it yourself when the window ends.

## Stop, resume, and start over

| You want to | Do this |
| --- | --- |
| Stop a running change | Press Ctrl-C, or send `SIGTERM`, once. The run saves where its copy reached and exits. The table keeps its fence (offline) or its replication slot (live) until you resume or start over. |
| Resume it | Run the same command again, including `--block-writes SCHEMA.TABLE` if the run blocked writes. It continues from where it stopped; nothing is copied twice. |
| Start over | Run the same command with `--restart SCHEMA.TABLE`, naming the table exactly. pg-sprite discards the shadow, the fence or slot, and the saved progress, then runs from the beginning. |
| Recover from bad data | When the server rejects a row under the new type or a constraint, the run reports `code: data-fault` and tries to abandon itself. Follow the verdict's detail: if it says the run was abandoned, nothing is left behind, so fix the data or the statement and run it again. If it says the run could not be abandoned, the shadow and the fence or slot remain, so fix the data or the statement and run it with `--restart SCHEMA.TABLE`. |

```sh
pg-sprite migrate --free-disk 50GiB --block-writes public.order_archive --restart public.order_archive \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

A stopped live run keeps its replication slot, and the slot keeps WAL on the server. Resume it
or start it over promptly rather than leaving it.
