# Current limitations

pg-sprite refuses a schema change when it cannot provide its online-safety
guarantees. These are current capability boundaries, not escape hatches:

| Change | Current behavior |
| --- | --- |
| Index builds on a partitioned parent | PostgreSQL cannot build an index concurrently at the parent level. PostgreSQL supports a plain blocking build, but pg-sprite refuses it by policy because it takes `ACCESS EXCLUSIVE`; `--force` does not bypass this decision. An operator who chooses a maintenance-window blocking build must run it outside pg-sprite. The partition-aware `CREATE INDEX ON ONLY` → per-partition `CREATE INDEX CONCURRENTLY` → `ATTACH PARTITION` flow is planned and tracked in [#34](https://github.com/block/pg-sprite/issues/34). |
| `ADD CONSTRAINT ... USING INDEX` on a partitioned parent | PostgreSQL does not support adopting an existing index on a partitioned parent in any supported version. pg-sprite refuses before execution. |
| `ADD FOREIGN KEY ... NOT VALID` on a partitioned parent | PostgreSQL does not support this before version 18, so pg-sprite refuses it on versions 14–17. It is supported on version 18 and later. |
| Copy-and-swap | The copy-and-swap backend is not yet available. Statements that require it route to `refuse`; pg-sprite never falls through to a blocking rewrite. |
