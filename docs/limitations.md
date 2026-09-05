# Current limitations

pg-sprite refuses a schema change when it cannot provide its online-safety
guarantees. This page explains the *mechanics* of each current refusal; the
complete support matrix — every operation and object type, tiered as
supported / planned / out of scope, with reasons — is
[capabilities.md](capabilities.md). These are current capability boundaries,
not escape hatches:

| Change | Current behavior |
| --- | --- |
| Index builds on a partitioned parent | PostgreSQL cannot build an index concurrently at the parent level. PostgreSQL supports a plain blocking build, but pg-sprite refuses it by policy because it takes `ACCESS EXCLUSIVE`; `--force` does not bypass this decision. An operator who chooses a maintenance-window blocking build must run it outside pg-sprite. The partition-aware `CREATE INDEX ON ONLY` → per-partition `CREATE INDEX CONCURRENTLY` → `ATTACH PARTITION` flow is planned but not yet implemented. |
| `ADD CONSTRAINT ... USING INDEX` on a partitioned parent | PostgreSQL does not support adopting an existing index on a partitioned parent in any supported version. pg-sprite refuses before execution. |
| `ADD FOREIGN KEY ... NOT VALID` on a partitioned parent | PostgreSQL does not support this before version 18, so pg-sprite refuses it on versions 14–17. It is supported on version 18 and later. |
| Copy-and-swap | The copy-and-swap backend is not yet available. Statements that require it route to `refuse`; pg-sprite never falls through to a blocking rewrite. |

## Declarative model boundaries

The declarative front door (desired-state files, `diff`, and schema export)
models **one ordinary table plus its indexes**. These are the boundaries of
that model today. Wherever an operation meets a boundary, it fails closed
with a typed refusal — never a silently wrong or incomplete result:

| Not modeled | Current behavior |
| --- | --- |
| Foreign keys (either direction) | Unsupported in the declarative model, in both directions. A desired file cannot declare a `REFERENCES` clause (refused at parse), and export refuses both sides of a foreign-key relationship: a table whose definition carries foreign-key constraints surfaces the parse gate's typed error, and a table that other tables reference refuses with its own typed error — a single-table baseline cannot carry incoming foreign-key topology, so rendering one would silently drop the relationship. Foreign-key **DDL is still supported through the statement front door** (`ADD FOREIGN KEY` routes to the online `NOT VALID` + `VALIDATE` sequence). Because a desired file can never declare a foreign key, `diff` on a live table that carries one plans a **destructive** `DROP CONSTRAINT` for it — gated like every destructive change, never auto-executed — and incoming foreign keys are invisible to a single-table diff entirely, so tables participating in foreign-key relationships in either direction should not be managed declaratively yet. |
| Partitioned tables | Partitioned parents and their partitions are introspectable, and the statement front door supports in-place changes on them (see the partitioned-parent rows above), but they cannot be expressed in or exported to a desired file: the model captures the partition key and attachment only to refuse — it does not carry partition bounds or the parent/partition topology. A partitioning mismatch between live and desired is a typed `diff` refusal, never a zero diff. |
| Classic table inheritance (`INHERITS`) | Both parents and children are refused during export. The model records classic inheritance edges in both directions only to fail closed: rendering a child would flatten inherited columns and rendering a parent would omit its children. Declarative partitions share `pg_inherits` but remain classified by `relispartition` and use the partition refusal above. |
| Unlogged tables | Persistence is not managed: converging it (`SET LOGGED` / `SET UNLOGGED`) is a full table rewrite. Export refuses an unlogged table — a plain `CREATE TABLE` baseline would silently change its crash-safety and replication behavior — and a persistence mismatch between live and desired is a typed `diff` refusal, never a zero diff. |
| Table and column comments | Comments are metadata outside the table-shape model. Desired files and exports do not carry them; manage them with owner tooling. |
| Storage parameters (`fillfactor`, etc.) | Storage parameters are not represented in desired files or exports. Manage them separately until the declarative model can classify their convergence and rewrite implications. |
| Column collations | An explicit `COLLATE` on a column is not managed: converging a collation delta rewrites the column and its indexes. Export refuses a collated column — a baseline without the clause would silently change sort order and index semantics — and a collation delta (including on an added column) is a typed `diff` refusal. |
| Non-table objects | Views, materialized views, standalone sequences, enums, domains, extensions, functions, and triggers are outside the model. A serial column's owned sequence is the one exception: it round-trips through the `serial` pseudo-types — and ownership is verified through the catalog (`pg_depend`), so a hand-written `nextval` default on a standalone sequence that merely carries the serial-style name refuses rather than exporting as `serial` and silently privatizing a shared sequence. A column may *use* an unmanaged type (an enum, a domain) — the type text round-trips — but the type's definition is not managed. |
| Multiple tables per file | A desired file is single-table scoped: exactly one `CREATE TABLE` plus `CREATE INDEX` statements on it. Multi-table schemas are managed as one file per table. |
| Live tables with no desired file | Not in `diff`'s view: the diff is single-table scoped, so it never plans or executes a `DROP TABLE`, and the imperative front door refuses the statement as unsupported. The set of tables a schema should contain is the whole-schema owner's to enumerate and reconcile — see [Deliberately operator-owned](capabilities.md#deliberately-operator-owned) for the catalog query and its exclusions. |
| Greenfield table creation | Reachable through desired-state execution (`migrate.RunDesired`, library-only today — no CLI verb): a plan whose table does not exist routes to the executor create path (`executor.ExecuteCreate`). Planning refuses every clause that binds to an existing object the absence proof does not cover (`PARTITION OF`, `INHERITS`, `LIKE`, and `OF`), plus `IF NOT EXISTS` and duplicate claimed relation names. Apply re-checks the same shape rules before anything executes. `REFERENCES` and `CONCURRENTLY` are refused upstream at desired-file parse (`statement.ParseDesired`); the create path's admission re-checks them as defense in depth. |
| Changed index or constraint definition | A redefinition diffs to drop-and-recreate, the drop is destructive, and desired-state execution refuses any plan containing a destructive statement — the whole plan, including the harmless recreate. Run the drop deliberately first (`DROP INDEX CONCURRENTLY` directly against the database; `ALTER TABLE ... DROP CONSTRAINT` through the imperative front door), then rerun — the remaining plan converges the recreate. |

## What desired-state execution converges today

Desired-state execution (`migrate.RunDesired`, library-only today) feeds
every planned statement back through the same gates as the imperative
front door, so the outcome of an ordinary desired-file edit is the
composition of the model boundaries above with those gates. At a glance:

| Desired-file edit | Outcome today |
| --- | --- |
| A desired file whose table does not exist yet | Converges — the greenfield create path verifies the table name and every relation name the desired file states (explicit index names and first-choice constraint-index and column-sequence names) are free and the role holds `CREATE` on the schema, then runs the `CREATE TABLE` and index builds as brief bounded steps. Names the server invents rather than names the desired file states are outside this coverage. An occupied claimed name is a typed `create-collision` refusal before anything runs — drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column. Duplicate-name SQLSTATEs backstop races for explicit names; for server-chosen names, the probe narrows the race to the time-of-check window, but nothing catches a name taken inside it. Unsupported create shapes (`PARTITION OF`, `INHERITS`, `LIKE`, `OF`, `IF NOT EXISTS`) and duplicate claimed names refuse at plan time and are re-checked at apply. |
| Add a column | Converges. Runs as a bounded attempt of the submitted form, so the table-size guard applies (below). |
| Widen a column type (`varchar(50)` → `varchar(255)`) | Converges — the same bounded attempt, under the same size guard. |
| Add an index | Converges via `CREATE INDEX CONCURRENTLY`. Not size-guarded: long online work on a large table is the pattern's purpose. |
| Add a constraint (`UNIQUE`, `CHECK`); `SET NOT NULL` | Converges via the safer online sequence; not size-guarded either. |
| Relax a `NOT NULL` | Refused, whole plan: dropping `NOT NULL` discards the same guarantee its constraint form would, so it is destructive. Run it deliberately through the imperative front door — it executes natively there — then rerun. |
| Change an index or constraint definition | Refused, whole plan — the drop-and-recreate row above. |
| Narrow a column type (`varchar(255)` → `varchar(50)`) | Refused: the change routes to copy-and-swap, which is not yet available. |
| A bounded-attempt edit on a table above the size threshold | Refused at that statement (`not-native-safe-table-too-large`): with the default 1 GiB threshold, adding a column to a larger table refuses until the threshold is raised. |

A destructive refusal is all-or-nothing: one destructive statement refuses
the whole plan, and the non-destructive statements beside it do not run —
the refusal detail says how many were skipped.

The size threshold is policy, not capability: `Options.MaxTableSizeBytes`
(the CLI's `--max-table-size`) defaults to 1 GiB, and the guard covers only
the blind bounded attempt of a submitted form — planner-proven online
sequences are exempt. On a table you operate deliberately, raising the
threshold is the sanctioned way to converge the bounded-attempt edits; the
refusal means pg-sprite cannot prove the change is instant at that size,
not that the change is unsafe. Why the guard covers exactly that path —
and why the wrong-guess cost scales with table size — is
[optimistic-attempt.md](optimistic-attempt.md).
