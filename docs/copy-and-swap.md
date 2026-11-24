# Running a copy-and-swap

Some schema changes cannot run in place without blocking the table, most commonly a column
type change such as `integer` to `numeric(12,2)`. pg-sprite routes these to **copy-and-swap**:
it builds the new shape as a shadow table, copies the rows into it in chunks, verifies the
copy, and swaps it in under the table's name with a short, bounded lock.

This page is the operator's guide: how to open the route, the two ways to run it, how to
choose, what each needs, and how to stop, resume, and start over. The design is in
[copy-and-swap-design.md](copy-and-swap-design.md); every refusal is in
[limitations.md](limitations.md) and [refusal-classes.md](refusal-classes.md).

## Contents

- [Preview: the route is closed until you open it](#preview-the-route-is-closed-until-you-open-it)
- [Live or offline](#live-or-offline)
- [What each run needs](#what-each-run-needs)
- [Free disk space](#free-disk-space)
- [Slot lag ceiling](#slot-lag-ceiling)
- [Run it](#run-it)
- [Stop, resume, and start over](#stop-resume-and-start-over)

## Preview: the route is closed until you open it

Copy-and-swap is in preview: it has not yet been exercised under production-like load, so it
runs only when you open it for the run with `--copy-and-swap` (`CopySwapOptions.Enabled` as a
library caller). Without the flag, a statement the planner routes to copy-and-swap is refused
before the route's preflight runs or anything is written — target resolution and planning
still read the catalog to route it — and every native change is unaffected:

```console
$ pg-sprite migrate --free-disk 1GiB --json \
    --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
{
  "outcome": "refused",
  "reason": "copy-and-swap-refused",
  "class": "capability-boundary",
  "cause": "copy-and-swap-not-enabled",
  "statement": "ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)",
  "table": "public.order_archive",
  "detail": "copy-and-swap is in preview and not enabled for this run: the change needs a shadow-table copy and cutover, which runs only when the policy opts in (migrate --copy-and-swap on the CLI, Options.CopySwap.Enabled in the library); nothing was executed"
}
```

The other route flags — `--free-disk`, `--slot-lag-ceiling`, `--block-writes`, `--keep-old`,
`--restart` — open nothing on their own; they govern a run that `--copy-and-swap` has opened. The flag does not
combine with `--dry-run`, which never runs anything and so needs no opening. With
`--desired`, a derived plan that routes any statement to copy-and-swap is refused whole,
native statements included, unless the flag is set. `--force` keeps its meaning: it runs the
statement natively, bypassing the route and this gate alike.

Every example on this page that runs a copy-and-swap carries the flag.

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
writes fail. Run each statement with `migrate --alter --block-writes` instead. pg-sprite does
not take the declaration on trust: the fence refuses any write that does arrive, so nothing is
lost, but the application sees errors for the whole run.
The fence and why it also covers replication apply workers are described in
[engine-role.md](engine-role.md#the-write-fence).

## What each run needs

Both:

- a table shape copy-and-swap carries: a usable primary key (one `smallint`, `integer`, or
  `bigint` column; [primary-keys.md](primary-keys.md) covers adding one to a table without
  it), and none of the shapes the
  [copy-and-swap row in limitations.md](limitations.md) lists, such as foreign keys,
  triggers, dependent views, or partitioning. The preflight refuses anything else with a
  typed `copy-and-swap-refused` cause before it builds anything;
- the privileges in [engine-role.md](engine-role.md) for tier 3;
- `--free-disk`, described next.

Live runs also need logical decoding. Without it, a live run is refused before anything is built:

```console
$ pg-sprite migrate --copy-and-swap --free-disk 1GiB --json \
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
free space of at least twice the table's heap, indexes, and TOAST. A live run also needs room
for the WAL its replication slot may retain, so its preflight adds the
[slot lag ceiling](#slot-lag-ceiling) to that figure: 1 GiB unless you pass
`--slot-lag-ceiling`. A live run on a 10 GiB table therefore needs 21 GiB free by default; an
offline run (`--block-writes`) holds no slot and needs 20 GiB. PostgreSQL cannot report the
free space on its own volume over SQL, so you pass it with `--free-disk` (binary units: `MiB`,
`GiB`, `TiB`). A run without it, or with less than it needs, is refused with cause
`copy-and-swap-disk-headroom`; for a live run the detail names both the shadow's figure and
the ceiling's.

- **Self-managed PostgreSQL:** run `df -h` on the volume holding the data directory.
- **Amazon RDS for PostgreSQL:** read the instance's `FreeStorageSpace` metric in CloudWatch.
- **Aurora PostgreSQL:** the cluster volume grows on demand, but only up to a maximum. Read the
  cluster's `AuroraVolumeBytesLeftTotal` metric in CloudWatch, the space left before that
  maximum, and pass the lower of it and any storage quota you set.

Never pass a made-up large value: the check is the only thing between the copy and a full
volume. We would like to remove this flag by reading the free space where the platform exposes
it. Until then it stays required.

A resumed run is asked only for what is left to grow. The space the stopped run already holds
is gone from the free space you measure, so the resume's preflight subtracts it from the
requirement: the shadow's size so far, and, for a live run, the WAL its replication slot
retains. pg-sprite reads both from the server, not from the saved progress. Say a live run on
a 10 GiB table stopped with 6 GiB of shadow built and 1.5 GiB of WAL in its slot, and you
resume it with `--slot-lag-ceiling 2GiB`. The resume needs 20 − 6 = 14 GiB for the shadow and
2 − 1.5 = 0.5 GiB for the slot, 14.5 GiB in all. Pass the free space the volume has now, as for
any run. Neither discount goes past its own requirement, and anything pg-sprite cannot read
(no shadow, a slot that is gone or lost, a server that cannot measure the WAL) counts as
nothing held, so the check falls back to a fresh run's figure. A run that stopped after its
swap committed, before it finished, is asked for no space at all when its statement runs
through copy-and-swap again: that run only drops the slot and the retained old table and
records the change done, which frees space. A run with
`--restart SCHEMA.TABLE` is checked as a fresh run: it discards what the stopped run held
before it builds anew.

## Slot lag ceiling

A live run's replication slot keeps WAL on the server until pg-sprite has applied the changes
in it. The run stops when the slot holds more than the slot lag ceiling: 1 GiB unless you pass
`--slot-lag-ceiling SIZE` (`CopySwapOptions.SlotLagCeilingBytes` as a library caller). An
offline run holds no slot, so the flag is refused with `--block-writes`. The ceiling, the
1 GiB default included, counts against `--free-disk` beside the shadow (see
[Free disk space](#free-disk-space)), so a ceiling the volume cannot hold is refused with cause
`copy-and-swap-disk-headroom` before anything is built. That check is a floor on the free
space, not a guarantee the slot cannot fill the volume: the check counts the ceiling once, and as
the next paragraph says, the slot can stand over it. A busy slot sized as below peaks at about
twice its ceiling just before PostgreSQL frees WAL, and the run stops once a cycle reads more
than three times the ceiling (`decode.RetainedHardLimit` gives that figure in bytes), so leave
the volume room for about three times the ceiling plus a catch-up cycle of the cluster's WAL.

The ceiling is a stop threshold pg-sprite checks once per catch-up cycle, not a limit
PostgreSQL enforces on the slot. The slot can hold more than the ceiling by the time a cycle
reads it, and retained WAL is allowed over it for a short grace (see `wal-retained` below).
Stopping frees no WAL: the slot stays so the next run can resume, and it keeps retaining WAL
for as long as the source takes writes. After a stop, watch the slot's retained WAL
(`pg_replication_slots`) and either resume soon or drop the slot deliberately, and raise the
ceiling only with free space on the volume for that much WAL, read the way
[Free disk space](#free-disk-space) describes for your platform.

`SIZE` is a positive whole number of bytes, optionally followed by a binary unit: `B`, `KiB`,
`MiB`, `GiB`, or `TiB`, case-insensitive; `KB`, `MB`, `GB`, and `TB` mean the same binary
multiples. `2GiB` and `2147483648` are the same ceiling. Zero, negative values, fractions such
as `1.5GiB`, and other units are refused as a usage error (exit 1) before anything runs. There
is no lower limit beyond one byte, as in the library, but a live run needs more than about 15
seconds and a catch-up cycle of the cluster's WAL, with margin, or it stops `wal-retained` even
while pg-sprite keeps up (see below); a ceiling well below the default stops a busy run sooner,
not more safely.

The run stops with a failed verdict (exit 1) whose detail says which of three causes ended it;
the library's `decode.SlotLagExceededError` carries the same as its `Cause`. The
slot and the saved progress stay, so running the same command again resumes the run:

- **`stream-behind`:** more WAL than the ceiling was waiting for pg-sprite to confirm, so
  pg-sprite fell behind the source's writes. Resume once the write rate drops; the resume
  drains the backlog from the slot.
- **`wal-retained`:** pg-sprite kept up, but the server retained more WAL than the ceiling
  for longer than it takes to release WAL pg-sprite has confirmed. Look for a transaction holding
  it back first: an open write transaction (`pg_stat_activity` where `backend_xid IS NOT
  NULL`, oldest `xact_start` first) or a prepared one (`pg_prepared_xacts`). End it, then
  resume. If there is none, the cluster writes more WAL per release interval and cycle than the
  ceiling holds. PostgreSQL frees a slot's WAL in steps about 15 seconds apart, and on a busy source
  each step stops one step short, so the slot keeps between 15 and 30 seconds of the cluster's
  WAL even while pg-sprite keeps up. That is everything the server logs, not only the
  application's writes: the copy's own inserts into the shadow and other databases count too.
  The ceiling must hold more than 15 seconds and a catch-up cycle of it, with margin (ten
  percent covers a record the server logs late or a stream a cycle slower). To read your slot
  the same way, compare `pg_current_wal_lsn()` with `restart_lsn` (retained) and
  `confirmed_flush_lsn` (unconfirmed) from `pg_replication_slots`; `make slot-release-proof`
  logs the same three on a busy test source. Resume with a higher ceiling, above the retained WAL the detail reports, and with room on the volume for
  that much WAL. The resume's preflight counts the new ceiling against
  `--free-disk` beside the shadow, less the shadow already built and the WAL the slot already
  retains (see [Free disk space](#free-disk-space)), so pass the free space the volume has now.
- **`wal-retained-hard-limit`:** pg-sprite kept up, but a cycle read the slot retaining more
  than three times the ceiling before that release time had passed. A healthy slot peaks at
  about twice its ceiling, so this is either a transaction holding the WAL back or a write rate
  the ceiling cannot hold. Look for the transaction first, as for `wal-retained`, and end it,
  then resume; if there is none, resume with a higher ceiling as for `wal-retained`:

```sh
pg-sprite migrate --copy-and-swap --free-disk 50GiB --slot-lag-ceiling 4GiB \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'
```

## Run it

```sh
export PGSPRITE_URL='postgres://app_owner:…@db-primary.example.com:5432/shop?sslmode=verify-full'

# Preview: route the statement and print the plan, run nothing
pg-sprite migrate --dry-run \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Live: the application keeps writing
pg-sprite migrate --copy-and-swap --free-disk 50GiB \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'

# Offline: the table's writes are blocked for the length of the run
pg-sprite migrate --copy-and-swap --free-disk 50GiB --block-writes public.order_archive \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

Point `PGSPRITE_URL` directly at the primary, not at a connection pooler.

A successful run prints one verdict, `executed-via-copy-and-swap`, and exits 0. The live
run above, with `--json`:

```json
{
  "outcome": "executed-via-copy-and-swap",
  "statement": "ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)",
  "table": "public.orders",
  "detail": "verified 52693 rows in 26 chunks (1 settled against the stream); the retained source is dropped",
  "swap_lsn": "0/3F2A1B8",
  "verification": {
    "rows": 52693,
    "chunks": 26,
    "settled": 1,
    "unsettled": 2,
    "carried": 3
  }
}
```

The `--block-writes` run's verdict also carries `"writes_blocked": true`, and its `detail`
ends by saying writes to the table were blocked for the length of the run.

`swap_lsn` is the server's WAL position read inside the swap transaction, under its lock:
writes to the table after it exist only in the new table. `verification` is what the
verification pass compared: rows hashed, chunks compared, and, for a live run, the chunks it
settled against the stream, the chunks it could not settle, and how many keys of those it left
for the swap to compare under its lock (`carried`, a count, never the keys). `detail` states
the rows, chunks, and settled chunks too; `unsettled` and `carried` are only in the object.
On this outcome a non-zero `unsettled` or `carried` is a record, not a warning: the swap compared those keys under its lock and would have refused had any differed.

The old table is dropped once the swap commits. Add `--keep-old` to keep it under its `_old`
name for a rollback window; you drop it yourself when the window ends.

## Stop, resume, and start over

| You want to | Do this |
| --- | --- |
| Stop a running change | Press Ctrl-C, or send `SIGTERM`, once. The run saves where its copy reached and exits. The table keeps its fence (offline) or its replication slot (live) until you resume or start over. |
| Resume it | Run the same command again — `--copy-and-swap` included, since the resume is a copy-and-swap run too — with `--block-writes SCHEMA.TABLE` if the run blocked writes. It continues from where it stopped; unless the slot was lost, nothing is copied twice. If a live run's replication slot is gone or the server reports it lost (after a failover, or when the server drops a slot that retained too much WAL), or a reconcile stopped partway, the resume reconciles first, with no flag needed: it creates a new slot, compares every landed chunk against the source, recopies only those that differ, and then continues. The comparison takes time in proportion to the rows already copied ([ST-4](invariants.md#st-4--slot-loss-is-a-modeled-state-transition-not-a-crash)). |
| Start over | Run the same command (again with `--copy-and-swap`) plus `--restart SCHEMA.TABLE`, naming the table exactly. pg-sprite discards the shadow, the fence or slot, and the saved progress, then runs from the beginning. `--restart` applies to `migrate --alter` only: `migrate --desired` rejects it, because there the derived plan, not you, decides which run the flag would discard. A stopped `--desired` copy resumes when you run the plan again; to start it over, run the stopped statement with `migrate --alter --copy-and-swap --restart SCHEMA.TABLE` first. The failed run's detail numbers the statement that stopped ("planned statement N of M"), and `pg-sprite diff --desired FILE --sql` prints the plan's statements. |
| Recover from bad data | When the server rejects a row under the new type or a constraint, the run reports `code: data-fault` and tries to abandon itself. Follow the verdict's detail: if it says the run was abandoned, nothing is left behind, so fix the data or the statement and run it again. If it says the run could not be abandoned, the shadow and the fence or slot remain, so fix the data or the statement and run it with `--restart SCHEMA.TABLE`. |

```sh
pg-sprite migrate --copy-and-swap --free-disk 50GiB --block-writes public.order_archive --restart public.order_archive \
  --alter 'ALTER TABLE public.order_archive ALTER COLUMN amount TYPE numeric(12,2)'
```

A stopped live run keeps its replication slot, and the slot keeps WAL on the server. Resume it
or start it over promptly rather than leaving it.
