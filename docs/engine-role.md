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
- **A new table belongs to the role that created it.** A shadow table created by the engine
  role would be owned by the engine role — which the cutover fidelity checklist (see
  [low-level-design.md](low-level-design.md)) would refuse to swap. The engine therefore
  runs `SET ROLE <owner>` before creating shadow objects, so they are born with the correct
  owner rather than repaired afterward.

## The tiers

Each tier includes everything above it. A schema change is admitted at the tier its plan
requires — nothing higher.

| Tier | Capability | Required access | Preflight check |
| --- | --- | --- | --- |
| 0 | Connect and resolve the target | `LOGIN`; `CONNECT` on the database; `USAGE` on the target schema (directly or via membership) | `has_database_privilege`, `has_schema_privilege(..., 'USAGE')` |
| 1 | In-place `ALTER TABLE` (the instant and fast native paths) | Inheritable **membership in the owning role** — sufficient on its own | `pg_has_role(current_user, <owner>, 'USAGE')` |
| 2 | Index builds: `CREATE INDEX [CONCURRENTLY]`, and the `ALTER TABLE` shapes that build one — `ADD CONSTRAINT UNIQUE` / `PRIMARY KEY` / `EXCLUDE` without `USING INDEX`, or `ADD COLUMN` with an inline `UNIQUE` / `PRIMARY KEY` | Tier 1 + **`CREATE` on the target schema** — table ownership alone is refused with `permission denied for schema` | `has_schema_privilege(..., 'CREATE')` |
| 3 | Copy-and-swap | Tier 2 + membership usable with `SET ROLE` (for owner-correct shadow objects); for logical-decoding CDC: `rds_replication` membership (Aurora/RDS) or the `REPLICATION` attribute (self-managed) | `pg_has_role(..., 'SET')` (16+; on 14–15 the Tier 1 `USAGE` check already proves `SET ROLE` access — membership options arrive in 16); `pg_has_role(current_user, 'rds_replication', 'MEMBER')` |
| 4 | Planner scratch database (execute-and-introspect) | A pre-provisioned `pg_sprite_scratch` owned by the engine role, **or** `CREATEDB` | *Not yet implemented* — a missing scratch database surfaces at scratch creation, not in the preflight |

Two cluster-level *facts* — settings, not grants — accompany Tier 3 and are checked in the
same preflight: `wal_level = logical` (`rds.logical_replication = 1` on Aurora/RDS, a
static parameter requiring a reboot), and free `max_replication_slots` /
`max_wal_senders` headroom.

## Provisioning

For a target whose tables are owned by `app_owner` in schema `app`:

```sql
-- Tier 1: owner-gated DDL through membership
GRANT app_owner TO pgsprite_engine;

-- Tier 2: index builds and shadow objects live in the schema
GRANT USAGE, CREATE ON SCHEMA app TO app_owner;  -- if the owner lacks it

-- Tier 3: only when copy-and-swap with logical decoding is in play (Aurora/RDS)
GRANT rds_replication TO pgsprite_engine;
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
use the same paths as any other table. A partitioned parent may run supported in-place changes,
but plans containing index-building steps are refused: PostgreSQL cannot create an index on a
partitioned parent concurrently, and pg-sprite does not yet implement the partition-aware
`CREATE INDEX ON ONLY` → per-partition `CREATE INDEX CONCURRENTLY` → `ATTACH PARTITION` flow.
