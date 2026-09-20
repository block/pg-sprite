# Declarative row security

You can export a table's RLS settings and policies, keep them alongside its SQL,
and verify that the live definition still matches. `pull` includes RLS when the
live table has settings or policies; ordinary tables get no extra SQL.
**Applying changes to RLS is not supported yet.** A difference produces a review
of the captured definitions and a refusal, not SQL to execute.

## Export and compare

For a schema containing one supported table, `documents`:

```sh
pg-sprite pull --url "$PG_DSN" --schema public --out schema
```

```text
PULLED  documents -> schema/documents.sql
Summary: 1 pulled, 0 refused, 0 errors
```

Then compare the exported definition with the same database:

```sh
pg-sprite diff --url "$PG_DSN" --schema public --desired schema/documents.sql
```

```text
-- no changes: live table matches the desired schema
```

`pull` never overwrites an existing file. Both commands use the usual database
connection flags. `diff` also needs permission to create a scratch schema and
materializes the declaration in a transaction that it rolls back; it does
not change the live table. Roles and qualified helpers must already exist.

An explicit `ENABLE` or `DISABLE ROW LEVEL SECURITY` statement declares the
**complete table-local RLS definition**, even when there are no policies. A file
that includes policies must include that setting too. Removing the last
policy therefore remains a difference while the setting stays in the file.
Policies without that setting are invalid. Removing every RLS declaration returns
to table-only scope; it does not request deletion of live policies. `FORCE` is
optional and defaults to `NO FORCE`.

Files without RLS declarations keep their table-only behavior: `diff` leaves
access control separately managed. Export preserves policies even when RLS is
disabled, and preserves enabled RLS even when there are no policies (default deny).
`fmt`, `lint`, and live desired-state execution do not accept the expanded format yet.

## Keep the SQL people already use

Row-level security (RLS) lets PostgreSQL decide which rows a role may read or
write. A policy is one of those rules. Supabase uses PostgreSQL's RLS, with roles
such as `authenticated` and helpers such as `auth.uid()` to identify the caller.
We do not need a second language for these definitions.

The input is ordinary SQL alongside the table definition:

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

This file is accepted by `diff` for inspection. The role, helper,
and necessary table grants must already exist. These policies cover reads and
inserts, not updates or deletes. Application authorization tests remain necessary.

The format follows [Supabase's RLS guide](https://supabase.com/docs/guides/database/postgres/row-level-security)
and [policy examples](https://github.com/supabase/supabase/blob/master/examples/prompts/database-rls-policies.md).
Their preference for separate policies per operation is authoring guidance, not
a reason to discard PostgreSQL's `FOR ALL` or restrictive policies during inspection.

## Current boundaries

Round trips preserve enabled and forced settings, permissive and restrictive
policies, commands, roles, omitted clauses, expressions, and policy comments.
Qualify every external helper and type in policy expressions, including objects
in `public`: use `public.doc_status`, not `doc_status`. Policy inspection searches
only its scratch schema and PostgreSQL built-ins, preventing accidental bindings.
Qualified helpers such as `auth.uid()` work, including `(SELECT auth.uid())`.
Policy expressions that directly query a table are refused until dependency
handling can preserve their identities. Other [table export limits](pull.md#refused-table-shapes)
still apply.

An unchanged definition produces an empty plan. Any table or RLS difference in
this mode is refused, including a missing live table. No partial plan is emitted. Admission errors exit 1 with a diagnostic; unsupported
RLS comparisons exit 2 with a refusal verdict. `--json` returns a verdict with
`outcome`, `reason`, and `detail` for those refusals; successful comparisons retain
the normal plan format. `--sql` writes the refusal as a SQL comment.
Equal definitions do not prove equal access: grants, role membership, helper
function bodies, and authentication configuration are outside this comparison.

Library callers use `RenderWithRowSecurity`, `ParseDesiredWithRowSecurity`, and
`diffplan.PlanWithRowSecurity`. The parser returns a separate inspection-only type
that the existing live executors cannot accept.

## Review a difference

Run the same `diff` command after editing the file. For example, disabling RLS
produces this review before the refusal verdict:

```text
public.documents — row security review
  enabled: true → false [may-widen]
Access impact is advisory; grants, role membership, and helper bodies are not compared.
```

Policy additions, removals, and edits show complete before/after definitions,
including roles, commands, predicates, and comments. Arbitrary predicate changes
are `review-required`; pg-sprite does not attempt to prove SQL equivalence.
`may-widen` flags disabling RLS, removing FORCE, adding a permissive policy,
removing a restrictive policy, or changing restrictive to permissive. These are
warnings about individual changes, not conclusions about combined effective access.
Comment-only edits are `metadata-only`. Every difference still exits 2.

With `--json`, the existing refusal verdict gains `schema`, `table`, and a
`row_security_review` object. An abbreviated example:

```json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "schema": "public",
  "table": "documents",
  "row_security_review": {
    "version": 1,
    "changes": [{
      "kind": "enabled",
      "before_setting": true,
      "after_setting": false,
      "access_impact": "may-widen"
    }],
    "table_changed": false,
    "table_comparison_complete": true
  }
}
```

Version 1 kinds are `enabled`, `forced`, `policy-added`, `policy-removed`, and
`policy-changed`. Policy changes carry `policy` and the applicable `before_policy`
and `after_policy` snapshots. Each snapshot contains `name`, `command` (PostgreSQL
catalog codes `*`, `r`, `a`, `w`, `d`), `permissive`, `roles`, `using`, `with_check`,
and `comment`. Null clauses remain null; they are not rewritten as predicates.
Consumers must reject unknown versions, kinds, or impact values.

Mixed table/policy changes set `table_changed`; the entire change remains blocked.
If the table comparison is unsupported, `table_comparison_complete` is false and
`table_comparison_error` explains why; `table_changed: false` then means unknown,
not unchanged. This review contains no execution SQL or approval fingerprint.
`--sql` renders it entirely as comments. Unchanged files retain the normal empty
plan response. Missing live tables remain refusals without review details.

Library callers can inspect `diffplan.RowSecurityReviewRequired` with `errors.As`;
it still unwraps to `schemadiff.ErrUnsupportedChange`. `ReviewRowSecurity` provides
the same review from two catalog models. See the [review tests](../pkg/schemadiff/row_security_review_integration_test.go).

## Reuse the format, define the execution contract

[Supabase's declarative workflow](https://supabase.com/docs/guides/local-development/declarative-database-schemas)
keeps desired SQL in `supabase/schemas` and generates versioned migrations from
schema files and migration history. Its current guide names `pg-delta` as the
default diff engine. The accompanying
[declarative prompt](https://github.com/supabase/supabase/blob/master/examples/prompts/declarative-database-schema.md)
still describes `migra`; do not treat those older caveats as the current engine's
complete support matrix.

pg-sprite already compares a live table with desired SQL materialized inside a
rolled-back scratch transaction. This work extends that model instead of importing another
schema engine. PostgreSQL should resolve SQL and supply its catalog representation.
Before enabling policy execution, settle these boundaries:

- **Explicit ownership.** Existing table-only files keep access control separately
  managed. A caller must opt into managing a table's complete RLS definition.
  That scope must be independent of the policies present in the file: removing
  the last policy must remain a reviewable change, not switch management off.
  The SQL declaration and `statement.DesiredWithRowSecurity` provide that explicit scope.
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
   diff. Implemented through automatic export and SQL declarations; execution remains refused,
   including greenfield creation.
3. **Plan and execute transitions.** Add typed security changes, exact-state
   revalidation, lock budgets, atomic application, dependency handling, and reports
   suitable for users and orchestrators.
4. **Prove application behavior.** Extend the local Supabase harness with real
   authenticated and anonymous requests, two users, allowed and denied writes,
   and interrupted transitions. Then validate hosted connection and privilege
   boundaries on a disposable project before claiming hosted support.

The [inspection tests](../pkg/schemadiff/row_security_integration_test.go),
[round-trip tests](../pkg/schemadiff/row_security_roundtrip_integration_test.go), and
[Supabase auth test](../integration/supabase/row_security_test.go) use real databases
and readable DDL. They prove catalog fidelity, round trips, and refusal boundaries,
**not support for applying policies**. The existing PostgreSQL CI matrix and
Supabase compatibility job both run `pkg/schemadiff`; no separate runner is needed.
Hosted validation is not a prerequisite for the local steps, nor replaced by them.

PostgreSQL's [`pg_policy` catalog](https://www.postgresql.org/docs/current/catalog-pg-policy.html)
and [`CREATE POLICY` reference](https://www.postgresql.org/docs/current/sql-createpolicy.html)
define the policy fields and behavior.
