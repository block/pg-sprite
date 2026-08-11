# The suggest report contract

The suggest report is the machine-readable result of `pg-sprite suggest` — the offline
advisory surface that maps DDL that is risky as written to the safer native form the engine
would run instead. It runs the same parse-and-classify pipeline as the front doors, with
zero live facts and no database, and it never gates: `suggest` always exits zero on a valid
script; `pg-sprite lint` owns the gate. This document is the contract: the fields, the
closed vocabularies, and the behavior required of a consumer. The Go source of truth is
`pkg/suggest`; tests in `pkg/suggest` and `internal/cli` pin everything documented here.

## Versioning: `format_version`

Every report carries `format_version`. A consumer that does not recognize the version must
**reject the report** — never guess at field semantics. The version covers the field shape
and the closed vocabularies below (caveats, guidance codes, and the embedded planner
reasons and execution contracts): adding a value to any of them is a contract change and
bumps `format_version`, even if no field is added or renamed.

### Relationship to the plan and lint reports

The suggest report, the [lint report](lint-report.md), and the
[plan report](plan-report.md) are **versioned independently** — each carries its own
`format_version`, and they move separately. They share vocabularies: a suggestion's
`reason` draws from the plan report's Reasons set and its `execution` from the plan
report's Execution contracts, and the suggest `format_version` pins the generation of
those sets a suggest consumer must understand, exactly as the plan and lint versions do
for their consumers. All three reports impose the same rule on unknown values, stated in
the next section.

## Consumer behavior for unknown values

Every enum field draws from a closed vocabulary listed here. A consumer that meets a value
it does not recognize must **fail closed** — treat the suggestion as unknown, refuse to run
its sequence, and surface it rather than ignore it. A caveat you cannot interpret may be the
one that says the sequence must not run inside your transaction.

## Report fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `format_version` | int | always | Contract version; reject unknown versions. |
| `suggestions` | array | always | The advisory results in statement order, one per safer-idiom decision; `[]` (never `null`) means every statement is already in its safest known form or is outside the advisory surface (refusals, table rewrites, and destructive drops are lint findings). |

## Suggestion fields

Every suggestion carries **exactly one** of `recommended` and `guidance`: a constructed
rewrite, or the typed manual path when the planner cannot construct one. Silence is not an
outcome — every statement `lint` flags `blocking-idiom` appears here.

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `statement` | int | always | 1-based index of the statement in the script. |
| `line` | int | always | 1-based source line of the statement's first token, so a consumer can annotate the advice onto the file it came from. |
| `column` | int | always | 1-based source column of the statement's first token. |
| `original` | string | always | The statement's **verbatim source text** (without the trailing semicolon), so it can be found in the source by exact match. |
| `operation` | string | always | Operator-facing label of the risky operation. Display only — never branch on it. |
| `reason` | string | always | The classifier's typed cause, drawn from the plan report's Reasons vocabulary; always `safer-idiom` under this version. |
| `recommended` | array | when constructible | The ordered safer SQL to run instead. A safer form of the submitted statement, not a semantic equivalent — it converges on the same declared end state with different locking, transactionality, and failure modes, which `caveats` names. |
| `execution` | string | with `recommended` | The typed execution contract for `recommended`, drawn from the plan report's Execution contracts vocabulary (`autocommit-each-step`: each step in its own implicit transaction, never inside an enclosing transaction block; a failed step leaves partial state the runner must detect and recover). Present exactly when `recommended` is. |
| `caveats` | array | with `recommended` | The typed conditions under which the recommendation differs from the original (see Caveats). Never empty when present — a rewrite with no trade would be the same statement. |
| `guidance` | string | when not constructible | The typed manual path (see Guidance). The submitted form still blocks; this names what to do about it. |

## Caveats (`caveats`)

The caveats are **independent — no caveat implies another**; a sequence carries every
caveat that applies to it. (`non-transactional` does not imply `separate-transactions`:
the first says a step refuses a transaction block, the second says the steps must not
share one.)

| Value | Meaning |
|---|---|
| `non-transactional` | The sequence contains a CONCURRENTLY statement, which cannot run inside a transaction block. |
| `separate-transactions` | The steps must commit separately — the weaker locks the sequence exists for are held to commit, so one enclosing transaction reproduces the blocking the rewrite avoids. |
| `invalid-index-on-failure` | A failed or cancelled concurrent build leaves an INVALID index that must be detected (`pg_index.indisvalid`) and dropped or rebuilt before retrying. |
| `detach-finalize-on-failure` | An interrupted concurrent detach leaves the partition half-detached; it must be finished with `DETACH PARTITION FINALIZE`. |
| `validation-scan` | The VALIDATE step still scans every row — the rewrite trades the lock strength, not the scan. |
| `scaffold-constraint-on-failure` | A failed VALIDATE leaves the NOT VALID constraint the sequence added on the live table, and replaying the sequence then fails at the ADD CONSTRAINT step (`duplicate_object`). The runner must detect the leftover constraint (`pg_constraint`) and resume from the VALIDATE step, or drop it and restart. |

## Guidance (`guidance`)

| Value | Emitted for | Manual path |
|---|---|---|
| `split-statement` | Any risky operation inside a multi-operation statement | Rewrites are constructed only for single-operation statements — a partial rewrite of a compound ALTER would be misleading. Split the statement into one operation per statement and advise again. |
| `add-column-then-constraint` | `ADD COLUMN` with an inline UNIQUE / PRIMARY KEY / FOREIGN KEY / CHECK | The inline constraint builds or validates under the ADD COLUMN's ACCESS EXCLUSIVE lock. Add the plain column first, then build the constraint with its online pattern. |
| `pre-add-validated-check` | `ATTACH PARTITION` | The attach scans the child under the parent's lock unless a validated CHECK matching the partition bound already exists on the child. Pre-add that CHECK (NOT VALID, then VALIDATE), attach, then drop it. The bound-matching CHECK cannot be constructed from the statement alone. |
| `not-null-scaffold` | `ADD CONSTRAINT … NOT NULL` | Prove the invariant with a NOT VALID CHECK (`col IS NOT NULL`) plus an online VALIDATE, then the NOT NULL constraint is a catalog flip — the same scaffold sequence the `SET NOT NULL` form gets constructed. |

## The rewrite table: operation → safer form → caveats

This is the specification of the advice — which safer native idiom each risky-as-written
operation gets, and what changes about how you must run it. It is useful even without
running pg-sprite: it is the "which safer idioms leave what behind when they fail" table
for PostgreSQL online DDL (background in
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md)).

| Operation as written | Recommended safer form | Caveats |
|---|---|---|
| `CREATE INDEX` / `DROP INDEX` / `REINDEX` (non-concurrent) | The `CONCURRENTLY` form of the same statement | `non-transactional`, `invalid-index-on-failure` |
| `ALTER TABLE … DETACH PARTITION` (non-concurrent) | `DETACH PARTITION … CONCURRENTLY` | `non-transactional`, `detach-finalize-on-failure` |
| `ALTER TABLE … ALTER COLUMN … SET NOT NULL` | `ADD CONSTRAINT … CHECK (col IS NOT NULL) NOT VALID` → `VALIDATE CONSTRAINT` → `SET NOT NULL` → `DROP CONSTRAINT` (scaffold) | `separate-transactions`, `validation-scan`, `scaffold-constraint-on-failure` |
| `ALTER TABLE … ADD PRIMARY KEY` / `ADD UNIQUE` | `CREATE UNIQUE INDEX CONCURRENTLY` → `ADD CONSTRAINT … USING INDEX` | `non-transactional`, `separate-transactions`, `invalid-index-on-failure` |
| `ALTER TABLE … ADD CHECK` / `ADD FOREIGN KEY` | `ADD CONSTRAINT … NOT VALID` → `VALIDATE CONSTRAINT` | `separate-transactions`, `validation-scan`, `scaffold-constraint-on-failure` |

An operation with a constructed rewrite that this table does not cover is a contract
violation: `pkg/suggest` fails closed rather than emitting caveat-less advice, and a test
walks every safer-idiom path the planner produces so the gap is caught in CI, not in a
consumer's report.

## Examples

A constructed rewrite (`pg-sprite suggest --json` over
`CREATE INDEX t_c_idx ON t (c)`):

```json
{
  "format_version": 1,
  "suggestions": [
    {
      "statement": 1,
      "line": 1,
      "column": 1,
      "original": "CREATE INDEX t_c_idx ON t (c)",
      "operation": "CREATE INDEX t_c_idx",
      "reason": "safer-idiom",
      "recommended": ["CREATE INDEX CONCURRENTLY t_c_idx ON t USING btree (c)"],
      "execution": "autocommit-each-step",
      "caveats": ["non-transactional", "invalid-index-on-failure"]
    }
  ]
}
```

A guidance suggestion (`ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int`):

```json
{
  "format_version": 1,
  "suggestions": [
    {
      "statement": 1,
      "line": 1,
      "column": 1,
      "original": "ALTER TABLE t ALTER COLUMN c SET NOT NULL, ADD COLUMN d int",
      "operation": "ALTER COLUMN c SET NOT NULL",
      "reason": "safer-idiom",
      "guidance": "split-statement"
    }
  ]
}
```

## Text output

Without `--json`, each suggestion renders as the statement header
(`statement N: operation — reason`) followed by either the safer sequence with its caveat
list or the guidance code with its manual path. The text form is for humans; automation
consumes the JSON report. A clean script prints nothing and always exits zero.
