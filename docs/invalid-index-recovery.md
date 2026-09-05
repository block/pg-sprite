# Invalid-index recovery

This is the operator runbook for the native path's invalid-index outcomes: an invalid
index (`pg_index.indisvalid = false`) that the executor found, or left, under the name a
`CREATE INDEX CONCURRENTLY` asked for. The executor's typed outcome
(`*executor.InvalidIndexError`) points here; this page says what each state means, which
states the executor recovers from on its own when asked, and what the remaining ones
license you to do — and, just as importantly, what they do not.

## What an invalid index is

A `CREATE INDEX CONCURRENTLY` build that fails after registering its catalog entry leaves
the index in place, marked invalid. An invalid index is pure cost: **every write still
maintains it, no query ever uses it**. It cannot heal on its own — it stays invalid until
someone drops it or rebuilds it (`REINDEX INDEX CONCURRENTLY`). The same marking is also,
transiently, the normal state of a **healthy build still in progress**: an in-flight
`CREATE INDEX CONCURRENTLY` is invalid until it finishes. That ambiguity is the entire
reason this page exists — "invalid" alone does not tell you whether the entry is abandoned
debris or someone's build that is about to succeed.

## How the executor tells the states apart

The build itself never drops anything. Before it starts, it reads one catalog snapshot
for an invalid index under the requested name in the target schema and classifies it by
proof strength, strongest first:

1. **A backend is visibly building it** — `pg_stat_progress_create_index` names the
   entry's OID. The build is in flight; nothing here is debris.
2. **It sits on a different table** — `pg_index.indrelid` is not the target table. It
   is not this change's to remove, whatever its state.
3. **This role cannot see whether it is being built** — `pg_stat_progress_create_index`
   hides the target of another role's command from a reader without
   `pg_read_all_stats` (the row is there, its `index_relid` is `NULL`), and records
   nothing at all when the builder runs with `track_activities` off. A silent progress
   view is only evidence when nothing is hidden and tracking is on.
4. **It is abandoned** — on the target table, no visible builder, nothing hidden,
   tracking on. Nobody is building it; nobody ever will.

After its own failed build, the executor proves its backend stopped and applies the same
reading to whatever it finds under the build's name.

## The states

| State (`Cleanup` sentinel) | Outcome code | Executor recovers? | What you do |
| --- | --- | --- | --- |
| This build's own leftover (`ErrBuildLeftInvalidIndex`) | `invalid-index-own-leftover` | **Yes** — `RebuildAbandonedIndex` | Call the recovery, or drop it yourself (below) |
| Another backend's build in flight (`ErrInvalidIndexBuildInFlight`) | `invalid-index-build-in-flight` | No — refuses | **Wait.** The error names the backend PID |
| Abandoned on the target table (`ErrAbandonedInvalidIndex`) | `invalid-index-abandoned` | **Yes** — `RebuildAbandonedIndex` | Call the recovery |
| On a different table (`ErrInvalidIndexOnOtherTable`) | `invalid-index-other-table` | No — refuses | Recover it from that table's own change, or inspect it yourself |
| Builder unobservable by this role (`ErrInvalidIndexBuilderUnobservable`) | `invalid-index-builder-unobservable` | **Yes** — `RebuildAbandonedIndex` proves it under the table lock | Call the recovery, or grant the role `pg_read_all_stats` and retry the build |
| Unproven (anything else, incl. `ErrAbandonmentUnproven`, `ErrTargetIdentityChanged`) | `invalid-index-unproven` | No — fails closed | Investigate before touching anything (below) |

`(*InvalidIndexError).Recoverable()` is the programmatic form of the third column.

## The automatic recovery: `RebuildAbandonedIndex`

`executor.RebuildAbandonedIndex` takes exactly the statement `BuildIndexConcurrently`
takes, under the same budget and pool, and is the recovery the recoverable states name.
It is safe because the removal is **proven, not named** — PostgreSQL drops by name, and a
name alone can never prove which entry it will hit:

1. Under a bounded `SHARE UPDATE EXCLUSIVE` lock on the target table — the lock every
   concurrent index command (`CREATE`, `DROP`, `REINDEX ... CONCURRENTLY`) holds for its
   whole life, so holding it proves no build, drop, or reindex of any index on the table
   is in flight, *including one this role could not see* — the entry is re-verified by
   OID (still under the requested name, still invalid, still on this table, no visible
   builder) and renamed to `pgsprite_abandoned_<oid>`. The lock is bounded; if it is not
   granted within the bound, the recovery reports a `*BudgetError` (`CauseLock`) and
   touches nothing. That is what a hidden in-flight build looks like from here: it holds
   the lock, so the proof cannot be taken, so nothing is dropped.
2. The rename commits, and every invalid index on the table whose name is *exactly* its
   own quarantine name is dropped with `DROP INDEX CONCURRENTLY`, each verified by OID
   before and after. The quarantine name can belong to nothing but the entry it was
   derived from, so a drop by that name is a drop by identity. Because the sweep keys on
   the name pattern, a recovery that dies between rename and drop leaves debris the next
   recovery removes on its own; an operator's own index that merely starts with the
   prefix is never matched.
3. The requested build runs exactly as `BuildIndexConcurrently`.

The recovery refuses, touching nothing, on a visible in-flight build or another table's
entry, and fails closed with `ErrAbandonmentUnproven` if the entry changes between two
verification points or a drop leaves it in place. It never removes a valid index: a valid
index under the name is not debris, the recovery has nothing to remove, and the build
fails on the name exactly as it would without the recovery.

The `IndexRecoveryReport` it returns lists what it dropped (schema, quarantine name, OID,
drop duration) and carries the verified build report.

## Recovering by hand

### This build's own leftover (`ErrBuildLeftInvalidIndex`)

The executor proved it: the build failed (or reported success but failed the validity
verification), the build's own backend provably stopped, and one catalog snapshot then
found an invalid index under the build's name on the exact table (by OID) the build was
admitted against, with no builder observable. No other actor can have an in-flight build
under this name — the invalid entry itself occupies the name.

`RebuildAbandonedIndex` is the recovery. The manual equivalent is the statement the
error names:

```sql
DROP INDEX CONCURRENTLY "schema"."index";
```

`CONCURRENTLY` matters: a plain `DROP INDEX` takes an `ACCESS EXCLUSIVE` lock on the
table and blocks all reads and writes while it waits. After the drop, the original change
can be retried.

### Another backend's build is in flight (`ErrInvalidIndexBuildInFlight`)

The error names the backend PID. Dropping the entry would destroy a healthy build that may
be hours in. **Wait** — either for it to finish (the entry becomes valid and your change
becomes a no-op or a name collision) or for it to fail (the entry becomes abandoned debris
and the next attempt recovers it). To watch it:

```sql
SELECT p.pid, p.phase, p.blocks_done, p.blocks_total, a.query_start
  FROM pg_stat_progress_create_index p
  JOIN pg_stat_activity a ON a.pid = p.pid
 WHERE p.index_relid = 'schema.index'::regclass;
```

### The entry is on a different table (`ErrInvalidIndexOnOtherTable`)

An invalid index under the requested name exists in the target schema, but on another
table (the error names it). It is not this change's to remove: recover it from that
table's own change, or — as a deliberate human decision, knowing your environment's
actors — confirm no build is running on it and drop it with `DROP INDEX CONCURRENTLY`.

```sql
SELECT n.nspname, c.relname, i.indisvalid, t.relname AS table_name
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  JOIN pg_class t ON t.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'schema' AND c.relname = 'index';
```

### This role cannot see the builder (`ErrInvalidIndexBuilderUnobservable`)

The entry is on the target table and no builder is *visible*, but the executor's role
either saw a progress row it could not read (another role's command, no
`pg_read_all_stats`) or runs with `track_activities` off, so the silence proves nothing.
Two ways forward:

- `RebuildAbandonedIndex` does not need to see the builder: a hidden build holds the
  table lock, so the proof lock is either granted (nobody is building — the entry is
  removed) or reported as a lock budget (somebody is — nothing is touched).
- Grant the executor's role `pg_read_all_stats` and retry the build; the outcome becomes
  in-flight or abandoned.

### The catalog state could not be proven (`invalid-index-unproven`)

Everything else fails closed into this state: the build's backend could not be proven
stopped (the verdict's deadline expired), the catalog inspection itself failed, the target
table was dropped or replaced while the build ran (`ErrTargetIdentityChanged`), or a
recovery found the entry changed between two of its verification points
(`ErrAbandonmentUnproven`). The executor is telling you it knows nothing beyond "a
leftover may exist" — the index under that name, if any, may equally be valid and in
production use.

Do not drop anything on the strength of this error. Run the queries above yourself:
establish whether an index under the name exists and whether it is invalid, then whether
any live session is building it. Only a confirmed invalid entry with no live build is a
candidate for `DROP INDEX CONCURRENTLY` — and if the target table was replaced, first
establish which table the entry actually sits on (`indrelid`). An entry left under a
`pgsprite_abandoned_<oid>` name is a quarantined entry whose drop did not complete; the
next `RebuildAbandonedIndex` on that table sweeps it.

## Why not just retry?

The executor refuses to build over an invalid entry under its requested name, so a retry
without recovery returns the same outcome. This is deliberate: a fresh build cannot reuse
the name while the entry occupies it, and removing the entry needs the proof
`RebuildAbandonedIndex` carries — a build that dropped by name on its way in could take
another actor's index with it.
