# Hosted Supabase checks

Use this suite on a **disposable hosted project**. It creates real Auth users,
application tables, policies, and publication entries, then removes its fixtures.
It leaves project settings alone. No Docker services or locally signed JWTs are used.

This is an opt-in complement to the [local suite](../README.md), not a replacement
for its larger DDL matrix or a required hosted CI job. Realtime checks establish
working delivery before applying schema changes, then verify continuity on the
same table and connections. Setup failures fail the test; writes are never retried.

## Run it

Create a disposable Supabase project and copy its **Direct connection** details
from **Connect**. Use a private PostgreSQL password file or your secret manager;
never check credentials into the repository. Keep the creation defaults if you
want to test the normal new-project experience.

In **Settings → API Keys**, copy the publishable and secret keys into a local JSON
file with mode `0600`. The secret key is used only to create and delete test users;
application requests use the publishable key and real user access tokens.

```json
{
  "publishable_key": "YOUR_PUBLISHABLE_KEY",
  "secret_key": "YOUR_SECRET_KEY"
}
```

Set the paths and project details, then run the suite from the repository root:

```sh
export SUPABASE_HOSTED_TEST=1
export SUPABASE_HOSTED_URL='https://YOUR_PROJECT_REF.supabase.co'
export SUPABASE_HOSTED_CREDENTIALS='/absolute/path/to/credentials.json'
export PGSPRITE_URL='postgresql://postgres@db.YOUR_PROJECT_REF.supabase.co:5432/postgres'
export PGPASSFILE='/absolute/path/to/pgpass'
go build -o ./bin/pg-sprite ./cmd/pg-sprite
export SUPABASE_HOSTED_BIN="$PWD/bin/pg-sprite"
go test -count=1 -timeout=10m -v ./integration/supabase/hosted
```

A successful case prints `--- PASS: TestHostedDeclarativeRLS`. With the opt-in
unset, cases print a skip reason and make no database or API requests. The baseline
requires a direct hostname matching the API project. Do not run multiple copies
against the same project: publication changes affect its shared Realtime service.

To test the session pooler, set `SUPABASE_HOSTED_SESSION_URL` to its URL from
**Connect** and add a password-file entry for that exact endpoint. Its host,
port, and username differ from the direct endpoint. A missing URL produces an
explicit skip.

Transaction-pooler refusal stays in the [controlled local suite](../../../pkg/dbconn/supabase_integration_test.go).
An idle hosted transaction pooler can reuse one backend and pass the session
probe, so a hosted assertion cannot reliably prove refusal. A passing probe
does not make transaction pooling supported; use direct or session connections.

To run the CLI lifecycle independently of Realtime and pooler tests:

```sh
go test -count=1 -timeout=5m -v ./integration/supabase/hosted -run '^TestHostedCLI'
```

These cases do not change Realtime publication membership. Passing them establishes
only the capabilities below; it does not clear failures in the separate Realtime
cases. For example, a successful column case prints
`--- PASS: TestHostedCLIAddColumn`.

## TLS checks without API keys

TLS tests use only `PGSPRITE_URL`, `PGPASSFILE`, and the opt-in. Supply the direct
URL with `sslmode=verify-full` and `sslrootcert` pointing at the CA downloaded from
**Database Settings → SSL Configuration**. Then run:

```sh
go test -count=1 -timeout=2m -v ./integration/supabase/hosted -run '^TestHostedTLS'
```

Expected cases are `TestHostedTLSDefault`, `TestHostedTLSRequire`,
`TestHostedTLSVerifyFull`, `TestHostedTLSRejectUntrustedCA`, and
`TestHostedTLSRejectWrongHostname`. Successful connections assert encryption using
`pg_stat_ssl`; negative cases require the specific certificate-trust or hostname
error, not just any failed connection. Verification cases skip explicitly when
`sslrootcert` is absent. The hostname negative case changes the expected TLS name
while still dialing the real database; it uses the underlying pgx driver to inject
that mismatch. These tests make no schema changes and do not test server-side
rejection of plaintext connections.

## What the cases establish

| Case | Passing result |
| --- | --- |
| `TestHostedCLIAuthDefault` | A desired `auth.uid()` default uses the real HTTP caller; spoofing another owner is denied |
| `TestHostedCLIAuthForeignKey` | A validated reference to real Auth users rejects orphan writes |
| `TestHostedCLIUniqueIndexFailure` | Duplicate data causes a typed failure and a reported invalid index; rows and access survive |
| `TestHostedRLSCancellationPreservesAccess` | Cancellation after a live policy drop rolls back the transaction; read and write isolation survive |
| `TestHostedCLICreateExportAndRLS` | Desired table creation, export/diff convergence, explicit RLS preview/apply, real tenant API isolation, and an unchanged second apply |
| `TestHostedCLIAddColumn` | A defaulted column is applied without losing existing data or RLS access controls |
| `TestHostedCLIAddIndex` | The new index is valid; table identity, rows, policies, and grants remain unchanged |
| `TestHostedCLIFailedNotNull` | Failed validation preserves NULL data and tenant access, leaves the column nullable, and reports the retained unvalidated helper constraint |
| `TestHostedCLIRefuseDropColumn` | Destructive preview and apply are refused without dropping a column |
| `TestHostedCLIRefuseCopySwap` | The unavailable copy-and-swap plan is refused before its safe prefix can run |
| `TestHostedCLIRLSLockFailure` | A lock-budget failure leaves existing policies and tenant access intact |
| `TestHostedRealtimeContinuity/initialize` | An active reader and a delivered baseline for both tenants precede all pg-sprite DDL |
| `TestHostedRealtimeContinuity/no_DDL_control` | Both tenants receive updates before schema changes |
| `TestHostedRealtimeContinuity/refuse_volatile_UUID` | Imperative and desired UUID-default changes are refused without applying their safe prefix; data and events survive |
| `TestHostedRealtimeContinuity/refuse_volatile_timestamp` | The same refusal checks hold for `clock_timestamp()` |
| `TestHostedRealtimeContinuity/add_column` | A nullable column preserves events and API isolation on the same table and sockets |
| `TestHostedRealtimeContinuity/add_index` | A concurrent index preserves events, API isolation, and table identity |
| `TestHostedDeclarativeRLS` | Policy changes alter real users' HTTP access; removing the last policy denies reads while retaining the data |
| `TestHostedSessionPooler` | A column change succeeds through the supplied session endpoint |

A passing refusal case means **safe rejection**, not support for executing that
DDL. None of these tests establishes support for every Supabase feature or plan.

Each test uses random fixture names and fails on a collision instead of deleting
an existing table. Cleanup has its own bounded context and deletes only objects
created by that case. If the process is forcibly terminated, inspect the test's
`pgsprite_hosted_…` tables and `pgsprite-…@example.com` users before removing any
leftovers. Do not reset `public` or delete unrelated Auth users.

## Realtime fixture lifecycle

`TestHostedRealtimeContinuity` keeps one table, publication membership, two users,
and two sockets for all cases. Setup waits for an active wal2json reader, verifies
the authenticated subscriptions, then sends one baseline update and requires
delivery to both users. Initialization failures fail the test before DDL runs.

The publication-startup budget is 75 seconds; each event must arrive within 30
seconds. Protocol heartbeats keep sockets alive during setup. There are no fixed
readiness sleeps, retried writes, or reconnects. This setup predicate is specific
to the fixture, not a public Supabase readiness API. The suite tests whether
pg-sprite preserves established delivery, not Supabase's cold-start guarantees.

To validate a fixture fix with the repository's fail-fast, race-enabled procedure:

```sh
scripts/test-flaky.sh TestHostedRealtimeContinuity 10 ./integration/supabase/hosted
```

The script stops at the first failure. A successful validation prints
`PASSED all 10 iterations`; it never retries a failed run to obtain a pass.
