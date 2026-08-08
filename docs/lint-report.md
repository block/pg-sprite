# The lint report contract

The lint report is the machine-readable result of `pg-sprite lint` — the offline checker
that runs the same parse-and-classify pipeline as the front doors, with zero live facts and
no database. It exists so CI can gate a DDL script before any environment sees it. This
document is the contract: the fields, the closed vocabularies, and the behavior required of
a consumer. The Go source of truth is `pkg/lint`; tests in `pkg/lint` and `internal/cli` pin
everything documented here.

## Versioning: `format_version`

Every report carries `format_version`. A consumer that does not recognize the version must
**reject the report** — never guess at field semantics. The version covers the field shape
and the closed vocabularies below (codes, severities, and the embedded planner reasons):
adding a value to any of them is a contract change and bumps `format_version`, even if no
field is added or renamed.

### Relationship to the plan report

The lint report and the [plan report](plan-report.md) are **versioned independently** — each
carries its own `format_version`, and they move separately. They share one vocabulary: a
finding's `reason` draws from the plan report's Reasons set, and the lint `format_version`
pins the reason set a lint consumer must understand, exactly as the plan `format_version`
does for plan consumers. A consumer of both must track both versions; understanding plan v1
says nothing about lint v1.

## Consumer behavior for unknown values

Every enum field draws from a closed vocabulary listed here. A consumer that meets a value
it does not recognize must **treat the finding as an error and fail the gate** — never
ignore it and proceed. This mirrors the linter's own posture: an unknown planner route
becomes an error-severity refusal, never a silent pass.

## Offline conservatism

The linter sees only the script — no catalog, no column types, no server. Every judgment is
therefore the planner's fail-closed judgment: a change lint passes without findings can
still sharpen at execution time, but a change lint flags will never quietly get worse. Two
codes make the conservatism visible instead of burying it:

- `possible-table-rewrite` marks a decision the planner could not verify (see the codes
  table) — the engine would take the heavy path, but a live database might prove the change
  free.
- `destructive` includes every index drop, because the linter cannot see whether an index
  is unique — and a dropped unique index whose gap admitted duplicate writes cannot be
  recreated at all.

## Report fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `format_version` | int | always | Contract version; reject unknown versions. |
| `postgres_versions` | string | always | The inclusive PostgreSQL major-version range the offline rules are derived for (`14-18`, see [postgresql-version-support.md](postgresql-version-support.md)). The linter never sees a server, so a stored report names the assumptions behind it instead. |
| `findings` | array | always | The findings in statement order; `[]` (never `null`) means the script is clean. |
| `errors` | int | always | Count of error-severity findings. |
| `warnings` | int | always | Count of warning-severity findings. |

## Finding fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `statement` | int | always | 1-based index of the statement in the script. |
| `line` | int | always | 1-based source line of the statement's first token, for CI annotations. |
| `column` | int | always | 1-based source column of the statement's first token. |
| `sql` | string | always | The statement's **verbatim source text** (without the trailing semicolon), so it can be found in the source by exact match. Unlike the plan report's canonical rendering, a lint finding points back at the file the author wrote. |
| `operation` | string | always | Operator-facing label of the flagged operation (`DROP COLUMN legacy_a`). Display only — never branch on it. |
| `code` | string | always | The typed finding kind (see Codes). Automation branches on this, never on prose. |
| `severity` | string | always | What the engine would do about it (see Severities). |
| `reason` | string | classifier findings | The planner's typed cause, drawn from the plan report's Reasons vocabulary. Absent for destructive findings, which are a property of the operation, not a routing decision. |
| `suggestion` | array | when constructible | The ordered safer SQL to run instead, present only for `blocking-idiom` findings where the planner constructed the rewrite. Its absence still means the submitted form blocks — the planner does not construct rewrites for multi-operation statements or for operations that need catalog knowledge (ATTACH PARTITION's proving CHECK). |

## Codes (`code`)

| Value | Severity | Example statement | Meaning |
|---|---|---|---|
| `unsupported-operation` | error | `ALTER TABLE t ADD CONSTRAINT x EXCLUDE USING gist (room WITH =)` | No known safe path — the engine refuses it. |
| `blocking-idiom` | warning | `CREATE INDEX i ON t (c)` | The submitted form blocks readers or writers and a safer native form exists; `suggestion` carries it when the linter can construct one. |
| `table-rewrite` | warning | `ALTER TABLE t ALTER COLUMN c TYPE jsonb USING c::jsonb` | The operation provably rewrites the table — only the engine's copy-and-swap path can run it online. |
| `possible-table-rewrite` | warning | `ALTER TABLE t ALTER COLUMN c TYPE bigint` | The linter cannot verify the operation against live column facts, so the engine would fail closed to the rewrite path — but the change may be a free relabel a live database would prove. |
| `app-breaking-rename` | warning | `ALTER TABLE t RENAME COLUMN email TO email_address` | PostgreSQL runs a column or table rename as a metadata-only catalog flip, but a rename cannot land atomically across running application instances — code still referencing the old name breaks the instant it commits. For a column, expand/contract instead: add the new column, dual-write and backfill, switch reads, then drop the old column as its own reviewed change. For a table, coordinate the rename with the application deploy that adopts the new name. Index renames are not flagged — SQL never references an index by name. |
| `destructive` | warning | `ALTER TABLE t DROP COLUMN legacy` | The operation discards live structure (a column, constraint, or index drop) and cannot be undone by re-running the schema. |

## Severities (`severity`)

| Value | Meaning | Exit behavior |
|---|---|---|
| `error` | The engine would refuse the statement — it cannot execute as written. | `pg-sprite lint` exits non-zero. |
| `warning` | The engine would execute the statement, but it has a safer form, needs a heavier path, or discards live structure. | Warnings alone exit zero. |

Only error-severity findings flip the exit code today; a policy layer (per-code gating,
inline suppression) is a planned extension and will be introduced as a contract change.

## Text output

Without `--json`, findings render one per line in the conventional linter shape —
`name:line:column: severity: code — operation` — where `name` is the linted file path or
`<stdin>`. The text form is for humans and editors; automation consumes the JSON report.
A clean script prints nothing and exits zero.
