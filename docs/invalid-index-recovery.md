# Invalid-index recovery

This is the operator runbook for the one native-path outcome that needs a human: an
invalid index (`pg_index.indisvalid = false`) that the executor found, or may have left,
and will not remove. The executor's typed outcome (`*executor.InvalidIndexError`) points
here; this page says what each state licenses you to do — and, just as importantly, what
it does not.

## What an invalid index is

A `CREATE INDEX CONCURRENTLY` build that fails after registering its catalog entry leaves
the index in place, marked invalid. An invalid index is pure cost: **every write still
maintains it, no query ever uses it**. It cannot heal on its own — it stays invalid until
someone drops it or rebuilds it (`REINDEX INDEX CONCURRENTLY`). The same marking is also,
transiently, the normal state of a **healthy build still in progress**: an in-flight
`CREATE INDEX CONCURRENTLY` is invalid until it finishes. That ambiguity is the entire
reason this page exists — "invalid" alone does not tell you whether the entry is abandoned
debris or someone's build that is about to succeed.

The executor never drops an index itself: PostgreSQL drops by name, not identity, so an
automatic drop could destroy another actor's index registered under the same name in the
same window. It holds operator advice to the same standard — a drop statement is named
only in the one state where ownership is proven.

## The three states

### The entry is this build's own leftover (`ErrBuildLeftInvalidIndex`)

The executor proved it: the build failed (or reported success but failed the validity
verification), the build's own backend provably stopped, and one catalog snapshot then
found an invalid index under the build's name in the target schema, with the target table
still the exact table (by OID) the build was admitted against. No other actor can have an
in-flight build under this name — the invalid entry itself occupies the name.

Recovery is the statement the error names:

```sql
DROP INDEX CONCURRENTLY "schema"."index";
```

`CONCURRENTLY` matters: a plain `DROP INDEX` takes an `ACCESS EXCLUSIVE` lock on the
table and blocks all reads and writes while it waits. After the drop, the original change
can be retried.

### An invalid index with this name already existed (`ErrPreexistingInvalidIndex`)

The executor refused to build because an invalid index under the requested name already
existed in the target schema — on any table. It cannot prove whose it is. Two very
different situations produce this state, and **one of them must not be dropped**:

- **Another actor's build is still running.** An in-flight `CREATE INDEX CONCURRENTLY`
  is invalid until it finishes. Dropping it destroys a healthy build that may be hours
  in. Check for one first:

  ```sql
  SELECT a.pid, a.state, a.query_start, a.query
    FROM pg_stat_activity a
   WHERE a.query ILIKE '%CREATE%INDEX%CONCURRENTLY%'
     AND a.state <> 'idle';
  ```

  If a build for this index is running, the answer is **wait** — either for it to finish
  (the entry becomes valid and your change becomes a no-op or a name collision) or for it
  to fail (the entry becomes true debris and the owner should clean it up).

- **It is abandoned debris from an earlier failure** (this executor's or anyone else's).
  With no live build touching it, an invalid entry cannot become valid on its own:

  ```sql
  SELECT n.nspname, c.relname, i.indisvalid, t.relname AS table_name
    FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    JOIN pg_class t ON t.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = 'schema' AND c.relname = 'index';
  ```

  Confirm the index sits on the table you expect, confirm no build is running, and then —
  as a deliberate human decision, knowing your environment's actors — drop it with
  `DROP INDEX CONCURRENTLY` and retry the change.

### The catalog state could not be proven (indeterminate)

Everything else fails closed into this state: the build's backend could not be proven
stopped (activity tracking off, a hidden backend, the verdict's deadline expired), the
catalog inspection itself failed, or the target table was dropped or replaced while the
build ran (`ErrTargetIdentityChanged`). The executor is telling you it knows nothing
beyond "a leftover may exist" — the index under that name, if any, may equally be valid
and in production use.

Do not drop anything on the strength of this error. Run both queries above yourself:
establish whether an index under the name exists and whether it is invalid, then whether
any live session is building it. Only a confirmed invalid entry with no live build is a
candidate for `DROP INDEX CONCURRENTLY` — and if the target table was replaced, first
establish which table the entry actually sits on (`indrelid`).

## Why not just retry?

The executor refuses to build over an invalid entry under its requested name, so a retry
without recovery returns the same outcome. This is deliberate: after a failure of its own
it could never distinguish pre-existing debris from its own leftover, and a fresh build
cannot reuse the name while the entry occupies it.
