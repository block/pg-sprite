# The progress report contract

The progress snapshot is the machine-readable observation a caller receives when it polls a
running schema change through the `*WithProgress` executor entry points. It is the one JSON
shape an operator or orchestrator consumes to display or act on execution progress. This
document is the contract: the fields, the closed vocabularies, and the behavior required of
a consumer. The Go source of truth is `pkg/progress`; `TestSnapshotJSONShape`,
`TestSnapshotJSONShapeForACopyStep` and `TestSnapshotJSONShapeForAChecksumStep` pin the exact
keys, including the three examples at the end of this page.

## Versioning: `format_version`

Every snapshot carries `format_version`. A consumer that does not recognize the version must
**reject the snapshot** — never guess at field semantics. The version covers more than the
field shape: the closed vocabularies below (phases, operations) are pinned to it. Adding a
phase or operation value is a contract change and bumps `format_version`, even if no field
is added or renamed.

Adding a field bumps `format_version` so a strict consumer can detect the new shape from the
version. The current version is **5**: version 2 added `detail.statement`; version 3 added
`detail.current_locker_pid`, `work.lockers_total`, and `work.lockers_done`; version 4 added
the `copy` operation and split `work` into two counter families — the server-observed build
counters and the engine-measured copy counters (`rows_*`, `bytes_*`) — selected by
`detail.operation`, so `work` is no longer a signal that a concurrent index build is running;
version 5 added the `checksum` operation and its engine-measured counter family
(`chunks_compared`, `rows_hashed`, `chunks_mismatched`, `chunks_repaired`, `chunks_reread`).

The [plan report](plan-report.md), [lint report](lint-report.md), and
[suggest report](suggest-report.md) are separate contracts with their own `format_version`;
all version independently.

## Consumer behavior for unknown values

`phase` and `detail.operation` draw from the closed vocabularies below. A consumer that
meets a value it does not recognize must treat the execution's state as **unknown** — never
map it onto a known value and proceed. Progress is observational: an unknown value never
licenses a consumer to intervene in the change itself.

## Snapshot fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `format_version` | int | always | Contract version; reject unknown versions. |
| `phase` | string | always | Overall execution phase (see Phases). |
| `step` | int | after the first step starts | 1-based position in a multi-step sequence. Absent before execution reaches step 1. |
| `total_steps` | int | after `Start` | Number of steps in the execution; `1` for single-statement entry points. |
| `elapsed_ns` | int | always | Nanoseconds since execution started. For a terminal phase, **frozen** at the instant the outcome was recorded — a late poll reports the execution's duration, not the observation's age. |
| `step_elapsed_ns` | int | always | Nanoseconds since the current step started; frozen the same way at a terminal phase. |
| `detail` | object | always | The operation currently executing (below). |

## Detail fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `operation` | string | once execution starts | The current operation's execution class (see Operations). |
| `statement` | string | after a step starts | The exact SQL string the executor executes for this step, after front-door qualification and canonicalization — not a display rendering. It remains present on terminal snapshots so an observer can identify the statement that produced the outcome. |
| `server_phase` | string | active concurrent build only | PostgreSQL's own phase string from `pg_stat_progress_create_index`, verbatim. |
| `active` | bool | always | Whether an operation is executing now. `false` with `phase: "running"` means a concurrent build's progress row has left the server view. |
| `attempt` | int | bounded retries only | The current attempt number when the executor is inside its bounded retry loop. |
| `current_locker_pid` | int | while waiting on a locker | PostgreSQL backend PID currently blocking the concurrent build; omitted when none is published. |
| `work` | object | measured work only | Present exactly when something measured the step's work — the server published a progress row for a concurrent build, or the engine reported the copy or checksum step's counters; then **every** counter below is present, so a fresh build or an empty copy reports honest zeros rather than an empty object. Which counters mean anything is decided by `operation`, not by `work` being present — see [Work counters](#work-counters). |

`statement` is the SQL the engine is running for the step: for a native operation the
submitter's statement after qualification and canonicalization, for a `copy` step the
engine's own frozen chunk insert (the template with its `$1`/`$2` key bounds, not a chunk's
rendered values). Either way it is real SQL that reached the server, so a consumer rendering
it into a shared surface must clamp and escape it. A `checksum` step runs several frozen
statements per chunk, so the orchestrator that owns the step chooses what, if anything, to
put in `statement`.

### Work counters

Three operations publish `work`, and each measures only its own counters; the other
operations' counters are `0`, never estimated. A consumer selects the counter family from
`detail.operation`, never from the presence of `work`: a present `work` says only that
something measured the step, and a consumer that reads its presence as "a concurrent index
build is running" will show a `copy` step as a build with zero blocks. Render the build
counters for `concurrent-index-build`, the copy counters for `copy`, the checksum counters
for `checksum`, and nothing from `work` for an operation you do not recognize.

**Server-observed** (`concurrent-index-build`): `blocks_done` / `blocks_total`,
`tuples_done` / `tuples_total`, and `lockers_done` / `lockers_total` come from
`pg_stat_progress_create_index`, read over the executor's reserved session.

**Engine-measured** (`copy`): `rows_copied` / `rows_total` and `bytes_copied` /
`bytes_total` come from the copy-and-swap row copy itself, which knows the rows it has landed
from the chunks it committed. The copy step is not a single server statement, so PostgreSQL
publishes no progress view for it; the tracker instead polls the engine's work source for
the step. Every counter is something the engine measured — a row count from committed chunks,
a size read from the catalog — never a projection; a counter the engine cannot measure stays
`0`. Native operations report no rows or bytes.

| Counter | Meaning |
| --- | --- |
| `rows_copied` | Rows this run's committed chunks inserted into the shadow: exact and monotone. A resumed run counts only its own rows, not those the earlier run landed below the watermark. |
| `rows_total` | The source's catalog row count (`pg_class.reltuples`) read once when the copy started; `0` for a table `ANALYZE` has never visited. A count, not a scan. |
| `bytes_copied` | The shadow's on-disk table size (`pg_table_size`: heap, TOAST, maps; no indexes) measured at the poll. |
| `bytes_total` | The source's on-disk table size, measured the same way at the same poll. |

The two tables differ in shape, so `bytes_copied` ends above or below `bytes_total` rather than
equal to it; a consumer that wants a rate derives it from two snapshots of `rows_copied` and
`elapsed_ns`. On a resumed run `rows_copied` / `rows_total` is this run's share of the source,
not the copy's completion: the rows the earlier run landed below the watermark are in the
shadow but not in this run's count, so the ratio ends short of `1`. Completion is the copy's
watermark reaching the top of the key space, which the engine reports as the step ending.

The size read runs in a read-only transaction of the copy's own, under the copy's
`lock_timeout` and `statement_timeout`: a poll queued behind a lock on either table ends at
that timeout whatever session defaults the caller's pool carries, so an observer never holds
the copy's stop path open.

**Engine-measured** (`checksum`): `chunks_compared`, `rows_hashed`, `chunks_mismatched`,
`chunks_repaired` and `chunks_reread` come from the checksum pass itself, which cuts its own
chunks and digests each one on both tables inside one snapshot. The counters cover a whole
`Check`: the comparison that finds differing chunks and, under the `repair` policy, the recopy
and the reread that follow it, so a pass that spends most of its time repairing still shows
movement. They are read from the pass's memory — no catalog read, nothing for a poll to wait
on — and reset to zero when a pass starts. Native and `copy` operations report none of them.

| Counter | Meaning |
| --- | --- |
| `chunks_compared` | Chunks the comparison has digested on both sides; a digest counts when its transaction commits. The rereads of the repair phase are not in this count. |
| `rows_hashed` | Source rows those comparison digests covered. |
| `chunks_mismatched` | Chunks the comparison found differing. Under `abort` this is the finding the pass returns with; under `repair` it is the number of chunks the recopy covers. |
| `chunks_repaired` | Chunks whose recopy from the source has committed. Every differing chunk is recopied in one transaction, so this moves from `0` to `chunks_mismatched` at that commit. A chunk still differing at its reread stops the pass; it stays counted here because its recopy did commit. |
| `chunks_reread` | Repaired chunks digested again after the recopy, in a fresh snapshot. It climbs from `0` towards `chunks_repaired` as the rereads commit, so `chunks_repaired − chunks_reread` is the repair phase's remaining work; a chunk that still differs at its reread is counted here before the pass stops on it. |

There is no completion figure for the comparison. A pass sizes its chunks from the time each
one takes, so the chunk count is known only when the pass ends, and no row total describes the
comparison either: the copy step's `rows_total` is `reltuples` at the copy's start, `0` on a
table `ANALYZE` has never visited and short on a source that kept growing, so `rows_hashed`
over it is an estimate dressed as a ratio, not a measure. As with the copy, completion is the
step ending, and a rate comes from two snapshots of `rows_hashed` and `step_elapsed_ns`. The
repair phase does have a measure: once `chunks_repaired` has moved, `chunks_reread` /
`chunks_repaired` is the share of the rereads done.

The counters are the pass's, not the step's. Once the pass returns, `work` leaves the snapshot
with it: a terminal snapshot carries no `work`, and a poll that lands after the pass returned
and before the step ends carries none either. The pass's final figures — under `abort`, the
mismatches it found — are the `Report` or `Outcome` the pass returned to its caller, not the
last snapshot a poller happened to take.

## Phases

| Value | Meaning |
|---|---|
| `pending` | Execution has not started. |
| `running` | Execution is active. |
| `finished` | Terminal: completed successfully. |
| `failed` | Terminal: reached a terminal failure. |

A terminal snapshot is immutable: once `finished` or `failed` is observed, every later poll
returns the identical snapshot, elapsed values included.

## Operations

| Value | Meaning |
|---|---|
| `admitting` | A sequence's steps are still being validated; no statement has run yet. |
| `optimistic` | One bounded direct native attempt. |
| `brief` | A brief transactional sequence step. |
| `validate-constraint` | A constraint-validation scan. |
| `concurrent-index-build` | A concurrent index build (`work` is server-observed). |
| `copy` | The copy-and-swap row copy from the source table into its shadow (`work` is engine-measured). |
| `checksum` | The copy-and-swap checksum pass comparing the source table with its shadow and, under the `repair` policy, recopying differing chunks (`work` is engine-measured). |

## Polling semantics

The tracker is caller-owned and has no goroutines or timers: polling lifetime is exactly the
caller's context. A poll during an active concurrent index build performs one read of the
server's progress view over the executor's reserved session; a poll during a `copy` or
`checksum` step asks the engine's work source once; every other poll is pure memory. On a query or source
error the returned snapshot still carries the last-known tracker state — `phase` is never
empty — with the error returned alongside for the caller to classify. Pollers serialize
against each other, so the reserved session and the work source each see one observation
at a time.

The handoffs that end a poll target's ownership of the step — stopping a build or a work
source, or replacing one with the other — wait for an observation in flight before they
return, so the engine never releases a session or the state a source reads while a poll is
still using it. That makes the work source part of the engine's stop path: its `Work` must
bound itself (memory reads, or catalog reads on a session with `statement_timeout` set) and
honour the poll's context, because a poll that returns only when its observer gives up
would stall the engine behind an observer that never does. A `Work` that reads the catalog
is a CO-9 read site — `pg_catalog`-qualified, tested under a shadowing `search_path`.

The tracker is also the operator's stop path for a running concurrent index build:
`Tracker.CancelBuild` signals the build's backend over the same reserved session, and only
while the build is active — the tracker never hands out the backend PID, so a caller cannot
hold one past the build's return and cancel whatever the pool next runs on that backend. The
signal itself runs under its own short deadline, detached from the caller's context: a caller
deadline expiring mid-signal must not tear down the session the build's failure verdict needs.
A caller whose context has already ended sends nothing and gets its context error back.

A nil return means the cancel request was *sent* to a backend the server, in the same
statement, had just reported active — not that the build has stopped, and not a guarantee
the build was still running when the signal arrived. The build's own return, with
`cancelled-externally`, is the confirmation. The reserved session's role must be able to
signal the build's backend (the same role, or a member of `pg_signal_backend`); otherwise
`pg_cancel_backend` raises an error, which `CancelBuild` returns wrapped — a permanent
condition of the role, not one a retry clears. `CancelBuild` refuses to signal blind:
`ErrBuildNotRunning` when the server shows no statement running on the backend (the build has
not reached the server yet, or has already finished), and `ErrBuildUnobservable` when the
server cannot say — the backend is hidden from this role, or activity tracking is off — so an
operator is never told to wait for a build that is in fact running. A build the caller's own
context ended reports `cancelled-by-caller`.

## Example

A poll during step 2 of a 3-step sequence, mid concurrent index build:

```json
{
  "format_version": 5,
  "phase": "running",
  "step": 2,
  "total_steps": 3,
  "elapsed_ns": 2750000000,
  "step_elapsed_ns": 750000000,
  "detail": {
    "operation": "concurrent-index-build",
    "statement": "CREATE INDEX CONCURRENTLY idx ON public.t (id)",
    "server_phase": "building index",
    "active": true,
    "attempt": 2,
    "current_locker_pid": 31337,
    "work": {
      "rows_copied": 0,
      "rows_total": 0,
      "bytes_copied": 0,
      "bytes_total": 0,
      "chunks_compared": 0,
      "rows_hashed": 0,
      "chunks_mismatched": 0,
      "chunks_repaired": 0,
      "chunks_reread": 0,
      "blocks_done": 11,
      "blocks_total": 40,
      "tuples_done": 7,
      "tuples_total": 21,
      "lockers_total": 3,
      "lockers_done": 1
    }
  }
}
```

A poll during step 2 of a 4-step copy-and-swap, mid row copy (`TestSnapshotJSONShapeForACopyStep`
pins it):

```json
{
  "format_version": 5,
  "phase": "running",
  "step": 2,
  "total_steps": 4,
  "elapsed_ns": 2750000000,
  "step_elapsed_ns": 750000000,
  "detail": {
    "operation": "copy",
    "statement": "INSERT INTO public.t_shadow (id) SELECT id FROM public.t WHERE id BETWEEN $1::bigint AND $2::bigint ON CONFLICT (id) DO NOTHING",
    "active": true,
    "work": {
      "rows_copied": 1200,
      "rows_total": 5000,
      "bytes_copied": 98304,
      "bytes_total": 409600,
      "chunks_compared": 0,
      "rows_hashed": 0,
      "chunks_mismatched": 0,
      "chunks_repaired": 0,
      "chunks_reread": 0,
      "blocks_done": 0,
      "blocks_total": 0,
      "tuples_done": 0,
      "tuples_total": 0,
      "lockers_total": 0,
      "lockers_done": 0
    }
  }
}
```

A poll during step 3 of a 4-step copy-and-swap, mid checksum pass under the `repair` policy —
three chunks compared, two found differing and recopied, one of the two reread so far
(`TestSnapshotJSONShapeForAChecksumStep` pins it):

```json
{
  "format_version": 5,
  "phase": "running",
  "step": 3,
  "total_steps": 4,
  "elapsed_ns": 2750000000,
  "step_elapsed_ns": 750000000,
  "detail": {
    "operation": "checksum",
    "active": true,
    "work": {
      "rows_copied": 0,
      "rows_total": 0,
      "bytes_copied": 0,
      "bytes_total": 0,
      "chunks_compared": 4,
      "rows_hashed": 3500,
      "chunks_mismatched": 2,
      "chunks_repaired": 2,
      "chunks_reread": 1,
      "blocks_done": 0,
      "blocks_total": 0,
      "tuples_done": 0,
      "tuples_total": 0,
      "lockers_total": 0,
      "lockers_done": 0
    }
  }
}
```
