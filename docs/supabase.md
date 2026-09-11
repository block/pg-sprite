# Using pg-sprite with Supabase

pg-sprite can execute supported native schema changes against Supabase
PostgreSQL. It does not replace Supabase's management of Auth, Storage,
Realtime, or application access policies. Copy-and-swap is not implemented;
changes that require it are refused.

## Connect

Use a database connection URL, not a Supabase API key. Start with the
[direct connection](https://supabase.com/docs/guides/database/connecting-to-postgres)
and a role that owns the application table. Supabase's `postgres` role is
not a PostgreSQL superuser; native operations do not require superuser access.
See [engine-role.md](engine-role.md) for the operation-specific grants.

Use TLS certificate verification for a hosted database. Set `PGSPRITE_URL`
through your credential tooling, and use `--ca-cert` with the server's CA
certificate when needed. Do not copy passwords into committed scripts.
Direct hosted connections may require IPv6. The tested Supavisor session endpoint also works. Do not use transaction
pooling: pg-sprite needs a stable backend session for its execution limits.
Disabling prepared statements does not make transaction pooling safe.

## What was tested

A disposable local `supabase/postgres:17.6.1.136` database (PostgreSQL 17.6)
was exercised as its non-superuser `postgres` role. This is the actual
Supabase database image, not an ordinary PostgreSQL image with renamed roles.
An additional local services test uses PostgREST 14.17, Supavisor 2.9.12
and Auth 2.196.0. The Realtime test adds Realtime 2.134.10. It is not a hosted-project or complete Supabase stack test.

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
| PostgREST after direct/session DDL | New columns became available through automatic schema-cache reload |
| Signed JWTs before and after DDL | Each tenant saw only its own row; unrelated tenant saw none |
| Realtime during column addition and concurrent index build | Both sockets stayed connected; all expected INSERT/UPDATE events arrived with tenant isolation and new-column payloads |

The `schemadiff`, `diffplan`, and `migrate` integration suites also passed
against this image. `TestNativeChangesPreserveRowSecurity` verifies tenant
visibility before and after native changes, in addition to checking the
policy catalog. An owner-only SELECT is insufficient evidence of RLS because
owners normally bypass it.

## Keep the boundaries explicit

- Manage RLS policies, grants and roles separately. A successful table diff
  does not mean these security settings were compared or reproduced
- Qualify extension types and functions outside `public`, such as
  `extensions.citext`. Desired-state scratch introspection uses its own
  search path; do not depend on the session's `extensions` search path
- Foreign keys, including references to `auth.users`, are outside the
  desired-file model. Use the supported statement workflow where available;
  see [capabilities.md](capabilities.md)
- Restrict experiments to application-owned tables. Do not reconcile
  Supabase-managed schemas as if they were application declarations
- Realtime coverage is limited to INSERT/UPDATE subscriptions during the tested
  native changes. Deletes, reconnect recovery, column removal, and table
  replacement need separate validation
- PostgREST cache refresh was exercised with the image's DDL event triggers;
  a deployment without those triggers needs its own reload workflow

Hosted role configuration and TLS remain unverified. The Auth
service runs its schema initialization; JWTs are signed by the test fixture,
so this does not test signup or login. These local results are not an
unrestricted Supabase support claim.

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
plus the table OID, RLS flag, publication membership, and index validity.

Each run recreates its disposable table and restarts only the fixture's Realtime
service **before** opening subscriptions. Realtime [caches publication table
identities](https://github.com/supabase/realtime/blob/v2.134.10/lib/extensions/postgres_cdc_rls/subscription_manager.ex);
recreating a table between runs otherwise leaves stale subscription state until
its periodic refresh. Readiness excludes the temporary HTTP server used during
seeding. There are no service restarts or socket reconnections during the schema
changes. This checks delivery of the expected events, not an exactly-once guarantee.
