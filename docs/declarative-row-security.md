# Declarative row security

**Design in progress.** Table introspection records RLS settings and policies.
Export refuses tables carrying them so a baseline cannot silently lose access
control. Desired files still reject policy statements; policy diffing, SQL export,
and execution are not implemented. A library caller supplying an RLS-bearing
desired model also receives a typed refusal rather than an incomplete plan.

## Keep the SQL people already use

Row-level security (RLS) lets PostgreSQL decide which rows a role may read or
write. A policy is one of those rules. Supabase uses PostgreSQL's RLS, with roles
such as `authenticated` and helpers such as `auth.uid()` to identify the caller.
We do not need a second language for these definitions.

The proposed input is ordinary SQL alongside the table definition:

```sql
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL
);

ALTER TABLE documents ENABLE ROW LEVEL SECURITY;

CREATE POLICY "Read your documents"
    ON documents
    FOR SELECT TO authenticated
    USING ((SELECT auth.uid()) = owner_id);

CREATE POLICY "Create your documents"
    ON documents
    FOR INSERT TO authenticated
    WITH CHECK ((SELECT auth.uid()) = owner_id);
```

This is the intended format, **not a file pg-sprite accepts today**. The role,
helper, and necessary table grants must already exist. These policies cover reads
and inserts, not updates or deletes. Application authorization tests remain necessary.

The format follows [Supabase's RLS guide](https://supabase.com/docs/guides/database/postgres/row-level-security)
and [policy examples](https://github.com/supabase/supabase/blob/master/examples/prompts/database-rls-policies.md).
Their preference for separate policies per operation is authoring guidance, not
a reason to discard PostgreSQL's `FOR ALL` or restrictive policies during inspection.

## Reuse the format, define the execution contract

[Supabase's declarative workflow](https://supabase.com/docs/guides/local-development/declarative-database-schemas)
keeps desired SQL in `supabase/schemas` and generates versioned migrations from
schema files and migration history. Its current guide names `pg-delta` as the
default diff engine. The accompanying
[declarative prompt](https://github.com/supabase/supabase/blob/master/examples/prompts/declarative-database-schema.md)
still describes `migra`; do not treat those older caveats as the current engine's
complete support matrix.

pg-sprite already compares a live table with desired SQL materialized inside a
rolled-back scratch transaction. Extend that model instead of importing another
schema engine. PostgreSQL should resolve SQL and supply its catalog representation.
Before enabling policy execution, settle these boundaries:

- **Explicit ownership.** Existing table-only files keep access control separately
  managed. A caller must opt into managing a table's complete RLS definition.
  That scope must be independent of the policies present in the file: removing
  the last policy must remain a reviewable change, not switch management off.
  The API for declaring that scope is still open.
- **Complete state.** Compare `ENABLE` and `FORCE` independently, plus policy name,
  command, permissive/restrictive mode, role set, `USING`, and `WITH CHECK`.
  Preserve omitted clauses and comments. Enabled RLS without policies means
  default deny; disabled RLS can still retain policies and `FORCE`.
- **Dependency identity.** Keep references such as `auth.uid()` qualified. Resolve
  dependencies against the intended environment, refusing ambiguous bindings.
  Equal expression text does not prove equal behavior if a referenced function,
  role membership, or grant changed. Do not silently bind a different scratch object.
- **Security changes need their own classification.** Adding a permissive policy,
  removing a restrictive one, disabling RLS, or changing a role can widen access
  without deleting data. A data-destruction label is not a complete authorization
  contract. Do not claim to prove arbitrary predicates equivalent.
- **Atomic transitions.** Recheck the reviewed state under the appropriate lock
  and apply a table's policy transition in one bounded transaction. Replacement
  must not leave a committed intermediate access rule. Refuse mixed table/policy
  plans until their execution strategy preserves that guarantee.

The executor must enforce these properties itself, following [SAFETY.md](../SAFETY.md).
Roles, grants, authentication setup, and Supabase-managed schemas remain outside
this table-scoped work.

## Build it in reviewable steps

1. **Observe without losing information.** Capture catalog state and refuse an
   incomplete export. Implemented here; table-only diff behavior stays intact.
2. **Round-trip the declaration.** Admit and export SQL under explicit RLS scope;
   materialize it in scratch and prove the unchanged definition produces an empty
   diff. Keep policy execution refused, including greenfield creation, until the
   executor supports the full declaration.
3. **Plan and execute transitions.** Add typed security changes, exact-state
   revalidation, lock budgets, atomic application, dependency handling, and reports
   suitable for users and orchestrators.
4. **Prove application behavior.** Extend the local Supabase harness with real
   authenticated and anonymous requests, two users, allowed and denied writes,
   and interrupted transitions. Then validate hosted connection and privilege
   boundaries on a disposable project before claiming hosted support.

The [inspection tests](../pkg/schemadiff/row_security_integration_test.go) use real
PostgreSQL and readable DDL. They prove catalog fidelity and export refusal,
**not support for applying policies**. The existing PostgreSQL CI matrix and
Supabase compatibility job both run `pkg/schemadiff`; no separate runner is needed.
Hosted validation is not a prerequisite for the local steps, nor replaced by them.

PostgreSQL's [`pg_policy` catalog](https://www.postgresql.org/docs/current/catalog-pg-policy.html)
and [`CREATE POLICY` reference](https://www.postgresql.org/docs/current/sql-createpolicy.html)
define the policy fields and behavior.
