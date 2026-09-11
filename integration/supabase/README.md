# Reading the Supabase tests

These tests show what pg-sprite can do on the pinned local Supabase stack and
where it must stop. Start with the SQL in a named test, then read its expected
outcome. The [Supabase guide](../../docs/supabase.md#what-works-today) maps the
results to the workflows you can use today and includes commands to run the suite.

## What a passing test means

| Test expectation | What `PASS` establishes |
| --- | --- |
| Succeeds | pg-sprite executed the requested change and the database checks matched the expected result |
| Requires unavailable copy-and-swap | pg-sprite refused the change with `backend-unavailable`; it did not execute the requested DDL |
| Refuses a destructive desired plan | The entire plan was refused, including any supported additions in the same plan |
| Execution fails | The expected error occurred, and the test verified the durable state left behind |

A passing refusal test does **not** mean the change is supported. When the engine
can safely execute it, that case needs new success and preservation expectations.
Likewise, a failed test means the observed behavior differs from its expectations;
read the assertion before concluding that a capability is unsupported.

## Follow a case

In [ddl_matrix_test.go](ddl_matrix_test.go), each DDL operation has its own named
test with the complete SQL. `%s` is the fixture table name. For successful native
changes, `proof` queries the real database and `want` gives the expected value.
The shared helper also checks existing rows, table identity, row-level security
policies, grants, publication membership, and tenant API access.

Copy-and-swap cases show both an explicit `ALTER TABLE` and a desired
`CREATE TABLE` definition. The `safe_prefix` column is intentional: neither
entry point may apply that harmless addition before refusing the unsupported
change. The helper compares schema and data snapshots, then verifies tenant reads
and new Realtime events on the same subscriptions opened before the refusal.

In [failure_test.go](failure_test.go), unsuccessful execution has specific
consequences. A lock-budget refusal leaves the table unchanged. Failed NOT NULL
validation leaves a `NOT VALID` check constraint. A failed concurrent unique
index build leaves an invalid index. These tests require the expected outcome
and verify those leftovers; they do not assume every failure rolls back all work.

[realtime_test.go](realtime_test.go) separately verifies INSERT/UPDATE delivery
while a column addition and concurrent index build run with ongoing writes.
Preserving publication membership alone would not prove event delivery.

## Scope of the evidence

The suite uses real local Supabase services and fixture-signed user tokens.
It does not establish hosted-project compatibility, signup/login behavior, or
support for every DDL operation. Each test covers its stated operation and
assertions. Keep the guide aligned as those checks expand.

## Maintaining the fixture

CI runs each service test once. Investigate intermittent failures and fix their
root cause rather than adding retries or routine repetitions.

Follow the repository's [test conventions](../../AGENTS.md#build-and-test):
investigate failures, find the root cause, and never increase timeouts to fix a
flake. For Supabase timing failures, inspect the assertion and service logs,
including final-server and subscription readiness. After fixing the cause, use
[`scripts/test-flaky.sh`](../../scripts/test-flaky.sh) to prove the fix holds
before declaring it resolved. This targeted verification is separate from normal
CI execution.

**Monthly image review (manual maintainer task):**

1. Check upstream releases for the images in `compose/supabase.yml`. Keep both
   version tags and immutable digests; review changes to roles, initialization,
   pooling, and replication before selecting updates
2. Open an update PR with the new pins. For Supavisor, compare `pooler.exs` with
   its upstream source and update its source link and validated version together
3. Run the required Supabase CI job.
   Investigate failures before accepting new versions
4. Update the tested versions and any changed limits in `docs/supabase.md` in
   the same PR. Record why an update is deferred in the update PR when applicable

The image review is a maintainer procedure, not an automated update bot. Fresh CI
runners pull the pinned images on each run, which also exposes unavailable pins.
