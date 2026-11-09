# Declarative row security

You can export a table's RLS settings and policies, keep them alongside its SQL,
and verify that the live definition still matches. `pull` includes RLS when the
live table has settings or policies; ordinary tables get no extra SQL.
`diff` shows the captured definitions and refuses to emit execution SQL for RLS
changes. Use `migrate --desired` to apply an RLS-only declaration to an existing supported table
through the [atomic executor](atomic-row-security.md). No RLS-specific flags are needed.

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
`fmt` and `lint` do not accept the expanded format yet.

## Apply the declaration

After reviewing the differences, apply the same file:

```sh
pg-sprite migrate --url "$PG_DSN" --schema public --desired schema/documents.sql
```

The command prints an executed verdict and the SQL that committed. A second apply
prints an already-converged verdict and runs no policy DDL. For scripts, add `--json`:

```sh
pg-sprite migrate --url "$PG_DSN" --schema public --desired schema/documents.sql --json
```

An already-converged response is:

```json
{
  "outcome": "executed-natively",
  "statement": "",
  "table": "public.documents",
  "detail": "already converged: row security matches; nothing to run"
}
```

On a change, `executed_sql` contains the ordered statements committed together.
The input owns the complete policy set: removing a policy from the file removes it
from the table. Disabling RLS or widening a policy changes access deliberately;
review that meaning and test application authorization before applying.

`--dry-run` uses the same review as `diff`: RLS differences exit 2 and show the
review without executing. That exit code describes the review-only preview,
not a claim that the atomic executor cannot apply an RLS-only difference.
Apply independently reads and validates the current table under its lock; it does
not execute a saved preview or pin the reviewed definition with a fingerprint.

Apply exits 0 after commit or a no-op and 2 for executor/target refusals.
Invalid or unsupported input declarations fail admission before execution and
exit 1 with a diagnostic, without a verdict. Operational failures also exit 1;
their JSON verdict includes the executor's `code`. Refusal verdicts use `reason`
and `class`, without a failure code.
`row-security-outcome-unknown` means the commit response was lost or failed:
inspect the live state before retrying; do not assume rollback. The whole RLS
attempt uses `--statement-timeout`, including scratch inspection and lock waits;
`--lock-timeout` bounds lock acquisition. RLS applies do not automatically retry.

### Create a new table with its row security

When the table does not exist, the same file creates it. `diff` and
`migrate --dry-run` preview the create from a transaction that rolls back:

```text
public.documents does not exist. migrate --desired creates it with its row security in one transaction:
  CREATE TABLE public.documents (id bigint PRIMARY KEY, owner text NOT NULL, body text);
  ALTER TABLE "public"."documents" ENABLE ROW LEVEL SECURITY;
  ALTER TABLE "public"."documents" NO FORCE ROW LEVEL SECURITY;
  CREATE POLICY "owners_read" ON "public"."documents"
    AS PERMISSIVE FOR SELECT TO PUBLIC
    USING ((owner = CURRENT_USER));
```

The preview exits 0. With `--json` it is the statement list with
`table_exists: false`:

```json
{
  "schema": "public",
  "table": "documents",
  "statements": [
    "CREATE TABLE public.documents (id bigint PRIMARY KEY, owner text NOT NULL, body text)",
    "ALTER TABLE \"public\".\"documents\" ENABLE ROW LEVEL SECURITY",
    "ALTER TABLE \"public\".\"documents\" NO FORCE ROW LEVEL SECURITY",
    "CREATE POLICY \"owners_read\" ON \"public\".\"documents\"\n    AS PERMISSIVE FOR SELECT TO PUBLIC\n    USING ((owner = CURRENT_USER))"
  ],
  "table_exists": false
}
```

`migrate --desired` then commits the table, its indexes, and its row security
in one transaction, so no session sees the table without its policies. The
verdict carries `"table_created": true`, its detail reads `table created with
its row security: all changes committed in one transaction`, and
`executed_sql` lists the statements above. A second run is already converged,
and its verdict leaves `table_created` out. The table shapes a new table may take are the
same as for an ordinary desired file, and a foreign key is still refused. A
standalone type at the table's name refuses as `create-collision`. The
[atomic RLS contract](atomic-row-security.md#create-a-missing-table-with-its-row-security)
lists the steps.

### Change the table when the row security matches

`pull` writes the row security into the file of every table that has it, so
adding a column or an index means editing a file that also declares the
table's policies. When the live row security matches the file and only the
table definition differs, the file is planned and run like a file without row
security. `diff` and `migrate --dry-run` show the ordinary plan, and `--json`
returns the ordinary [plan report](plan-report.md), fingerprint included.

`migrate --desired` runs that plan through the desired-state sequence. Its
JSON is `migrate.DesiredResult` with `plan`, `verdicts`, `outcome`, and
`convergence`, and earlier committed steps stay committed if a later step
fails. The run never writes a policy or a row security setting. After the
last commit it plans again, row security included, and reports `executed`
only when that plan is empty. If another session changed the policies while
the table plan ran, the run fails, its error says that the row security no
longer matches the file, and `convergence.status` is `unverifiable`. The table
statements stay committed. Review the policy change before running again: a
second run of the same file restores the policies the file declares.

A file whose row security differs from the live table is refused when its
table definition differs too. Converge the two separately: change the row
security first with the table definition unchanged, then change the table.
Go callers run the table change with `migrate.RunDesiredWithRowSecurity`,
which refuses a file whose row security differs before anything runs.

Ordinary files use the existing desired-state sequence instead; their JSON is
`migrate.DesiredResult` with `plan`, `verdicts`, and overall `outcome`. Earlier
committed steps stay committed if a later step fails. Mixed table/RLS changes
are refused as a whole. `--desired` cannot be combined with `--alter`, `--force`,
or `--accept-blocking`.

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

This file is accepted by `diff` for inspection and by `migrate --desired` for execution. The role, helper,
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
`--extension-schema` does not change that: it applies to the table definition
only, so a column can be `email citext` while a policy that compares with it
writes `extensions.citext`.
Qualified helpers such as `auth.uid()` work, including `(SELECT auth.uid())`.
A policy may read another table, such as a membership check written as
`EXISTS (SELECT 1 FROM public.team_members m WHERE m.user_id = auth.uid())`, when
it names that table schema-qualified. Scratch builds the policy against the live
table it names, which is the table the applied policy will read; `CREATE POLICY`
takes only an `ACCESS SHARE` lock on it, so writes to that table continue. An
unqualified table name, a CTE name, or a database-qualified name is refused, and
so is any table with the policy's own table name: scratch would bind that name to
the live table rather than to itself, and PostgreSQL rejects a policy that reads
its own table as infinite recursion when it is queried. A `pg_catalog` table is
refused because introspection prints it unqualified, so the exported file could
not be admitted again. A query that locks the rows it reads (`FOR SHARE`,
`FOR UPDATE`, and their weaker forms) is refused because building the policy
would take `ROW SHARE` on that table and wait behind a session holding
`EXCLUSIVE`. The comparison does not cover the referenced table's own policies,
which apply to the subquery under the caller's role.

A change to an existing table's policies is refused when a table they read has
policies that read it back, directly or through other tables and views.
PostgreSQL accepts such a policy, then fails every query it applies to with
"infinite recursion detected in policy". The check follows the dependencies
PostgreSQL records for policies and views, so it does not follow a function
body that reads a table, and it counts a view whichever role owns it. It reads
the policies committed when the change takes its lock on the table; a later
change to another table's policies can still close the loop.

A `SECURITY DEFINER` helper function avoids the referenced table's policies,
and the recursion, only when its owner bypasses row security on the table it reads:
a role with `BYPASSRLS`, or the table's owner when the table does not force row
security. Such a helper runs with its owner's access, so it must check the
caller's authorization itself. Other
[table export limits](pull.md#refused-table-shapes) still apply.

An unchanged definition produces an empty plan. A table difference with
matching row security produces the
[ordinary table plan](#change-the-table-when-the-row-security-matches). Any
other difference in this mode is refused. A missing live table is previewed as a
[create with its row security](#create-a-new-table-with-its-row-security). No partial plan is emitted. Admission errors exit 1 with a diagnostic; unsupported
RLS comparisons exit 2 with a refusal verdict. `--json` returns a verdict with
`outcome`, `reason`, and `detail` for those refusals; successful comparisons retain
the normal plan format. `--sql` writes the refusal as a SQL comment.
Equal definitions do not prove equal access: grants, role membership, helper
function bodies, and authentication configuration are outside this comparison.

Library callers that handle both kinds of desired file use `ParseDesiredFile`. It
picks the front door from the file's statements, returns whichever declaration it
admitted, and refuses with that front door's typed errors. A caller that only
handles row-security files uses `ParseDesiredWithRowSecurity` directly; both feed
`diffplan.PlanWithRowSecurity`, and `RenderWithRowSecurity` writes a file back out.
The row-security declaration is a separate type. Only the dedicated atomic RLS
executor and `migrate.RunDesiredWithRowSecurity`, which changes only the table,
accept it for live execution; generic native and create executors do not.
`HasRowSecurityDeclaration` stays for callers that only need to know whether SQL
declares row security, without admitting it.

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
and `after_policy` snapshots. Both fields are always present: an absent side is
JSON `null` (both are `null` for setting changes). Each snapshot contains `name`, `command` (PostgreSQL
catalog codes `*`, `r`, `a`, `w`, `d`), `permissive`, `roles`, `using`, `with_check`,
and `comment`. Null clauses remain null; they are not rewritten as predicates.
Consumers must reject unknown versions, kinds, or impact values.

When the row security and the table both differ, the review sets
`table_changed`, and the entire change remains blocked.
If the table comparison is unsupported, `table_comparison_complete` is false and
`table_comparison_error` explains why; `table_changed: false` then means unknown,
not unchanged. This review contains no execution SQL or approval fingerprint.
`--sql` renders it entirely as comments. Unchanged files retain the normal empty
plan response. A missing live table previews its create instead of a review.

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
The execution contract preserves these boundaries:

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
- **Atomic transitions.** Derive the change from live state under the appropriate lock
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
   diff. Implemented through automatic export and SQL declarations. A table that
   does not exist yet is created together with its row security in one transaction.
3. **Execute transitions atomically.** The Go executor now locks, derives, applies,
   and verifies one table's RLS state in one bounded transaction. A table change
   runs on its own when the row security already matches; a change to both
   remains unsupported. A policy that reads another schema-qualified table is supported. The CLI selects this executor for explicit RLS declarations through `migrate --desired`.
4. **Prove application behavior.** The local Supabase harness now checks real
   PostgREST requests from two authenticated users and anonymous callers, allowed
   and denied writes, changed visibility, and rollback after a cancelled apply.
   The [hosted suite](../integration/supabase/hosted/README.md) also exercised
   preview, apply, convergence, tenant access, and atomic rollback as the owning
   `postgres` role on a disposable project. Hosted non-owner privilege validation
   remains a follow-up; owner-role results do not establish that boundary.

The [inspection tests](../pkg/schemadiff/row_security_integration_test.go),
[round-trip tests](../pkg/schemadiff/row_security_roundtrip_integration_test.go), and
[Supabase auth test](../integration/supabase/row_security_test.go) use real databases
and readable DDL. They prove catalog fidelity, round trips, and refusal boundaries,
**not support for applying policies**. The separate
[executor tests](../pkg/executor/row_security_integration_test.go) cover atomic
application, rollback, lock waits, and non-owner access on PostgreSQL. The existing PostgreSQL CI matrix and
Supabase compatibility job both run `pkg/schemadiff`; no separate runner is needed.
Hosted validation is not a prerequisite for the local steps, nor replaced by them.

PostgreSQL's [`pg_policy` catalog](https://www.postgresql.org/docs/current/catalog-pg-policy.html)
and [`CREATE POLICY` reference](https://www.postgresql.org/docs/current/sql-createpolicy.html)
define the policy fields and behavior.

The [RLS API tests](../integration/supabase/row_security_api_test.go) and
[cancellation test](../integration/supabase/row_security_api_rollback_test.go)
exercise the atomic executor on the pinned Supabase stack through application
requests. They run automatically in the existing Supabase compatibility CI job.
Tokens are fixture-signed; signup/login flows and Realtime authorization changes
are outside these tests.
