# Current limitations

pg-sprite refuses a schema change when it cannot provide its online-safety
guarantees. These are current capability boundaries, not escape hatches:

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
| Non-table objects | Views, materialized views, standalone sequences, enums, domains, extensions, functions, and triggers are outside the model. A serial column's owned sequence is the one exception: it round-trips through the `serial` pseudo-types. A column may *use* an unmanaged type (an enum, a domain) — the type text round-trips — but the type's definition is not managed. |
| Multiple tables per file | A desired file is single-table scoped: exactly one `CREATE TABLE` plus `CREATE INDEX` statements on it. Multi-table schemas are managed as one file per table. |
