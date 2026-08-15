# CLI output examples

Representative, real outputs for every shape the CLI produces: the human
dry-run report, the JSON plan report for each disposition, and the linter.
All were captured against the compose database (`make db-up`, PostgreSQL 16)
with this schema:

```sql
CREATE TABLE users (id bigint PRIMARY KEY, email text);
CREATE TABLE events (id bigint, created date) PARTITION BY RANGE (created);
```

The dry-run exit code is a contract: **0** when every statement is
executable, **2** (the refusal exit code) when any statement would be
refused. CI can gate on it without parsing JSON. The JSON report schema is
[plan-report.md](plan-report.md); the diagnostic codes are documented at
[postgres-online-ddl-reference.md#dry-run-diagnostic-codes](postgres-online-ddl-reference.md#dry-run-diagnostic-codes).

## Human dry-run report (text)

Each finding is a labeled diagnostic — `severity[code]:` on its own line,
content indented beneath it — followed by the doc anchors for every code and
a plan summary. Here the submitted `ADD CONSTRAINT ... UNIQUE` blocks as
written, so pg-sprite substitutes the safer online sequence (exit 0):

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run
statement 1:
  ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);

warning[safer-idiom]:
  ADD CONSTRAINT users_email_key — holds a blocking lock on the table for
  the whole operation — writes (and for some forms reads) wait until it
  finishes

help:
  pg-sprite will run a safer online sequence instead:
  1. CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "users" ("email");
  2. ALTER TABLE "users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key";

note:
  each step commits on its own — not transactionally equivalent, and the
  sequence must not run inside a transaction block

docs:
  https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md#safer-idiom

plan:
  public.users (PostgreSQL 16.14 (Debian 16.14-1.pgdg13+1)) — 1 statement, 2 steps to run, 0 refused

dry-run:
  nothing was executed

apply:
  re-run without --dry-run
```

Every shape below is the same report as JSON (`--json`); the text rendering
is display only, the JSON is the machine contract.

## Runs as written (`metadata-only`) — exit 0

A safe submitted form executes unchanged: `exec_sql` is the statement
itself.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN note text' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "execute",
  "fingerprint": "sha256:653b46e2647e787478db2feb7115b8fe440e392115d465278fec0cfd33892484",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD COLUMN note text",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD COLUMN note",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "ALTER TABLE users ADD COLUMN note text"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

## Safer-sequence substitution (`safer-idiom`) — exit 0

The same report as the text example above: the decision carries the
substituted sequence in `safer_sql`, and `exec_sql` is what `migrate` would
run.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "execute",
  "fingerprint": "sha256:e2b2a51466547f4469754c9296524844f868eb5a967aba1e290ed8aa7ee63996",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD CONSTRAINT users_email_key",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom",
          "safer_sql": [
            "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
            "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
          ],
          "safer_sql_execution": "autocommit-each-step"
        }
      ],
      "exec_sql": [
        "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
        "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

## Refused: no online rewrite exists (`rewrite-required`) — exit 2

The column and its constraint arrive in one statement, so no online
substitution can be constructed; the change must be rewritten as separate
online steps. No `exec_sql` is offered.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN nickname text UNIQUE' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "rewrite-required",
  "fingerprint": "sha256:9773ff32c62b04e97bacb0ae85cf0ad528164c7baafc33db66d8adf20d7a5674",
  "statements": [
    {
      "sql": "ALTER TABLE users ADD COLUMN nickname text UNIQUE",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "rewrite-required",
      "decisions": [
        {
          "operation": "ADD COLUMN nickname",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom"
        }
      ]
    }
  ]
}
```

## Refused: needs a backend this build lacks (`backend-unavailable`) — exit 2

A genuine table rewrite routes to the copy-and-swap backend, which is not
implemented yet.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN id TYPE text' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "unavailable",
  "fingerprint": "sha256:fb4836fa89efab4280be1680b8c3181fb902ec67131ae5959df3d5e872f1b3c5",
  "statements": [
    {
      "sql": "ALTER TABLE users ALTER COLUMN id TYPE text",
      "destructive": false,
      "route": "copy-and-swap",
      "backend": "copy-and-swap",
      "disposition": "unavailable",
      "decisions": [
        {
          "operation": "ALTER COLUMN id TYPE text",
          "destructive": false,
          "route": "copy-and-swap",
          "reason": "type-rewrite"
        }
      ]
    }
  ]
}
```

## Refused: target facts forbid the plan (`unsupported-partitioned-parent`) — exit 2

The routed plan builds an index concurrently, but the target is a
partitioned parent — PostgreSQL cannot `CREATE INDEX CONCURRENTLY` there.
The refusal cause is the report-level `reason`.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'CREATE INDEX events_created_idx ON events (created)' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "events",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "refuse",
  "reason": "unsupported-partitioned-parent",
  "fingerprint": "sha256:e0cebea56d6c5577722d17be16442b06303a817af0c009913d746fd3d1c379e0",
  "statements": [
    {
      "sql": "CREATE INDEX events_created_idx ON events USING btree (created)",
      "destructive": false,
      "route": "native",
      "disposition": "refuse",
      "reason": "unsupported-partitioned-parent",
      "decisions": [
        {
          "operation": "CREATE INDEX events_created_idx",
          "destructive": false,
          "route": "native",
          "reason": "safer-idiom"
        }
      ]
    }
  ]
}
```

## Executable but destructive (`destructive`) — exit 0

A `DROP COLUMN` routes to execute — the destructive flag is surfaced for
the reviewer or orchestrator to gate on; `migrate` itself does not block it.

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite migrate --alter 'ALTER TABLE users DROP COLUMN email' --dry-run --json
{
  "format_version": 1,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "disposition": "execute",
  "fingerprint": "sha256:77caa837ce1750b23eaaf9f988ede933f624c8c4c6de1dc2fee7780bf64a4c1f",
  "statements": [
    {
      "sql": "ALTER TABLE users DROP email",
      "destructive": true,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "DROP COLUMN email",
          "destructive": true,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "ALTER TABLE users DROP email"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

## Lint

The offline linter needs no database: it flags blocking idioms in a DDL file
(or stdin) in the conventional `file:line:column: severity` shape, with the
safer form suggested beneath each finding. Given:

```sql
-- /tmp/changes.sql
CREATE INDEX users_email_idx ON users (email);
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
```

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite lint /tmp/changes.sql
/tmp/changes.sql:1:1: warning: blocking-idiom — CREATE INDEX users_email_idx
  safer form (not equivalent — see https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md): CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email);
  run each statement in its own transaction, never one block; after a failed CONCURRENTLY build, check pg_index.indisvalid and rebuild
/tmp/changes.sql:2:1: warning: blocking-idiom — ADD CONSTRAINT users_email_key
  safer form (not equivalent — see https://github.com/block/pg-sprite/blob/main/docs/postgres-online-ddl-reference.md): CREATE UNIQUE INDEX CONCURRENTLY "users_email_key" ON "users" ("email");
  ALTER TABLE "users" ADD CONSTRAINT "users_email_key" UNIQUE USING INDEX "users_email_key";
  run each statement in its own transaction, never one block; after a failed CONCURRENTLY build, check pg_index.indisvalid and rebuild
```

The same findings as JSON (schema: [lint-report.md](lint-report.md)):

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite lint /tmp/changes.sql --json
{
  "format_version": 1,
  "postgres_versions": "14-18",
  "findings": [
    {
      "statement": 1,
      "line": 1,
      "column": 1,
      "sql": "CREATE INDEX users_email_idx ON users (email)",
      "operation": "CREATE INDEX users_email_idx",
      "code": "blocking-idiom",
      "severity": "warning",
      "reason": "safer-idiom",
      "suggestion": [
        "CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email)"
      ],
      "suggestion_execution": "autocommit-each-step"
    },
    {
      "statement": 2,
      "line": 2,
      "column": 1,
      "sql": "ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)",
      "operation": "ADD CONSTRAINT users_email_key",
      "code": "blocking-idiom",
      "severity": "warning",
      "reason": "safer-idiom",
      "suggestion": [
        "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"users\" (\"email\")",
        "ALTER TABLE \"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
      ],
      "suggestion_execution": "autocommit-each-step"
    }
  ],
  "errors": 0,
  "warnings": 2
}
```

## Diff

The declarative front door: point `diff` at a reviewed desired-state file
and it emits the classified statements that converge the live table onto
it. Given the live `users` table above and:

```sql
-- /tmp/users.sql
CREATE TABLE users (
    id bigint PRIMARY KEY,
    email text,
    nickname text,
    CONSTRAINT u UNIQUE (email)
);
```

```
~/kiran01bm/github/pg-sprite main ./bin/pg-sprite diff --desired /tmp/users.sql
-- plan derived by pg-sprite diff; execute statements via pg-sprite migrate,
-- which refuses blocking forms — running this script directly bypasses that gate
-- native (metadata-only)
ALTER TABLE public.users ADD COLUMN nickname text;
```
