# Using pg-sprite with Supabase

Your Supabase app runs on PostgreSQL. pg-sprite helps you change its tables as
you build: add a column for a new feature, or add an index as your queries grow.

Local tests cover column additions and concurrent index builds alongside
Supabase's access policies, Data API, and Realtime subscriptions. Hosted projects
are the next validation step. Changes that need a replacement table are refused
today because copy-and-swap is not implemented yet.

Start with [a database connection](#choose-a-connection). The [test results](#what-works-today)
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

## Choose a connection

In your Supabase dashboard, open **Connect** and choose **Direct connection**
or **Session pooler**. pg-sprite needs that PostgreSQL connection URL; the API
key your app uses with the Supabase client is not a database credential.
Connect as a role that owns the application table. Supabase's `postgres` role
worked for the tested changes without superuser access.
See [engine-role.md](engine-role.md) for the operation-specific grants.

Use TLS certificate verification for a hosted database. Set `PGSPRITE_URL`
through your credential tooling, and use `--ca-cert` with the server's CA
certificate when needed. Do not copy passwords into committed scripts.
Direct hosted connections may require IPv6. Supavisor offers two pooling modes:

- **Session mode** keeps the same PostgreSQL connection for the client's session.
  The tested session endpoint works with pg-sprite
- **Transaction mode** can assign a different PostgreSQL connection after each
  transaction. Do not use it for pg-sprite: execution limits need a stable session

Disabling prepared statements does not make transaction pooling safe.

## Try a column addition

After you [install pg-sprite](../README.md) and set `PGSPRITE_URL`, you can submit
a change for one of your app's tables. For example, if `public.documents` exists
and does not yet have a `title` column:

```sh
pg-sprite migrate --alter 'ALTER TABLE public.documents ADD COLUMN title text' --json
```

This command applies the change. A successful result includes:

```json
{"outcome": "executed-natively"}
```

That is an excerpt; the full result includes execution details. For agents,
use the structured `outcome` and refusal `reason` fields to decide what to do
next. A refusal is a stopping point to inspect, not a reason to retry with
`--force`. Keep RLS policies, grants, and Supabase-managed schemas outside the
change unless you are deliberately managing them through another workflow.

## What works today

A disposable local `supabase/postgres:17.6.1.136` database (PostgreSQL 17.6)
was exercised as its non-superuser `postgres` role. This is the actual
Supabase database image, not an ordinary PostgreSQL image with renamed roles.
An additional local services test uses PostgREST 14.17, Supavisor 2.9.12
and Auth 2.196.0. The Realtime test adds Realtime 2.134.10. It is not a
hosted-project or complete Supabase stack test.

| Experiment | Result |
| --- | --- |
| Add a nullable column to an RLS-protected table | Executed natively; existing policy and RLS remained intact |
| Add an index to that table | Executed as `CREATE INDEX CONCURRENTLY`; RLS remained intact |
| Diff desired SQL containing `DEFAULT auth.uid()` | Planned successfully |
| Diff a column using `extensions.citext` | Planned successfully with a schema-qualified type |
| Rewrite a text column to integer | Refused with `backend-unavailable` |
| Enable RLS through the schema-change entry point | Refused with `unsupported-statement` |
| Supavisor session endpoint | Column addition and concurrent index succeeded; session timeouts verified |
| Supavisor transaction endpoint | Connection refused; named prepared statements conflict, and disabling them reaches `ErrNoSessionAffinity` |
| PostgREST after direct/session schema changes | New columns became available through automatic schema-cache reload |
| Signed JWTs before and after schema changes | Each tenant saw only its own row; unrelated tenant saw none |
| Realtime during column addition and concurrent index build | Both sockets stayed connected; all expected INSERT/UPDATE events arrived with tenant isolation and new-column payloads |

The `schemadiff`, `diffplan`, and `migrate` integration suites also passed
against this image. `TestNativeChangesPreserveRowSecurity` verifies tenant
visibility before and after native changes, in addition to checking the
stored policy definitions. Here, each tenant is a separate customer whose rows
must stay private. An owner-only SELECT is insufficient evidence of RLS because
owners normally bypass it.

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
2. **Cover more app workflows.** Exercise deletes and reconnects in Realtime,
   column changes beyond additions, and tables linked to `auth.users`. Make the
   limits of desired schema files and access-policy handling clear in each case
3. **Validate table rewrites when the engine supports them.** Copy-and-swap must
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

Expect `PASS` for `TestAPIAndPooler` and `TestRealtimeDuringNativeChanges`,
followed by `ok` for each package. Without `SUPABASE_SERVICES_TEST=1`, the
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

Each run recreates its disposable table and restarts only the fixture's Realtime
service **before** opening subscriptions. Realtime [caches publication table
identities](https://github.com/supabase/realtime/blob/v2.134.10/lib/extensions/postgres_cdc_rls/subscription_manager.ex);
recreating a table between runs otherwise leaves stale subscription state until
its periodic refresh. Readiness excludes the temporary HTTP server used during
seeding. There are no service restarts or socket reconnections during the schema
changes. This checks delivery of the expected events, not an exactly-once guarantee.
