# Using pg-sprite with Supabase

Your Supabase app runs on PostgreSQL. pg-sprite helps you change its tables as
you build: add a column for a new feature, or add an index as your queries grow.

Local tests cover column additions and concurrent index builds alongside
Supabase's access policies, Data API, and Realtime subscriptions. Hosted projects
are the next validation step. Changes that need a replacement table are refused
today because copy-and-swap is not implemented yet.

Start with [your first change](#make-your-first-change). The [test results](#what-works-today)
and [roadmap](#where-we-go-next) show how far the current coverage goes.

## The Supabase features involved

You may know these through the Supabase dashboard or client library. Here is
what sits behind them, and why they matter when a table changes:

| Name | What it does here |
| --- | --- |
| **Supavisor** | Supabase's connection pooler: it sits between clients and PostgreSQL and manages database connections |
| **Row-level security (RLS)** | PostgreSQL policies that control which rows a user can read or change—for example, letting each customer see only their own data |
| **Auth and JSON Web Tokens (JWTs)** | Auth handles sign-in and issues signed tokens identifying the user; policies can use `auth.uid()` to read that user's ID |
| **PostgREST** | Serves database tables through a REST API; its schema cache must refresh before newly added columns are available through the API |
| **Realtime** | Lets your app subscribe to live updates; a schema change should preserve the subscriptions your app depends on |

See Supabase's guides to [database connections](https://supabase.com/docs/guides/database/connecting-to-postgres),
[row-level security](https://supabase.com/docs/guides/database/postgres/row-level-security),
and the [Data API](https://supabase.com/docs/guides/api) for more detail.

## Make your first change

This walkthrough adds a nullable `title` column to an existing `public.documents`
table. Substitute your own app table and column. Start on a development project;
the compatibility results below come from local Supabase services.

### Install pg-sprite

With Go installed:

```sh
go install github.com/block/pg-sprite/cmd/pg-sprite@latest
```

This installs the `pg-sprite` binary in Go's bin directory. Add that directory
to your `PATH` if your shell cannot find it. See [installation options](../README.md#install).

### Connect to your project

In your Supabase dashboard, open **Connect** and choose **Direct connection**
or **Session pooler**. pg-sprite needs that PostgreSQL connection URL; the API
key your app uses with the Supabase client is not a database credential.
Connect as a role that owns the application table. Supabase's `postgres` role
worked for the tested changes without superuser access.
See [engine-role.md](engine-role.md) for the operation-specific grants.

Supavisor offers two pooling modes:

- **Session mode** keeps the same PostgreSQL connection for the client's session.
  The tested session endpoint works with pg-sprite and can help on IPv4-only networks
- **Transaction mode** can assign a different PostgreSQL connection after each
  transaction. Do not use it for pg-sprite: execution limits need a stable session

Copy the full connection string from **Connect**, replace its password placeholder
with your database password, and set it in your shell. Keep the host, port, and
username from the selected connection mode; direct and pooled usernames differ.
URL-encode special characters in the password.

```sh
export PGSPRITE_URL='YOUR_POSTGRES_CONNECTION_URL'
```

Replace the example value with your connection string. This sets an environment
variable without opening a connection. Keep credentials out of source control;
your secret manager can also set this variable for you or your agent.

For hosted connections, use `sslmode=verify-full` in the URL to verify the server's
certificate and hostname. If you need to supply a CA certificate separately,
save the certificate for your project and point pg-sprite at it:

```sh
export PGSPRITE_CA_CERT='/absolute/path/to/project-ca.crt'
```

That variable sets the certificate file used by the commands below. A certificate
error should be fixed by checking the hostname and trusted certificate, rather
than disabling verification. Hosted certificate handling remains a validation
milestone for this guide.

### Preview the change

Run a dry run against your database:

```sh
pg-sprite migrate --alter 'ALTER TABLE public.documents ADD COLUMN title text' --dry-run --json
```

It inspects the live table and returns a plan without applying the change.
For this column addition, an executable plan includes these fields (excerpt):

```json
{
  "schema": "public",
  "table": "documents",
  "table_exists": true,
  "disposition": "execute"
}
```

Review the full report's `statements`, including `exec_sql` and `destructive`.
If the table is missing, check the project and table name. If the change is
refused, inspect the reason before proceeding.

### Apply the change

When you are ready, run the same command without `--dry-run`:

```sh
pg-sprite migrate --alter 'ALTER TABLE public.documents ADD COLUMN title text' --json
```

This command changes the database. A successful result includes (excerpt):

```json
{"outcome": "executed-natively"}
```

The execution checks the database again; an earlier dry run is not a guarantee
that the change will still be executable. Existing rows have `NULL` in `title`
until your app writes a value.

### Check it in Supabase

Open the table in **Table Editor**, or run this in **SQL Editor**:

```sql
SELECT column_name, data_type, is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = 'documents'
  AND column_name = 'title';
```

Expected result:

| column_name | data_type | is_nullable |
| --- | --- | --- |
| title | text | YES |

Then read the table through your app with a normal signed-in user. Confirm that
the new column is available and each user still sees only the rows they should.
If your app uses Realtime, check its subscriptions too. An owner query in SQL
Editor confirms the schema, but does not prove your users' access policies work.

### Using a coding agent

Give your agent the table and desired change, with database credentials supplied
through its environment. Have it preview the change, explain the plan, apply it
when authorized, and verify the result through your app's access path.

Agents should read the structured plan and verdict fields; the JSON formats and
exit codes are in [CLI output examples](cli-output-examples.md). A refusal is a
stopping point to inspect, not a reason to retry with `--force`. Keep RLS policies,
grants, and Supabase-managed schemas outside this workflow.

## What works today

A disposable local `supabase/postgres:17.6.1.136` database (PostgreSQL 17.6)
was exercised as its non-superuser `postgres` role. This is the actual
Supabase database image, not an ordinary PostgreSQL image with renamed roles.
An additional local services test uses PostgREST 14.17, Supavisor 2.9.12
and Auth 2.196.0. The Realtime test adds Realtime 2.134.10. It is not a
hosted-project or complete Supabase stack test.

| Experiment | Result | Test |
| --- | --- | --- |
| Add a nullable column to an RLS-protected table | Executed natively; existing policy and RLS remained intact | [RLS preservation](../pkg/migrate/rls_integration_test.go) |
| Add an index to that table | Executed as `CREATE INDEX CONCURRENTLY`; RLS remained intact | [RLS preservation](../pkg/migrate/rls_integration_test.go) |
| Diff desired SQL containing `DEFAULT auth.uid()` | Applied successfully; follow-up diff empty | [Desired schema](../integration/supabase/schema_test.go) |
| Diff a column using `extensions.citext` | Applied with a schema-qualified type; follow-up diff empty | [Desired schema](../integration/supabase/schema_test.go) |
| Defaults, nullability, widening `varchar`, and converting `varchar` to `text` | Executed without replacing the table; policies, grants, publication membership, data, and tenant API access preserved | [Native DDL matrix](../integration/supabase/ddl_matrix_test.go) |
| Check and unique constraints, column rename and removal | Catalog changes verified; tenant API access and existing protections preserved | [Native DDL matrix](../integration/supabase/ddl_matrix_test.go) |
| Foreign key from an app table to `auth.users` | Statement workflow added and validated the constraint; orphan writes rejected | [Auth foreign key](../integration/supabase/failure_test.go) |
| Integer widening, text-to-integer, `varchar` shrinking, numeric scale changes, and volatile defaults | Both statement and desired-schema workflows refused with `backend-unavailable`; full schema/data snapshots unchanged, including a safe addition in the same plan | [Copy-and-swap refusals](../integration/supabase/ddl_matrix_test.go) |
| Stored generated column or explicit `USING` expression | Statement workflow refused with `backend-unavailable`; schema and data unchanged | [Additional refusals](../integration/supabase/failure_test.go) |
| Desired plan containing a column removal | Refused with `destructive-change`; no safe prefix applied | [Whole-plan admission](../integration/supabase/failure_test.go) |
| Lock contention, null rows during `SET NOT NULL`, and duplicate rows during a unique index build | Typed outcomes and durable database state verified; tenant API access preserved | [Failure paths](../integration/supabase/failure_test.go) |
| API and Realtime after each copy-and-swap refusal | Tenant reads and new INSERT events still worked on the original sockets | [Service continuity](../integration/supabase/continuity_test.go) |
| Enable RLS through the schema-change entry point | Refused with `unsupported-statement` | [Refusals](../integration/supabase/schema_test.go) |
| Supavisor session endpoint | Column addition and concurrent index succeeded; session timeouts verified | [Execution](../integration/supabase/services_test.go), [timeouts](../pkg/dbconn/supabase_integration_test.go) |
| Supavisor transaction endpoint | Refused with `ErrNoSessionAffinity`, even with named prepared statements disabled | [Pooler boundary](../pkg/dbconn/supabase_integration_test.go) |
| PostgREST after direct/session schema changes | New columns became available through automatic schema-cache reload | [API and tenants](../integration/supabase/services_test.go) |
| Signed JWTs before and after schema changes | Each tenant saw only its own row; unrelated tenant saw none | [API and tenants](../integration/supabase/services_test.go) |
| Realtime during column addition and concurrent index build | Both sockets stayed connected; all expected INSERT/UPDATE events arrived with tenant isolation and new-column payloads | [Realtime](../integration/supabase/realtime_test.go) |

The required **Supabase compatibility** CI job runs every test linked above,
plus the `schemadiff`, `diffplan`, and `migrate` integration suites against this
image, on code PRs and pushes to `main`. Docs-only PRs keep the usual lighter
checks. See the [workflow](../.github/workflows/ci.yml). `TestNativeChangesPreserveRowSecurity` verifies tenant
visibility before and after native changes, in addition to checking the
stored policy definitions. Here, each tenant is a separate customer whose rows
must stay private. An owner-only SELECT is insufficient evidence of RLS because
owners normally bypass it.

A refusal and an execution failure have different consequences. A refused rewrite
leaves the tested table unchanged. Failed constraint validation can leave a
`NOT VALID` check constraint, and a failed concurrent unique index build can leave
an invalid index. The failure tests verify those leftovers and the reported
outcome; they do not assume every unsuccessful change rolls back completely.

## What to keep in mind

- Manage RLS policies, grants and roles separately. A successful table diff
  does not mean these security settings were compared or reproduced
- Qualify extension types and functions outside `public`, such as
  `extensions.citext`. When pg-sprite inspects a desired schema file, it
  uses a separate workspace with its own search path
- Desired schema files do not yet model foreign keys, including references
  to `auth.users`. Use the supported statement workflow where available;
  see [capabilities.md](capabilities.md)
- Work on tables your app owns. Leave Supabase-managed schemas such as
  `auth` and `storage` to Supabase
- Realtime coverage is limited to INSERT/UPDATE subscriptions during the tested
  native changes. Deletes, reconnect recovery, column removal, and table
  replacement need separate validation
- PostgREST cache refresh was exercised with the image's schema-change event triggers;
  a deployment without those triggers needs its own reload workflow

Hosted role configuration and TLS remain unverified. The Auth
service runs its schema initialization; JWTs are signed by the test fixture,
so this does not test signup or login. These local results are not an
unrestricted Supabase support claim.

## Where we go next

The next milestones build on the local tests. Each needs repeatable evidence
before we expand the support claim:

1. **Validate hosted projects.** Run the same checks on a disposable Supabase
   project, including certificate verification, network access, and hosted roles
2. **Validate declarative schema workflows.** Export an existing Supabase schema,
   edit the desired SQL files, preview the diff, and apply supported changes.
   Verify that the live schema matches the files and a second diff is empty,
   while access policies and Realtime subscriptions still work. Make clear which
   objects the files describe and which remain managed separately
3. **Cover more app workflows.** Exercise deletes and reconnects in Realtime,
   Realtime payloads after column renames and removals, and real signup/login
   flows. Make the limits of desired schema files and access-policy handling
   clear in each case
4. **Validate table rewrites when the engine supports them.** Copy-and-swap must
   preserve data and the surrounding policies, grants, and replication setup.
   A successful copy alone is not enough to claim Supabase compatibility

Have a Supabase workflow you want covered? The local fixture below gives you
a place to reproduce a problem or contribute a new case.

## Repeat the database checks

The compose target binds only to localhost and uses disposable test credentials.
Choose an unused port if 55438 is occupied.

```sh
docker compose -p pgsprite-supabase -f compose/supabase.yml up --wait -d
PG_DSN='postgres://postgres:pgsprite_test_only@127.0.0.1:55438/postgres?sslmode=disable' \
  go test -race -count=1 ./pkg/schemadiff ./pkg/diffplan ./pkg/migrate
docker compose -p pgsprite-supabase -f compose/supabase.yml down -v
```

Success prints `ok` for each package. Failures retain the normal Go test
assertion output. This suite uses disposable schemas and roles on the test
server; never point it at a project containing real application data.

## Repeat the service checks

The opt-in Go tests use the real local services and pg-sprite's public library
entry point. They need Go and Docker; no separately built CLI, Python, or Node.
Use the default fixture ports and run one experiment at a time.

```sh
docker compose -p pgsprite-supabase -f compose/supabase.yml --profile services --profile realtime up --wait -d
SUPABASE_SERVICES_TEST=1 go test -race -count=1 -v ./integration/supabase
SUPABASE_SESSION_URL='postgres://postgres.pgsprite:pgsprite_test_only@127.0.0.1:55440/postgres?sslmode=disable' \
SUPABASE_TRANSACTION_URL='postgres://postgres.pgsprite:pgsprite_test_only@127.0.0.1:55441/postgres?sslmode=disable' \
  go test -race -count=1 ./pkg/dbconn -run TestSupavisorSessionBoundary
docker compose -p pgsprite-supabase -f compose/supabase.yml --profile services --profile realtime down -v
```

Expect `PASS` for every test and subtest, followed by `ok` for each package. Without `SUPABASE_SERVICES_TEST=1`, the
service tests skip. The fixed localhost addresses prevent accidentally pointing
these destructive fixtures at a hosted project.

`TestAPIAndPooler` verifies tenant visibility, automatic PostgREST schema-cache
refresh, and native changes through direct and session connections. It requires
the typed `ErrNoSessionAffinity` error for transaction pooling; an unrelated
connection failure cannot satisfy it. `TestSupavisorSessionBoundary` additionally
checks the session endpoint's actual lock and statement timeout values.

Auth must initialize its schema before the JWT test: the database image's
bootstrap `auth.uid()` reads a legacy setting, while the Auth service updates
it to read PostgREST's JSON claims. The fixture runs the real Auth service
instead of replacing that function with a test implementation.

`TestRealtimeDuringNativeChanges` creates an RLS-protected table with 100,000
seed rows and opens subscriptions for two tenants. It checks baseline delivery,
then adds a column and builds an index through the session endpoint while writes
continue. The writer stays active until both changes finish. The test verifies
every committed INSERT/UPDATE event and the new column in subsequent payloads,
plus the table's PostgreSQL identifier (OID), RLS flag, publication membership,
and index validity. A publication defines which tables PostgreSQL exposes to
replication; Realtime uses it to receive these row changes.

The DDL matrix checks PostgreSQL catalogs and actual tenant API responses.
Rewrite refusal tests snapshot columns, data, constraints, indexes, policies,
grants, and publication membership before both entry points run. The continuity
test then inserts rows after each refusal and requires delivery to the original
Realtime subscriptions, with tenant isolation intact.

Each run recreates its disposable table and restarts only the fixture's Realtime
service **before** opening subscriptions. Realtime [caches publication table
identities](https://github.com/supabase/realtime/blob/v2.134.10/lib/extensions/postgres_cdc_rls/subscription_manager.ex);
recreating a table between runs otherwise leaves stale subscription state until
its periodic refresh. Readiness excludes the temporary HTTP server used during
seeding. There are no service restarts or socket reconnections during the schema
changes. This checks delivery of the expected events, not an exactly-once guarantee.
