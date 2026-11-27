# Replication slots: what a live copy-and-swap holds on your database

A live copy-and-swap reads the table's writes from the WAL through a logical replication slot.
The slot is the one thing the run leaves on the source database that can grow on its own: it
keeps WAL on the volume until pg-sprite has applied the changes in it, including while no run
is going. This page explains how that works and what you control.

- **Part 1** covers the basics: WAL, replication slots, the positions that decide what WAL is
  kept, and how pg-sprite uses them.
- **Part 2** is the runbook: the controls you have, how to check a slot, what to do when a run
  stops at the slot lag ceiling, and how to resume, drop the slot, or start over.

An offline run (`--block-writes SCHEMA.TABLE`) holds no slot, so nothing on this page applies
to it. The operator's guide to the route as a whole is [copy-and-swap.md](copy-and-swap.md).

## Contents

- [Part 1: how it works](#part-1-how-it-works)
  - [WAL and when PostgreSQL frees it](#wal-and-when-postgresql-frees-it)
  - [Replication slots](#replication-slots)
  - [The positions that matter](#the-positions-that-matter)
  - [What PostgreSQL does when a slot holds too much](#what-postgresql-does-when-a-slot-holds-too-much)
  - [How pg-sprite uses its slot](#how-pg-sprite-uses-its-slot)
  - [The slot lag ceiling](#the-slot-lag-ceiling)
- [Part 2: runbook](#part-2-runbook)
  - [Your controls](#your-controls)
  - [Check the slot](#check-the-slot)
  - [The run stopped at the ceiling](#the-run-stopped-at-the-ceiling)
  - [Free the WAL: resume, drop the slot, or start over](#free-the-wal-resume-drop-the-slot-or-start-over)
  - [The volume is already full](#the-volume-is-already-full)
  - [The run was killed or the host died](#the-run-was-killed-or-the-host-died)
  - [After a failover](#after-a-failover)
  - [Clean up slots no run owns](#clean-up-slots-no-run-owns)
  - [For library hosts](#for-library-hosts)

## Part 1: how it works

### WAL and when PostgreSQL frees it

Every change PostgreSQL makes is first written to the write-ahead log (WAL), a sequence of
segment files in `pg_wal`, 16 MiB each by default. A position in the WAL is an LSN (log sequence number), written
like `0/3F2A1B8`; `pg_current_wal_lsn()` is where the server is writing now.

At each checkpoint PostgreSQL removes or recycles the segments nothing needs any more. A
segment is still needed while crash recovery, `wal_keep_size`, a physical standby, or a
**replication slot** needs WAL from it. A slot is the only one of these that has no upper bound
by default.

### Replication slots

A replication slot is a named, persistent bookmark in the WAL for one consumer. It survives
restarts, and the server keeps every byte of WAL from the slot's position onward until the
consumer says it has processed it. That is what lets a consumer disconnect and come back
without missing a change. It is also the risk: **the server keeps the WAL whether or not
anything is reading the slot.** A slot nobody reads holds WAL forever.

A logical slot (the kind pg-sprite uses) decodes the WAL into row changes through an output
plugin. pg-sprite uses the built-in `pgoutput` plugin, filtered to the one table by a
publication.

### The positions that matter

`pg_replication_slots` has one row per slot. These columns decide what the slot costs:

| Column | What it means | What it holds back |
| --- | --- | --- |
| `restart_lsn` | The oldest WAL decoding would have to start from again, usually the start of the oldest transaction still open when the slot last moved forward. | **WAL on the volume.** No segment at or after it is removed. |
| `confirmed_flush_lsn` | How far the consumer (pg-sprite) has confirmed applying changes. A new stream starts sending from here. | Nothing directly. It lets `restart_lsn` move forward. |
| `catalog_xmin` | The oldest catalog version decoding may still need, to read rows with the table definition they were written under. | **Vacuum of the system catalogs** (`pg_class`, `pg_attribute`, and so on), which can bloat while it is held. |
| `active`, `active_pid` | Whether a process is streaming from the slot now. | Nothing. An inactive slot holds exactly as much as an active one. |
| `wal_status` | The server's verdict on the slot's WAL: `reserved`, `extended`, `unreserved`, or `lost`. | Nothing. `lost` means the WAL it needed is gone. |

Two measures follow from these, and pg-sprite judges both:

- **Retained WAL** = `pg_current_wal_lsn() − restart_lsn`: what the slot keeps on the volume.
  This is the disk cost.
- **Unconfirmed WAL** = `pg_current_wal_lsn() − confirmed_flush_lsn`: what pg-sprite has yet to
  apply. This is how far behind the run is.

Retained is always at least unconfirmed, and two things keep it larger even while the run keeps
up:

1. **The server releases WAL in steps.** `restart_lsn` only moves at the running-transactions
   records the server writes about every 15 seconds, so a healthy, busy slot still keeps between
   15 and 30 seconds of the cluster's WAL.
2. **An open transaction pins it.** `restart_lsn` cannot pass the start of a transaction that is
   still open on the source, however far pg-sprite has confirmed. A forgotten `BEGIN` in a psql
   session or a prepared transaction can hold WAL for hours.

### What PostgreSQL does when a slot holds too much

PostgreSQL has no limit of its own unless you set one:

| Server setting | What happens as the slot's retained WAL grows |
| --- | --- |
| `max_slot_wal_keep_size = -1` (the default) | Nothing stops it. WAL grows until the volume is full; PostgreSQL then cannot write WAL, and the server shuts down. This is the outage the slot lag ceiling exists to prevent. |
| `max_slot_wal_keep_size` set to a size | Past the size, `wal_status` goes to `unreserved`, and at the next checkpoint the server removes the WAL and **invalidates the slot**: `wal_status = 'lost'`, and on PostgreSQL 17 and later `invalidation_reason = 'wal_removed'`. The disk is saved; the slot can no longer stream. |
| `idle_replication_slot_timeout` (PostgreSQL 18) | A slot left inactive for longer than the timeout is invalidated, with `invalidation_reason = 'idle_timeout'`. |

A lost slot does not lose the schema change: the next pg-sprite run reconciles (see
[Free the WAL](#free-the-wal-resume-drop-the-slot-or-start-over), and
[ST-4](invariants.md#st-4--slot-loss-is-a-modeled-state-transition-not-a-crash) for the
guarantee).

### How pg-sprite uses its slot

The slot's whole life follows the run:

```text
  start            copy + catch up             verify          swap commits
    │                    │                        │                  │
    ▼                    ▼                        ▼                  ▼
 create slot ──▶ stream writes to shadow ──▶ compare chunks ──▶ drop slot
 (exported        confirm after each apply     (stream still       (WAL freed)
  snapshot)                                     applied)
                         │
                         │ stop: Ctrl-C, SIGTERM, ceiling, error, kill
                         │ (not an abandoned data fault: that drops it)
                         ▼
                 slot stays, inactive, still holding WAL
                 until a resume, a drop, or --restart
```

[ST-3](invariants.md#st-3--slot-cleanup-is-guaranteed-on-success-failure-and-crash) is the
guarantee behind this lifecycle, and the rest of this section states its parts.

- **Name.** The slot and its single-table publication share one name, `pgsprite_` followed by
  eight hex digits derived from the database, schema, and table (for example
  `pgsprite_b32f988b`). The same table always gets the same name, which is how a resume finds
  its slot.
- **Created at the start** with an exported snapshot, so the copy reads the table as of exactly
  the slot's starting point and no write falls between the two. A live run needs one free slot
  under `max_replication_slots` and two free senders under `max_wal_senders`.
- **Recorded in the checkpoint row.** The table `pgsprite.pgsprite_checkpoint` has one row per
  table in flight, naming the slot, the copy's saved progress (the watermark), and the last
  position applied. A resume reads the row to find where to continue.
- **Confirmed only after applying.** pg-sprite confirms a position to the server only once the
  changes up to it are on the shadow, so the slot never lets go of a change that has not landed.
- **Dropped once the swap commits.** On success the slot and publication are gone.
- **Kept on every stop but two.** Ctrl-C, `SIGTERM`, the ceiling, an error, or a killed process
  all leave the slot so the next run can resume without copying again. While it waits, it is
  inactive and still holding WAL. The exceptions drop it: a data fault the run abandons (the
  server rejected the rows as the statement would shape them, so the shadow, the slot, and the
  checkpoint row all go), and a first progress save that fails, which drops the slot it just
  made. A data fault the run could not abandon keeps the slot, and its detail says so.

### The slot lag ceiling

The slot lag ceiling is the most WAL a run's slot may hold before the run stops itself: 1 GiB
unless you pass `--slot-lag-ceiling SIZE` (`CopySwapOptions.SlotLagCeilingBytes` in the
library). It protects the source's volume from a run that has fallen behind.

pg-sprite checks the slot once per catch-up cycle and stops with a failed verdict (exit 1)
naming one of three causes:

| Cause | Measure | When it trips | What it usually means |
| --- | --- | --- | --- |
| `stream-behind` | Unconfirmed WAL | One reading over the ceiling | pg-sprite cannot keep up with the source's write rate. |
| `wal-retained` | Retained WAL | Over the ceiling for longer than the server's release interval plus a cycle | pg-sprite keeps up, but an open transaction pins the WAL, or the cluster writes more than the ceiling holds in 15–30 seconds. |
| `wal-retained-hard-limit` | Retained WAL | One reading over three times the ceiling, before that grace has passed | The same as `wal-retained`, too far over to wait. |

The ceiling does three other things:

- **It paces the copy.** A live run holds its copy back while unconfirmed WAL is over a quarter
  of the ceiling, so the catch-up gets ahead before more rows are copied.
- **It counts against `--free-disk`.** The preflight adds the ceiling to the shadow's size, so a
  ceiling the volume cannot hold is refused before anything is built.
- **It is a stop, not a cap.** PostgreSQL does not enforce it, the slot can be over it by the time
  a cycle reads it, and a busy slot peaks at about twice its ceiling. Leave the volume room for
  about three times the ceiling plus a catch-up cycle of the cluster's WAL;
  [copy-and-swap.md](copy-and-swap.md#slot-lag-ceiling) owns the sizing and the flag's syntax.

**Stopping frees no WAL.** The stopped run's slot stays, and it keeps growing for as long as the
source takes writes. The ceiling ends the run; you still have to resume it, drop the slot, or
start over (Part 2).

## Part 2: runbook

Before you start:

- Run every command and query here against the **primary** that holds the slot, connected
  directly, not through a connection pooler. Set `PGSPRITE_URL` the way
  [copy-and-swap.md](copy-and-swap.md#run-it) does for the run itself.
  `pg_current_wal_lsn()` raises an error on a standby.
- Dropping a slot by hand needs a role that may: a superuser, a role with the `REPLICATION`
  attribute, or the platform's replication role on a managed service.

### Your controls

| Control | What it does | When to use it |
| --- | --- | --- |
| `--block-writes SCHEMA.TABLE` | Runs offline: no slot at all, writes to the table fail until the swap. | The table can go without writes for the run, or the server lacks logical decoding. |
| `--slot-lag-ceiling SIZE` | Sets the stop threshold, the copy's pace, and the WAL the preflight reserves. Default `1GiB`. | Raise it for a busy cluster, with free space for about three times the value. |
| `--free-disk SIZE` | The free space on the source volume; the preflight requires the shadow plus the ceiling. | Every copy-and-swap. Measure it; never pass a made-up value. |
| Ctrl-C or `SIGTERM`, once | Stops the run cleanly: saves the copy's progress and exits. The slot stays. | Pausing a run. Resume soon. |
| Run the same command again | Resumes from the saved progress and drains the slot. | After a stop whose detail says the same statement resumes. A divergence stop, or a data fault the run could not abandon, stops again on a rerun; its detail says to use `--restart`. |
| `--restart SCHEMA.TABLE` | Discards the shadow, the slot, and the saved progress, then runs from the beginning in the same command, with a new copy and a new slot. | Starting over. On a volume that is nearly full, drop the slot by hand instead: `--restart` starts filling it again at once. |
| `pg-sprite reap` | Drops `pgsprite_` slots no in-flight run owns. Never touches a slot a run's row names. | Cleaning up after a run that ended before its row was written, or whose cleanup did not finish. |
| `pg_drop_replication_slot('<name>')` | Drops a stopped run's slot by hand. The server frees the WAL at its next checkpoint; the shadow and saved progress stay. | Disk is running out and you cannot resume now. |
| `max_slot_wal_keep_size` (server) | PostgreSQL's own hard limit for every slot on the server. | A backstop above the run's own hard limit: more than three times the ceiling plus a catch-up cycle of the cluster's WAL (`decode.RetainedHardLimit` gives the first figure). Set lower, it invalidates healthy runs, which then reconcile and compare every chunk. It invalidates any slot past it, not only pg-sprite's. |

### Check the slot

While a run goes, or after it stops:

```sql
SELECT slot_name,
       active,
       wal_status,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn))         AS retained,
       pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)) AS unconfirmed,
       catalog_xmin
FROM pg_replication_slots
WHERE starts_with(slot_name, 'pgsprite_');
```

The engine's own slots are named exactly `pgsprite_` and eight hex digits; another slot that
starts with `pgsprite_` is not one it created.

- `active = t`: a run is streaming from it. `retained` and `unconfirmed` rise while the copy runs
  and fall as it catches up.
- `active = f`: a stopped run. `retained` grows with every write on the cluster until you act.
- `wal_status = lost`: the server invalidated it. The next run reconciles.

To see which table a slot belongs to and where the run stopped:

```sql
SELECT schema_name, table_name, slot_name, phase, watermark, updated_at
FROM pgsprite.pgsprite_checkpoint
ORDER BY updated_at DESC;
```

`pg-sprite status` lists the pg-sprite sessions running now; a live run's stream shows as a
`START_REPLICATION SLOT pgsprite_…` query.

### The run stopped at the ceiling

The failed verdict's detail names the cause and the WAL it read. Act on it promptly: the slot is
holding WAL while you decide.

1. **Check the slot** with the query above, and the free space on the volume
   ([how to measure it](copy-and-swap.md#free-disk-space)).
2. **Look for a transaction pinning the WAL** (for `wal-retained` and
   `wal-retained-hard-limit` first; it is worth a look for `stream-behind` too):

   ```sql
   SELECT pid, usename, application_name, state, xact_start, left(query, 80) AS query
   FROM pg_stat_activity
   WHERE backend_xid IS NOT NULL
     AND application_name <> 'pg-sprite'
   ORDER BY xact_start;

   SELECT gid, prepared, owner, database FROM pg_prepared_xacts ORDER BY prepared;
   ```

   The oldest `xact_start` is the one that pins the WAL. Ask its owner to finish it. If you
   must end it yourself, `SELECT pg_terminate_backend(<pid>)` **rolls back everything that
   transaction did**, and its client sees a dropped connection, so confirm with the owner
   first. A prepared transaction has no session: its owner decides between `COMMIT PREPARED`
   and `ROLLBACK PREPARED`. Once it ends, the slot's `restart_lsn` moves at the server's next
   running-transactions record, about 15 seconds later, and the segments go at the next
   checkpoint, not at once.
3. **Resume** by running the same command again:
   - `stream-behind`: resume when the write rate drops. If the slot's unconfirmed WAL is already
     close to the ceiling, the resume can stop again at once; resume with a higher ceiling, above
     what the slot holds, if the volume has room.
   - `wal-retained` or `wal-retained-hard-limit` with the transaction ended: resume as is.
   - Either one with no transaction to blame: the cluster writes more than the ceiling holds.
     Resume with a higher ceiling, above the retained WAL the detail reports:

   ```sh
   pg-sprite migrate --copy-and-swap --free-disk 50GiB --slot-lag-ceiling 4GiB \
     --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'
   ```

   Pass the free space the volume has now. The resume's preflight subtracts what the stopped run
   already holds, the shadow built so far and the WAL the slot retains.
4. **If you cannot resume soon** and the volume is filling, free the WAL by one of the options
   in the next section.

### Free the WAL: resume, drop the slot, or start over

There are three ways to stop a stopped run's slot from holding WAL. They differ in what the
next run has to redo:

| Option | Frees | Next run | Cost |
| --- | --- | --- | --- |
| **Resume** (run the same command) | The WAL, as the run confirms it | Continues from the saved progress | None: nothing is copied twice. Only after a stop whose detail says the same statement resumes (see [Your controls](#your-controls)). |
| **Drop the slot by hand** | The WAL, at the server's next checkpoint | Reconciles, then continues | Compares every chunk already copied against the source and recopies the ones that differ. Takes time in proportion to the rows copied. The shadow keeps its disk. |
| **`--restart SCHEMA.TABLE`** | The slot's WAL and the shadow's disk, but the same command then builds a new shadow and slot | Starts from the beginning | The whole copy again. |

Dropping the slot by hand does **not** throw the schema change away: the shadow and the checkpoint
row stay, and the next run creates a fresh slot and repairs the gap by comparison instead of
copying everything again. Only `--restart` starts from scratch. On a volume that is nearly
full, the hand drop is the one that gives room back: `--restart` starts a new copy and a new
slot in the same command.

To drop a stopped run's slot by hand, first confirm no run is going:

- `pg-sprite status` shows no `START_REPLICATION SLOT` query for the slot, and the checkpoint
  row's `updated_at` (the query in [Check the slot](#check-the-slot)) has stopped advancing.
- The slot reads `active = false`:

  ```sql
  SELECT slot_name, active, active_pid FROM pg_replication_slots WHERE slot_name = 'pgsprite_b32f988b';
  ```

`active = false` alone is not enough: a run that is still going has short gaps with no stream
open, between its catch-up and its swap. The server refuses to drop a slot that is held, so a
drop cannot cut a stream, but a drop in such a gap makes the run reconcile. Then drop it:

```sql
SELECT pg_drop_replication_slot('pgsprite_b32f988b');
```

The server removes the WAL at its next checkpoint. To have it sooner, run `CHECKPOINT` (a
superuser, or a role granted `pg_checkpoint` on PostgreSQL 15 and later). Leave the
publication of the same name in place; the next run uses it.

If `active` is true, look at what holds the slot. If a run is still going, stop it with Ctrl-C
or `SIGTERM` rather than dropping the slot under it. If the process that ran it is gone
(killed, or its host lost), the server's stream process for it holds the slot until
`wal_sender_timeout` passes; see [The run was killed or the host died](#the-run-was-killed-or-the-host-died).
Once you know that process is gone, `SELECT pg_terminate_backend(<active_pid>)` ends the
stream process, which holds no data of its own, and the slot can be dropped.

To start over, which discards everything the stopped run built:

```sh
pg-sprite migrate --copy-and-swap --free-disk 50GiB --restart public.orders \
  --alter 'ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)'
```

`--restart` runs the change again from the beginning in the same command; the CLI has no command
that only discards. A library caller can discard without running with `Runner.Discard`.

### The volume is already full

When the volume fills, PostgreSQL cannot write WAL and the server shuts down, so none of the
SQL above runs until it is back:

1. **Free or add space first.** Grow the volume, or move files that are not the database's off
   it. Never delete files in `pg_wal` by hand: the server needs them to start, and removing
   them can lose committed data.
2. **Start the server.**
3. **Drop the slot by hand**, as in the previous section, and run `CHECKPOINT` so the server
   removes the WAL now. The schema change is not lost: the next run reconciles.
4. **Rerun** once the volume has room for the run, with `--free-disk` measured again.

### The run was killed or the host died

`SIGKILL`, an out-of-memory kill, a lost container, or a crashed host stop the run without its
clean save:

- **The slot stays**, inactive once the server notices the connection is gone, and holds WAL like
  any stopped run's.
- **Progress goes back to the last save.** The copy saves its progress every 10 seconds; a rerun
  copies again from there. A run killed before its first save copies from the start.
- **Resume the same way:** run the same command again. If the resume refuses with
  `resume-slot-held`, the old stream's server process has not exited yet. It ends within
  `wal_sender_timeout` (60 seconds by default); run the command again then.

### After a failover

The slot lives on the primary that created it. If the database fails over to a standby the slot
did not reach, the slot is gone on the new primary. The next run sees the slot missing and
reconciles, as after a hand drop. If the slot did reach the new primary (a synchronized slot),
the next run resumes from it as after any stop; pg-sprite does not yet check that the slot's
writer changed behind it, so it takes the slot's word.

If the old primary comes back as a server of its own with its copy of the slot, that slot holds
WAL there. `pg-sprite reap` keeps a slot while its own database's checkpoint row names it, and
the old primary's copy of the row still does, so drop that slot there by hand, as above, once
you have confirmed no run uses that server.
[ST-3](invariants.md#st-3--slot-cleanup-is-guaranteed-on-success-failure-and-crash) states the
cleanup guarantee for slots no row owns.

### Clean up slots no run owns

A slot no in-flight checkpoint row names is an orphan: a run that ended before its row was
written, or whose cleanup did not finish. It holds WAL like any other slot.

```sh
pg-sprite reap --dry-run   # list what would be dropped
pg-sprite reap             # drop the orphans and their publications
```

`reap` keeps a slot an in-flight row names, a slot a process holds, and any `pgsprite_` slot the
engine did not create, so it never frees a stopped run's slot. It drops slots and publications
only: shadow tables, checkpoint rows, and table data are never touched. Free a stopped run's slot by resuming,
dropping it by hand, or `--restart`, as above. The full report is in
[cli.md](cli.md#drop-orphan-slots).

### For library hosts

A host that runs copy-and-swap through `pkg/migrate` or `pkg/schemachange` can route on the same
decisions as this page, from typed errors rather than the CLI's prose:

- A ceiling stop is a `*decode.SlotLagExceededError`. Its `Cause` names one of the three causes,
  and `Cause.RetainsWAL()` is true for the two where a transaction or the write rate pins the
  WAL, the ones whose remedy is to look for the transaction, then raise the ceiling.
- A lost slot is a `*decode.SlotLostError`, matched with `errors.Is(err, decode.ErrSlotLost)`;
  the next run reconciles.
- A resume refused because the old stream still holds the slot carries the refusal cause
  `schemachange.CauseResumeSlotHeld` (`resume-slot-held`); retry after `wal_sender_timeout`.
- `decode.RetainedHardLimit(ceiling)` is the retained WAL a run stops at regardless of grace,
  the figure to size a volume and `max_slot_wal_keep_size` by.
