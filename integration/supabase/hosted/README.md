# Hosted Supabase checks

Use this suite on a **disposable hosted project**. It creates real Auth users,
application tables, policies, and publication entries, then removes its fixtures.
It leaves project settings alone. No Docker services or locally signed JWTs are used.

This is an opt-in complement to the [local suite](../README.md), not a replacement
for its larger DDL matrix or a required hosted CI job. Hosted Realtime startup has
shown intermittent missing events even in the baseline that runs no pg-sprite DDL.
Failures remain failures; the suite does not retry them or restart managed services.

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

## What the cases establish

| Case | Passing result |
| --- | --- |
| `TestHostedRealtimeBaseline` | Both tenants receive INSERT and UPDATE events without any pg-sprite DDL |
| `TestHostedRealtimeContinuousBaseline` | All 20 distinct baseline writes reach the subscribed user; no DDL runs |
| `TestHostedNativeColumnAndIndex` | Nullable column and concurrent index changes preserve table identity, API isolation, and event delivery |
| `TestHostedRefuseVolatileUUID` | Imperative and desired UUID-default changes are refused without applying the safe prefix or changing data; the subscriptions still deliver events |
| `TestHostedRefuseVolatileTimestamp` | The same refusal and continuity checks hold for `clock_timestamp()` |
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

A channel acknowledgement is not an event-delivery guarantee. Diagnose baseline
failures separately from failures after DDL. Do not hide missing events with
longer timeouts, retries, or a service restart. The upstream Realtime implementation
periodically refreshes publication membership; rapidly creating and dropping
published tables is a fixture-lifecycle consideration, not yet a proven cause of
all observed hosted failures.
