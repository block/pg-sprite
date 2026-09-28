# Hosted Supabase checks

Use this suite on a **disposable hosted project**. It creates real Auth users,
application tables, policies, and publication entries, then removes its fixtures.
It leaves project settings alone. No Docker services or locally signed JWTs are used.

This is an opt-in complement to the [local suite](../README.md), not a replacement
for its larger DDL matrix or a required hosted CI job. Hosted Realtime startup has
shown intermittent missing events even in the baseline that runs no pg-sprite DDL.
Cold-start diagnostics are a separate opt-in and retain missing events as failures.
The continuity suite initializes one published fixture and keeps it through all
schema-change cases. Neither path retries writes or restarts managed services.

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

To test poolers, also set `SUPABASE_HOSTED_SESSION_URL` and
`SUPABASE_HOSTED_TRANSACTION_URL` to the corresponding URLs from **Connect**.
Their host, port, and username differ from the direct endpoint. Add password-file
entries for those exact endpoints. Missing pooler URLs produce explicit skips;
a failed connection is not accepted as proof that transaction pooling was refused.

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
| `TestHostedTransactionPoolerRefusesSessionAffinity` | Connection setup returns the specific session-affinity refusal |

A passing refusal case means **safe rejection**, not support for executing that
DDL. None of these tests establishes support for every Supabase feature or plan.

Each test uses random fixture names and fails on a collision instead of deleting
an existing table. Cleanup has its own bounded context and deletes only objects
created by that case. If the process is forcibly terminated, inspect the test's
`pgsprite_hosted_…` tables and `pgsprite-…@example.com` users before removing any
leftovers. Do not reset `public` or delete unrelated Auth users.

## Realtime initialization and cold-start diagnostics

`TestHostedRealtimeContinuity` keeps one table, publication membership, two users,
and two sockets for all cases. Setup waits for an active wal2json reader, then
sends one baseline update and requires delivery to both users. No DDL runs if
initialization fails. A 75-second **setup** deadline spans the approximately
60-second publication refresh observed in the hosted investigation. Every actual
event still has the original 30-second deadline, with no retries or reconnects.
Protocol heartbeats keep the sockets alive during setup. This readiness predicate
is fixture-specific; it is not a public Supabase readiness API or a production SLA.

The hosted investigation reproduced a subscription acknowledgement while the slot
was absent after an empty-to-nonempty publication transition. Twelve distinct writes
committed without events during the observation; a later write arrived when the
slot started. The database rows were not lost. This is why schema-change continuity
and cold-start delivery are separate results.

An independent comparison using `@supabase/supabase-js` 2.116.0 reproduced the
gap with authenticated subscriptions. Both the default client and
`postgres_changes_options: {wait: true}` acknowledged before the reader started.
Of 13 committed INSERTs, both received only the last two; the earlier 11 did not
arrive during the observation. Wire-level checks ruled out SDK callback filtering.
No pg-sprite DDL ran. A separate control that waited for the reader delivered all
three writes to both clients, without retrying writes. These observations do not
identify the hosted server revision or establish a public readiness contract.

Run the startup diagnostics explicitly:

```sh
SUPABASE_HOSTED_STARTUP_DIAGNOSTICS=1 \
  go test -count=1 -timeout=3m -v ./integration/supabase/hosted -run '^TestHostedRealtimeStartup'
```

`TestHostedRealtimeStartupBaseline` checks immediate INSERT/UPDATE delivery;
`TestHostedRealtimeStartupContinuous` requires all 20 distinct writes; and
`TestHostedRealtimeStartupSlotReady` checks for an active reader before writing.
These diagnostics have known intermittent failures. Without the extra opt-in they
print an explicit skip reason; a passing normal suite does **not** certify cold-start
delivery. Keep failed startup results when reporting compatibility.

To validate a fixture fix with the repository's fail-fast, race-enabled procedure:

```sh
scripts/test-flaky.sh TestHostedRealtimeContinuity 10 ./integration/supabase/hosted
```

The script stops at the first failure. Ten passing runs are stability evidence for
the initialized fixture, not an excuse to retry a failing run or a guarantee about
all hosted Realtime behavior.
