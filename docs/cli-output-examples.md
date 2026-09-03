# CLI output examples

Representative, real JSON outputs for every shape the CLI produces: the
plan report for each dry-run disposition, the execution verdict, the
linter, and diff. The human text rendering of the same reports is display
only — see the animated demos in [demos/](demos/) for how it reads; the
JSON is the machine contract. The text reports color their diagnostic
labels when stdout is a terminal (`--color=auto|always|never`;
[`NO_COLOR`](https://no-color.org) and `TERM=dumb` disable auto-detection);
the JSON and `diff --sql` outputs are never colored. All were captured
verbatim from a real session against the compose database (`make db-up`,
PostgreSQL 16) —
`$` marks the command, everything after it is the tool's output — with
this schema:

```sql
CREATE TABLE users (id bigint PRIMARY KEY, email text);
CREATE TABLE events (id bigint, created date) PARTITION BY RANGE (created);
```

`migrate` exit codes follow the dry-run contract (0 = executable, 2 = refused —
including a target table that does not exist, so a typo'd name cannot gate green)
defined with the [diagnostic codes](postgres-online-ddl-reference.md#dry-run-diagnostic-codes);
CI can gate on the exit code without parsing JSON. The gate is refusals only: a
destructive-but-executable change (`DROP COLUMN`) warns and exits 0 — a gate
that must stop drops checks `.statements[].destructive` in the JSON report.
Exit 2 covers every refusal cause; the exit code answers *should this
proceed*, the report's typed `reason` and disposition answer *why not*. The contract is
`migrate`'s: `lint` exits 1 when a script has error-severity findings
(warnings alone exit 0), and `diff` prints the plan and exits 2 when it
contains a statement execution would refuse — a missing table is diff's
greenfield case (the plan creates the table), not a refusal, unlike the
dry run. Statement kinds `migrate` does not support (`DROP INDEX`,
`REINDEX`, `CREATE TABLE`, and anything that is not `ALTER TABLE` or
`CREATE INDEX`) emit the same refusal verdict on `--dry-run` as on apply —
a verdict, not a plan report — and exit 2. The JSON report schema is
[plan-report.md](plan-report.md).

- [Codes used in these examples](#codes-used-in-these-examples)
- [Refusal reasons](#refusal-reasons)
- [Migrate](#migrate)
  - [Runs as written (`metadata-only`) — exit 0](#runs-as-written-metadata-only--exit-0)
  - [Safer-sequence substitution (`safer-idiom`) — exit 0](#safer-sequence-substitution-safer-idiom--exit-0)
  - [Real execution](#real-execution)
  - [Refused: no online rewrite exists (`rewrite-required`) — exit 2](#refused-no-online-rewrite-exists-rewrite-required--exit-2)
  - [Refused: needs a backend this build lacks (`backend-unavailable`) — exit 2](#refused-needs-a-backend-this-build-lacks-backend-unavailable--exit-2)
  - [Refused: target facts forbid the plan (`unsupported-partitioned-parent`) — exit 2](#refused-target-facts-forbid-the-plan-unsupported-partitioned-parent--exit-2)
  - [Executable but destructive (`destructive`) — exit 0](#executable-but-destructive-destructive--exit-0)
- [Lint](#lint)
  - [Blocking idioms flagged (`blocking-idiom`) — exit 0](#blocking-idioms-flagged-blocking-idiom--exit-0)
- [Diff](#diff)
  - [Converge to the desired state (`metadata-only`) — exit 0](#converge-to-the-desired-state-metadata-only--exit-0)

## Codes used in these examples

Every diagnostic in the examples below carries one of these typed codes. Each
is a one-line summary; the linked reference entry is authoritative.

| Code | Meaning |
|---|---|
| [`metadata-only`](postgres-online-ddl-reference.md#metadata-only) | A brief catalog-only change: at most a short `ACCESS EXCLUSIVE` lock, no table scan or rewrite. Runs as written. |
| [`safer-idiom`](postgres-online-ddl-reference.md#safer-idiom) | The statement blocks as written but an equivalent online sequence exists; pg-sprite substitutes it. Each step commits on its own — the sequence is not transactionally equivalent to the original. |
| [`type-rewrite`](postgres-online-ddl-reference.md#type-rewrite) | The column type change is not binary-coercible, so PostgreSQL rewrites the whole table under `ACCESS EXCLUSIVE`. Routed to the copy-and-swap backend. |
| [`rewrite-required`](postgres-online-ddl-reference.md#rewrite-required) | The statement blocks as written and no online replacement could be constructed. Refused; split the change into separate online steps. The statement's `guidance` field names the typed manual path ([Guidance vocabulary](suggest-report.md#guidance-guidance)). |
| [`backend-unavailable`](postgres-online-ddl-reference.md#backend-unavailable) | The plan routes to a backend (online shadow-table copy with cutover) this build does not implement yet. Refused; nothing executes. |
| [`unsupported-partitioned-parent`](postgres-online-ddl-reference.md#unsupported-partitioned-parent) | The routed plan builds an index concurrently but the target is a partitioned parent, where PostgreSQL cannot `CREATE INDEX CONCURRENTLY`. Refused. |
| [`unsupported-statement`](postgres-online-ddl-reference.md#unsupported-statement) | The planner knows no safe path for the statement (for example `SET UNLOGGED`, `CLUSTER ON`). Refused — the same typed reason the run path's refusal verdict carries. |
| [`table-not-found`](postgres-online-ddl-reference.md#table-not-found) | The target table does not exist, so classification fell back to zero facts; running without `--dry-run` would fail. The dry run exits 2 and the report carries `table_exists: false`. |
| [`destructive`](postgres-online-ddl-reference.md#destructive) | The change discards live data or structure (`DROP COLUMN`, `DROP TABLE`, truncating conversions). A warning alongside the routing decision, not a refusal. |
| [`blocking-idiom`](lint-report.md#codes-code) | Lint-only code: the submitted form blocks readers or writers and a safer native form exists; the finding's `suggestion` carries the safer SQL when the linter can construct it. |

## Refusal reasons

Every refusal verdict (`"outcome": "refused"`, exit 2) carries exactly one of
these typed `reason` tokens — the value automation switches on; prose belongs
in `detail`. The set is closed and pinned by test (`verdict.Reasons()`).

| Reason | Meaning |
|---|---|
| `unsupported-statement` | No safe path is known for the statement — only `ALTER TABLE` and `CREATE INDEX` reach classification — or a greenfield create plan carries a shape the create path refuses (`PARTITION OF`, `IF NOT EXISTS`). |
| `index-statement` | Index maintenance (`DROP INDEX`, `REINDEX`) has a native safe idiom (`CONCURRENTLY`) and is never attempted; the verdict's `safer_idiom` names it. |
| `not-native-safe-table-too-large` | The size guard skipped the optimistic attempt: the table exceeds the configured bound and the change is not provably metadata-only. |
| `insufficient-privileges` | The connected role lacks the access the change needs; `detail` names the exact missing GRANT (see [engine-role.md](engine-role.md)). |
| `unsupported-partitioned-parent` | The routed plan builds an index on a partitioned parent, where PostgreSQL cannot `CREATE INDEX CONCURRENTLY`. |
| `not-native-safe-budget-exceeded` | The optimistic attempt exceeded its lock or statement budget and was cancelled; the verdict's `cause` narrows which budget fired. |
| `not-native-safe-rewrite-required` | The submitted form blocks and must run as a safer native sequence, but none could be constructed. |
| `backend-unavailable` | The change routes to an execution strategy this build does not implement (copy-and-swap). |
| `destructive-change` | The desired-state plan discards live structure — a dropped column, constraint, index, or `NOT NULL` — and desired-state execution runs no destructive statement; run the drop deliberately instead ([execution model](execution-model.md)). |
| `plan-fingerprint-mismatch` | The plan recomputed at execution time does not carry the pinned fingerprint: the plan a reviewer approved is not the plan that would execute, so nothing runs ([execution model](execution-model.md)). |
| `create-collision` | The greenfield create plan's table name or a claimed index/constraint-index name is occupied. Nothing runs; re-derive the plan against the live catalog and review what it says now. Catalog absence checks handle existing occupants; duplicate-name SQLSTATEs remain the race backstop. |

## Migrate

The imperative front door: submit one DDL statement; pg-sprite classifies
it, routes it, and either runs it, substitutes a safer online sequence, or
refuses. `--dry-run --json` emits the plan report without executing.

### Runs as written (`metadata-only`) — exit 0

A safe submitted form executes unchanged: `exec_sql` is the statement
itself.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN note text' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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

### Safer-sequence substitution (`safer-idiom`) — exit 0

The submitted `ADD CONSTRAINT ... UNIQUE` blocks as written, so pg-sprite
plans the safer online sequence instead: the decision carries it in
`safer_sql`, and `exec_sql` is what `migrate` would run.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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

### Real execution

The same statement without `--dry-run` runs the substituted sequence for
real. The verdict names what was executed and every step that committed
(exit 0):

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --json
{
  "outcome": "executed-natively",
  "statement": "ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)",
  "table": "public.users",
  "detail": "the submitted form blocks; pg-sprite ran the safer native sequence instead — all 2 steps committed",
  "executed_sql": [
    "CREATE UNIQUE INDEX CONCURRENTLY \"users_email_key\" ON \"public\".\"users\" (\"email\")",
    "ALTER TABLE \"public\".\"users\" ADD CONSTRAINT \"users_email_key\" UNIQUE USING INDEX \"users_email_key\""
  ]
}
```

### Refused: no online rewrite exists (`rewrite-required`) — exit 2

The column and its constraint arrive in one statement, so no online
substitution can be constructed; the change must be rewritten as separate
online steps. No `exec_sql` is offered. The `guidance` field names the
typed manual path — here `add-column-then-constraint`: add the plain
column first, then build the constraint as a separate, named
`ADD CONSTRAINT` with its online pattern (see the
[suggest report's Guidance vocabulary](suggest-report.md#guidance-guidance)).

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN nickname text UNIQUE' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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
      ],
      "guidance": "add-column-then-constraint"
    }
  ]
}
```

### Refused: needs a backend this build lacks (`backend-unavailable`) — exit 2

A genuine table rewrite routes to the copy-and-swap backend, which is not
implemented yet.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users ALTER COLUMN id TYPE text' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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

### Refused: target facts forbid the plan (`unsupported-partitioned-parent`) — exit 2

The routed plan builds an index concurrently, but the target is a
partitioned parent — PostgreSQL cannot `CREATE INDEX CONCURRENTLY` there.
The refusal cause is the report-level `reason`.

```console
$ pg-sprite migrate --alter 'CREATE INDEX events_created_idx ON events (created)' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "events",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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

### Executable but destructive (`destructive`) — exit 0

A `DROP COLUMN` routes to execute — the destructive flag is surfaced for
the reviewer or orchestrator to gate on; `migrate` itself does not block it.

```console
$ pg-sprite migrate --alter 'ALTER TABLE users DROP COLUMN email' --dry-run --json
{
  "format_version": 2,
  "source": "alter",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
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

The offline linter needs no database: it flags blocking idioms in a DDL
file (or stdin) with the safer form suggested for each finding (schema:
[lint-report.md](lint-report.md)). Given:

```sql
-- /tmp/changes.sql
CREATE INDEX users_email_idx ON users (email);
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
```

### Blocking idioms flagged (`blocking-idiom`) — exit 0

```console
$ pg-sprite lint /tmp/changes.sql --json
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
it, as the same plan report (`"source": "diff"`). Given the live `users`
table above (with the unique constraint from the execution example) and:

```sql
-- /tmp/users.sql
CREATE TABLE users (
    id bigint PRIMARY KEY,
    email text,
    nickname text,
    CONSTRAINT users_email_key UNIQUE (email)
);
```

### Converge to the desired state (`metadata-only`) — exit 0

```console
$ pg-sprite diff --desired /tmp/users.sql --json
{
  "format_version": 2,
  "source": "diff",
  "schema": "public",
  "table": "users",
  "server_version": "16.14 (Debian 16.14-1.pgdg13+1)",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:4c349e89a66fed63dd07f693dae62e356695f2a586306cfd5a153e6c1efcd9f9",
  "statements": [
    {
      "sql": "ALTER TABLE public.users ADD COLUMN nickname text",
      "kind": "add-column",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD COLUMN nickname",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "ALTER TABLE public.users ADD COLUMN nickname text"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```
