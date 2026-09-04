# The progress report contract

The progress snapshot is the machine-readable observation a caller receives when it polls a
running schema change through the `*WithProgress` executor entry points. It is the one JSON
shape an operator or orchestrator consumes to display or act on execution progress. This
document is the contract: the fields, the closed vocabularies, and the behavior required of
a consumer. The Go source of truth is `pkg/progress`; `TestSnapshotJSONShape` pins the exact
keys, including the example at the end of this page.

## Versioning: `format_version`

Every snapshot carries `format_version`. A consumer that does not recognize the version must
**reject the snapshot** — never guess at field semantics. The version covers more than the
field shape: the closed vocabularies below (phases, operations) are pinned to it. Adding a
phase or operation value is a contract change and bumps `format_version`, even if no field
is added or renamed.

Adding a field bumps `format_version` so a strict consumer can detect the new shape from the
version. The current version is **3**: version 2 added `detail.statement`; version 3 added
`detail.current_locker_pid`, `work.lockers_total`, and `work.lockers_done`.

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
| `work` | object | server-observed work only | Present exactly when the server published a progress row; then **every** counter below is present, so a fresh build reports honest zeros rather than an empty object. |

`statement` is the submitter's statement after qualification and canonicalization, so a
consumer rendering it into a shared surface must clamp and escape it.

### Work counters

`blocks_done` / `blocks_total`, `tuples_done` / `tuples_total`, and
`lockers_done` / `lockers_total` come from
`pg_stat_progress_create_index` during a concurrent index build. `rows_copied` /
`rows_total` and `bytes_copied` / `bytes_total` are reserved for copy-and-swap and are `0`
on every native operation — the engine never fabricates copy counters.

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
| `concurrent-index-build` | A concurrent index build (the one operation with server-observed `work`). |

## Polling semantics

The tracker is caller-owned and has no goroutines or timers: polling lifetime is exactly the
caller's context. A poll during an active concurrent index build performs one read of the
server's progress view over the executor's reserved session; every other poll is pure
memory. On a query error the returned snapshot still carries the last-known tracker state —
`phase` is never empty — with the error returned alongside for the caller to classify.

## Example

A poll during step 2 of a 3-step sequence, mid concurrent index build:

```json
{
  "format_version": 3,
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
