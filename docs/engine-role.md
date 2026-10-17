# The engine role: what access schema changes actually need

This is the provisioning contract for the PostgreSQL role pg-sprite connects as — the
**engine role**. It answers one question precisely: *what is the minimum access a database
user needs to run schema changes against tables it does not own?* Every requirement here is
mechanically checkable, and the engine's preflight refuses with the exact missing `GRANT`
rather than failing mid-change.

The contract is deliberately tiered: a team that only ever needs in-place `ALTER TABLE` and
online index builds should not be asked to provision replication access it will never use.

## Why ownership, not privileges

PostgreSQL has no grantable "ALTER" privilege. `ALTER TABLE`, `CREATE INDEX` against a
table, and the rename-swap of copy-and-swap are all **owner-gated**: they require the
current role to *be* the table's owner — or to be a **member of the owning role**, because
ownership checks pass through role membership. Membership is therefore the mechanism this
contract is built on:

```sql
GRANT app_owner TO pgsprite_engine;
```

is the entire trick. The engine role never logs in as the application's user, never needs
the owner's password, and losing the membership fails closed — the next owner-gated
statement is refused by the server with `must be owner of table ...`.

Two facts about created objects complete the picture (both verified against a live server;
the version matrix in [postgresql-version-support.md](postgresql-version-support.md)
applies):

- **An index belongs to the table's owner**, regardless of which member role created it —
  the native index path is ownership-correct automatically.
- **A new table belongs to the role that created it.** The copy-and-swap design therefore
  calls for `SET LOCAL ROLE <owner>` before creating shadow objects so they are born with the
  correct owner rather than repaired afterward. That path is planned; greenfield creation
  with a create owner (below) is the first path that actually runs `SET LOCAL ROLE`.

## The tiers

Each tier includes everything above it. A schema change is admitted at the tier its plan
requires — nothing higher.

Before it classifies an `ADD COLUMN`, the engine adds each column to an empty temporary table
with no columns (`CREATE TEMPORARY TABLE ... ()`) inside a transaction it always rolls back, to
learn from the server whether the addition rewrites the table. Whether it does depends only on
the added column's type and default, so the probe itself never opens the live table: it takes
no lock on it and needs no privilege on it. Adding the column does evaluate its default and its
domain's checks once, as the real `ALTER TABLE` would, and a function either one calls may read
a table another session has locked; the probe never waits for such a lock, and plans the
column as an `unverified` rewrite instead. A column that rewrites is added again with its type
alone, which tells whether its type or its default is what rewrites, and the plan's reason names
that cause. An `ADD COLUMN IF NOT EXISTS` for a column the table already has is
read from the catalog and not probed, since the server skips it. The probe does need `TEMPORARY` on the database and a server that
accepts writes, so an `ADD COLUMN` cannot be planned on a hot standby or by a role whose
`TEMPORARY` was revoked from `PUBLIC` and not granted back. The plan fails with
`preflight.ErrAddColumnProbeUnavailable` rather than guessing, naming which of the two to fix.

The per-table session lock uses `pg_try_advisory_lock`, which is available to every role and
requires no additional grant in any tier. Naming the holder of a contended lock reads
`pg_stat_activity`; a role that cannot see other sessions still gets the holder's backend PID.

| Tier | Capability | Required access | Preflight check |
| --- | --- | --- | --- |
| 0 | Connect and resolve the target | `LOGIN`; `CONNECT` on the database; `USAGE` on the target schema (directly or via membership) | `has_database_privilege`, `has_schema_privilege(..., 'USAGE')` |
| 1 | In-place `ALTER TABLE` (the instant and fast native paths) | Inheritable **membership in the owning role**; an `ADD COLUMN` also needs `TEMPORARY` on the database, which PostgreSQL grants to `PUBLIC` by default | `pg_has_role(current_user, <owner>, 'USAGE')` |
| 2 | Index builds: `CREATE INDEX [CONCURRENTLY]`, and the `ALTER TABLE` shapes that build one — `ADD CONSTRAINT UNIQUE` / `PRIMARY KEY` / `EXCLUDE` without `USING INDEX`, or `ADD COLUMN` with an inline `UNIQUE` / `PRIMARY KEY` | Tier 1 + **`CREATE` on the target schema** — table ownership alone is refused with `permission denied for schema` | `has_schema_privilege(..., 'CREATE')` |
| 3 | Copy-and-swap | Tier 2 + membership usable with `SET ROLE` (for owner-correct shadow objects); for logical-decoding CDC: `rds_replication` membership (Aurora/RDS) or the `REPLICATION` attribute (self-managed) | `pg_has_role(..., 'SET')` (16+; on 14–15 the Tier 1 `USAGE` check already proves `SET ROLE` access — membership options arrive in 16); `pg_has_role(current_user, 'rds_replication', 'MEMBER')` |

Copy-and-swap uses the empty shadow plus the transaction-scoped `pkg/schemadiff` scratch schema
for execute-and-introspect; it does not add a higher privilege tier or require `CREATEDB`. See
[the D1 decision](copy-and-swap-design.md#d1--no-durable-scratch-database). The scratch schema
does need `CREATE` **on the database** (`CREATE SCHEMA` is a database-level privilege) — a
requirement of every declarative plan, not only copy-and-swap, that the tier table does not yet
carry and preflight's privilege probe does not yet check; both are open follow-ups. The
checkpoint table ([D3](copy-and-swap-design.md#d3--store-checkpoints-in-the-target-database))
lives in an engine-owned `pgsprite` schema created on first use, so a copy-and-swap run needs
the same database-level `CREATE` once, and `pkg/checkpoint` reports the server's refusal as is
when the engine lacks it. A deployment that keeps database-level `CREATE` away from the engine
role can pre-provision instead: have a role that holds `CREATE` run the engine's first use
once, which creates the `pgsprite` schema and the `pgsprite.pgsprite_checkpoint` table, then
`ALTER SCHEMA pgsprite OWNER TO <engine>` and `ALTER TABLE pgsprite.pgsprite_checkpoint OWNER TO
<engine>`. `Ensure` creates only what is absent, so it issues no `CREATE` against objects that
already exist, and it refuses — rather than adopts — a schema or table that another role still
owns, because anything that role hangs off them would run with the engine's privileges on every
checkpoint write. The one DDL `Ensure` does issue against an existing object is on its own
table: after an engine upgrade that added a checkpoint column, the first `Ensure` runs
`ALTER TABLE pgsprite.pgsprite_checkpoint ADD COLUMN …` for each column the table lacks, which
is why the engine role must keep owning the table rather than merely writing to it. That
`ALTER TABLE` takes `ACCESS EXCLUSIVE` on the checkpoint table — no other table — for the
duration of the add, bounded by the session's `lock_timeout`; a run of another engine instance
reading or writing its row at that moment waits or times out, and a timed-out `Ensure` has
committed nothing and is safe to run again. The window is the first start after an upgrade,
not every start.

### The write fence

A quiesced copy-and-swap run (`pkg/schemachange.Runner`) does not take the caller's word that
nothing writes the table: it installs a statement-level `BEFORE INSERT OR UPDATE OR DELETE OR
TRUNCATE` trigger, `pgsprite_write_fence`, on the source before its first checkpoint save, and
every write the trigger sees is refused with SQLSTATE `55006` (`object_in_use`) and a message
naming the table. The trigger calls a function of the same name in the engine's own `pgsprite`
schema, created once per database as the engine role and owned by it, so the table's owner
needs no access to that schema for the fence to fire. The fence stays on a stopped run's source
until the run is resumed or abandoned (`DropWriteFence`); a completed run carries it to the
retained `_old` table with the swap, where it is dropped with the table or, under `KeepOld`,
taken off so the retained table can be renamed back and take writes at once.

What this needs: `CREATE TRIGGER` on a table requires the `TRIGGER` privilege on it or
ownership, which the Tier 1 membership in the owning role already gives; the function needs
`CREATE` on the `pgsprite` schema, which the engine owns. A pre-provisioned deployment that
created the schema for the engine needs nothing further once the schema is owned by the engine
role. The fence does not stop a superuser, nor a session running under
`session_replication_role = replica`, which fires no ordinary triggers; a deployment whose
writers can hold either has to quiesce the table by its own means, and a difference the
verification pass then finds is reported, never repaired. Before `Run`, quiesce the table as far
as the application allows — stop or pause its writers, or route them around it — so the fence
refuses nothing in practice; the fence is the proof, not the plan.

Two cluster-level *facts* — settings, not grants — accompany Tier 3 and are checked by
`preflight.CheckCopySwapEnvironment` once the privilege check has passed and the shape check
has minted its `CopySwapShape`; the environment check is what mints the `CopySwapTarget`
every writing step requires: `wal_level = logical` (`rds.logical_replication = 1` on Aurora/RDS, a
static parameter requiring a reboot), one free slot under `max_replication_slots`, and two free
senders under `max_wal_senders` — the slot's connection holds its exported snapshot while the
copy imports it, and the stream decodes on a connection of its own. Both apply only to a run
that decodes WAL; a quiesced run skips them. The same check compares the free disk the
caller measured on the database volume — PostgreSQL has no function that reports it —
against the shadow copy's headroom, and
refuses an unmeasured volume rather than assuming it is large enough. The figure is the
bytes the volume can still absorb before writes fail: on a fixed-size volume, the free
space the host reports for the data directory; on a managed volume that grows on demand
(an Aurora cluster volume), the distance to the service's storage limit or to the quota
the operator set — the shadow copy and the WAL a replication slot pins both land there.
Never pass a sentinel such as the largest integer; that disables the only check that stands
between the copy and a full volume. Each refusal carries its own cause
([refusal-classes.md](refusal-classes.md#copy-and-swap-preflight-refusals-keyed-on-copyswaprefusalcause)).

### Off-ladder: greenfield `CREATE TABLE`

Creating a new table sits outside the ladder. By default the engine role creates and owns it;
this owner-less mode deliberately preserves the original behavior. When the embedding caller
names a create owner (`migrate.Options.CreateOwner`; the CLI has no declarative apply command
yet), each bounded create step runs `SET LOCAL ROLE <owner>` before its SQL. Preflight proves the
engine can assume that role — membership (`pg_has_role(..., 'MEMBER')`, which is what `SET ROLE`
consults; a `NOINHERIT` member qualifies), plus the `SET` membership option on PostgreSQL 16+ —
that the owner has `USAGE` and `CREATE` on the schema, and that the engine has `CONNECT`.
Tables, serial sequences, and indexes are consequently born under the owner, so that owner's
default privileges apply. Missing access is refused with the exact `GRANT` (`GRANT <owner> TO
<engine>`, `WITH SET TRUE` on 16+); a named owner that is not a role on the server is refused
before anything runs. After the `CREATE TABLE` commits, the executor reads the table's catalog
owner back and fails closed (`create-owner-mismatch`, or `create-owner-unverified` when the
read does not complete) if it is not the named owner; it never repairs ownership with
`ALTER TABLE ... OWNER TO`.

One owner per target, not per schema, is deliberate: a per-schema map would mirror
schema_overrides in orchestrators and is left until a target with differently-owned schemas
needs it.

## Provisioning

For a target whose tables are owned by `app_owner` in schema `app`:

```sql
-- Tier 1: owner-gated DDL through membership
GRANT app_owner TO pgsprite_engine;

-- Tier 2: index builds and shadow objects live in the schema
GRANT USAGE, CREATE ON SCHEMA app TO app_owner;  -- if the owner lacks it

-- Tier 3: shadow objects are created under the owner, so the membership
-- must be usable with SET ROLE. The Tier 1 grant above already is: SET is
-- the default. On PostgreSQL 16+ only, a grant can be made WITH SET FALSE,
-- which fails this tier; the 16+ spelling of the default is
--   GRANT app_owner TO pgsprite_engine WITH SET TRUE;
-- and is a syntax error on 14 and 15.

-- Tier 3, only when copy-and-swap decodes WAL: one of
GRANT rds_replication TO pgsprite_engine;          -- Aurora/RDS
ALTER ROLE pgsprite_engine WITH REPLICATION;       -- self-managed

-- Not grants, but checked alongside them for a run that decodes WAL:
--   wal_level = logical          (rds.logical_replication = 1 on Aurora/RDS; both need a restart;
--                                 another managed service's own switch for it is not detected)
--   no slot of the name derived for the table held by another database or as a physical slot
--   one free slot under max_replication_slots and two free senders under max_wal_senders
--                                 (the slot's snapshot connection and the decoding stream)
```

The contract covers the target table's own access. A `FOREIGN KEY` that references a
table owned by a *different* role additionally needs `REFERENCES` on the referenced
table (`GRANT REFERENCES ON <referenced> TO <owning role>`), which the preflight does
not check — where every table shares one owning role, the owner already holds it.

One membership grant per owning role: a database where every schema is owned by one
application role needs exactly one `GRANT`. This contract relies on the membership being
inheritable and `SET ROLE`-capable; grants issued `WITH SET FALSE` or to a `NOINHERIT`
engine role break Tiers 1 and 3, and the preflight refuses them naming the statement that
actually repairs the option — `GRANT ... WITH INHERIT TRUE` / `WITH SET TRUE` on
PostgreSQL 16+, `ALTER ROLE ... INHERIT` on 14–15, where inheritance is a role attribute
that `GRANT` cannot change.

## What the engine role must not have

The contract is as much about what is absent:

- **No superuser.** Aurora does not offer it, and nothing here needs it.
- **No `rds_superuser`.** It does not bypass ownership checks and adds unrelated power.
- **No application login.** The engine role is its own identity; audit logs distinguish
  engine DDL from application traffic.
- **No ownership transfer.** The application role keeps owning its objects before, during,
  and after every schema change; the engine borrows owner rights through membership and
  `SET ROLE`, both revocable with one statement.
- **No `GRANT OPTION` / no `CREATEROLE`.** The engine never grants anything to anyone.

## How the engine enforces this

Preflight resolves the target table's owner from the catalog (`pg_class.relowner`), then
evaluates the tier checks for the change's planned strategy. A missing requirement is a
typed refusal naming the exact `GRANT` statement that would satisfy it — the same
fail-closed posture as every other refusal in the engine, and the reason this page exists:
the refusal points here, and this page says what to provision and why it is safe.

Preflight also records the target's relation kind. Leaf partitions are ordinary tables and
use the same paths as any other table. A partitioned parent may run supported in-place changes;
the parent-specific refusal boundaries and their rationale are documented in
[current limitations](limitations.md).
