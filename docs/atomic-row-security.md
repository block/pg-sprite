# Atomic row security changes

The dedicated Go executor converges the complete RLS definition of one existing
ordinary table. It does not change columns, indexes, or constraints and does not
create missing tables. `diff` remains a preview; no new CLI flags are required.

## Call the Go API

Use `statement.ParseDesiredWithRowSecurity` on the same SQL you preview with
`diff`, then call the dedicated executor with an existing `dbconn` pool:

```go
desired, err := statement.ParseDesiredWithRowSecurity(`
    CREATE TABLE documents (
        id bigint PRIMARY KEY,
        owner_id bigint NOT NULL
    );
    ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
    CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);
`)
if err != nil {
    return err
}
report, err := executor.ExecuteRowSecurity(ctx, pool, "public", desired, executor.Budget{
    LockTimeout:      100 * time.Millisecond,
    StatementTimeout: 5 * time.Second,
})
if err != nil {
    return err
}
// report.Statements contains the committed statements; an empty slice means no change.
```

`StatementTimeout` also caps the entire attempt. There is no approval token or
saved fingerprint. Orchestrators retain their own replan and consent rules.
Changing an RLS definition can widen access even when no data is deleted.

## Execution contract

1. Validate the parsed declaration and nonzero budgets.
2. Begin one transaction with a deadline for the entire attempt and transaction-local
   lock and statement timeouts. Acquire `ACCESS EXCLUSIVE` on the target.
3. Read the live definition and materialize the desired SQL in a rolled-back
   savepoint on the same connection. Refuse unsupported table shapes or any table delta.
4. If RLS already matches, finish without policy DDL. Otherwise replace the complete
   policy set (including unchanged policies), apply ENABLE/DISABLE and FORCE/NO FORCE, and preserve policy comments.
   Live SQL is rendered from the inspected desired catalog, not replayed from the input.
5. Read back the complete definition. Commit only if it matches the desired model.

The lock blocks reads and writes briefly; this is bounded metadata DDL, not an
online copy. Other sessions never see the intermediate policy set. A failure before
commit rolls back every change. A lost commit response is an unknown outcome:
inspect the database before retrying. There are no automatic retries.

Lock exhaustion reports `budget-lock-exceeded`; the statement or whole-attempt
deadline reports `budget-statement-exceeded`. A missing target reports
`table-not-found`. Invalid declarations, unsupported targets, and insufficient
privileges report permanent `row-security-refused` outcomes, preserving the underlying
cause. This includes unresolved policy roles and qualified helper functions during
scratch inspection. Caller cancellation is kept separate from budget exhaustion.

The caller needs table-owner privileges and permission to create the temporary
scratch schema. Both privileges are checked before locking and checked again under
the lock. Roles and qualified helpers must already exist. Grants, role
membership, helper bodies, authentication, and Supabase-managed schemas are outside
this operation. Application authorization tests are still needed. Concurrent
administration of those dependencies is not serialized by the table lock.

## Invariants and tests

- **RS-1:** Read the live baseline only after taking the target lock; never accept a
  caller-supplied diff as execution authority. Verify ownership before and after locking.
  Refuse mixed changes and unsupported table shapes before live DDL.
- **RS-2:** All policy/settings changes and the final catalog comparison share one
  transaction. Fault injection after live DDL must prove the original state survives.
- **RS-3:** Bound lock waits, individual statements, and the entire attempt. Cancellation
  or lock exhaustion must leave the original policies intact.
- **RS-4:** Execute only admitted RLS statements against the qualified target, with a
  pg_catalog-only search path. Scratch objects never persist. Verify convergence before commit.

These checks belong to the executor, regardless of whether a CLI or orchestrator
calls it. SchemaBot can retain its existing replan and consent workflow.
