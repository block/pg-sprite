# The plan report contract

The plan report is the machine-readable dry-run plan both front doors emit — `migrate
--dry-run --json` (imperative) and `diff --json` (declarative). It is the one JSON shape an
operator or orchestrator consumes to decide whether and how a change would execute. This
document is the contract: the fields, the closed vocabularies, the identity rules, and the
behavior required of a consumer. The Go source of truth is `pkg/plan`; tests in `pkg/plan`,
`pkg/planner`, `pkg/router`, and `pkg/schemadiff` pin everything documented here, including
the examples at the end of this page.

## Versioning: `format_version`

Every report carries `format_version`. A consumer that does not recognize the version must
**reject the report** — never guess at field semantics. The version covers more than the
field shape: the closed vocabularies below (sources, routes, reasons, backends, dispositions,
kinds) and the fingerprint serialization are all pinned to it. Adding a vocabulary value or
changing the fingerprint definition is a contract change and bumps `format_version`, even if
no field is added or renamed.

The [lint report](lint-report.md) is a separate contract with its own `format_version`; the
two version independently. Lint findings embed this contract's Reasons vocabulary — the lint
`format_version` pins the reason set for lint consumers, as this one does for plan consumers.

## Consumer behavior for unknown values

Every enum field in the report draws from a closed vocabulary listed here. A consumer that
meets a value it does not recognize must **treat the statement as unknown and refuse it** —
never ignore it and proceed. This is the same fail-closed posture the engine itself takes
with SQL it does not fully understand.

## Report fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `format_version` | int | always | Contract version; reject unknown versions. |
| `source` | string | always | Front door that derived the plan (see Sources). |
| `schema` | string | when resolved | Target schema. The **resolved** name the engine planned against — an unqualified alter reports the schema the engine introspected (`public`), never an empty echo of the submitted text. Absent only when the statement has no single table target. |
| `table` | string | when targeted | Target table; absent for statements with no single table target (index maintenance). |
| `server_version` | string | when connected | The PostgreSQL `server_version` the plan was derived against. Classification is version-sensitive; a stored or forwarded report names the server whose rules produced it. |
| `table_exists` | bool | diff source only | Whether the live table was found. Absent means "not introspected" (alter source); `false` means the plan is the full desired schema. |
| `disposition` | string | always | Aggregate disposition across all statements (see Dispositions). |
| `fingerprint` | string | always | The plan's stable identity (see Fingerprint). |
| `statements` | array | always | The ordered plan; `[]` (never `null`) means nothing to do. |

## Statement fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `sql` | string | always | The statement in the engine's **canonical rendering**: parsed and reprinted through the PostgreSQL deparser, whichever front door derived it. Never a verbatim echo of submitted text — the same change carries the same string through either door. Commented input is refused rather than silently stripped; optional noise words follow the grammar's canonical spelling. |
| `kind` | string | diff source only | Classifies a diff-derived statement (see Kinds) so a consumer can gate whole classes of change. Absent for the alter source: a submitted statement may carry several operations and has no single kind. |
| `destructive` | bool | always | Marks statements that discard live structure — a dropped column, constraint, or index. Derived from the classifier's decisions, so both sources report it identically by construction. Always emitted, never omitted: a safety flag a consumer gates on must be explicit even when false. |
| `route` | string | always | The planner's aggregate route for the statement (see Routes). |
| `backend` | string | except refusals | The assigned execution strategy (see Backends); absent for refusals. |
| `disposition` | string | always | What execution would do with this statement now (see Dispositions). |
| `decisions` | array | always | The planner's per-operation classifications (below). |
| `exec_sql` | array | native route | The ordered SQL the native backend would run — the safer sequence when the planner constructed one. Absent for non-native routes. |

## Decision fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `operation` | string | always | Operator-facing label (`DROP COLUMN legacy_status`). Display only — never branch on it. |
| `destructive` | bool | always | Whether this operation discards live structure. Always emitted, never omitted. |
| `route` | string | always | Where the operation goes (see Routes). |
| `reason` | string | always | The typed cause of the routing decision (see Reasons). Automation branches on this, never on prose. |
| `unverified` | bool | when true | The planner failed closed to this route for lack of live facts — the route is what the engine would do, not a proven property of the change. With facts (a live introspection or a supplied column type) the same operation may classify as native. Absent means the decision is proven. |
| `safer_sql` | array | safer-idiom only | The ordered native sequence to run instead of the submitted form, when the planner could construct it. |

## Closed vocabularies

### Sources (`source`)

| Value | Meaning |
|---|---|
| `alter` | Derived from a submitted DDL statement (`migrate --dry-run`). |
| `diff` | Derived from a desired-state schema diff (`diff --desired`). |

### Routes (`route`)

| Value | Meaning |
|---|---|
| `native` | PostgreSQL runs it online natively — directly or via the safer idiom in `exec_sql`. |
| `copy-and-swap` | Needs a table rewrite; only the engine's shadow copy + cutover can do it online. |
| `refuse` | No known safe path; not executed. |

### Reasons (`reason`)

| Value | Meaning |
|---|---|
| `metadata-only` | A brief ACCESS EXCLUSIVE catalog change, no scan and no rewrite. |
| `online-idiom` | Already the safe native form (CONCURRENTLY, NOT VALID, VALIDATE, USING INDEX). |
| `fast-default` | ADD COLUMN with a constant default — the catalog stores the default, no rewrite. |
| `binary-coercible` | A type change PostgreSQL relabels without a rewrite (widen varchar, varchar to text, widen numeric precision). |
| `safer-idiom` | Native, but the submitted form blocks; `safer_sql` carries the online rewrite when one can be constructed. |
| `app-breaking-rename` | A column or table rename — metadata-only for PostgreSQL, but running application code still referencing the old name breaks the instant it commits. For a column the safe sequence is expand/contract: add the new column, dual-write and backfill, switch reads, then drop the old column as its own reviewed change. For a table, coordinate the rename with the application deploy that adopts the new name. Index renames stay `metadata-only` — SQL never references an index by name. |
| `volatile-default` | ADD COLUMN whose default the planner cannot prove constant — PostgreSQL rewrites the table. |
| `generated-stored` | Adding a stored generated column computes every row — a full rewrite. |
| `type-rewrite` | A type conversion PostgreSQL cannot relabel — rewrite plus reindex. |
| `relocation` | SET TABLESPACE moves the heap — a rewrite-scale copy. |
| `partition-parent-lock` | Partition attach/detach in its lock-taking form. |
| `unsupported-operation` | The planner does not recognize the operation or knows no safe path for it. |

### Backends (`backend`)

| Value | Meaning |
|---|---|
| `native` | Direct PostgreSQL DDL (the safer sequence when one exists). |
| `copy-and-swap` | Shadow-table copy with checksum-gated cutover. |

### Dispositions (`disposition`)

| Value | Meaning |
|---|---|
| `execute` | The engine would run it now. |
| `rewrite-required` | Native but blocking as submitted, and no safer sequence could be constructed — resubmit in the online form. |
| `unavailable` | Routed to a backend that is not yet implemented. |
| `refuse` | The planner refused the statement; no backend is assigned. |

### Kinds (`kind`, diff source only)

| Value | Meaning |
|---|---|
| `create-table` | Creates the table (missing-table plans only). |
| `drop-index` | Drops an index. |
| `drop-constraint` | Drops a table constraint. |
| `drop-column` | Drops a column. |
| `add-column` | Adds a column. |
| `alter-type` | Changes a column's type. |
| `set-default` | Sets or replaces a column default. |
| `drop-default` | Drops a column default. |
| `set-not-null` | Adds the NOT NULL attribute. |
| `drop-not-null` | Removes the NOT NULL attribute. |
| `add-constraint` | Adds a table constraint. |
| `create-index` | Creates an index. |

## Fingerprint

`fingerprint` is the plan's stable identity: `sha256:` plus the hex digest over what would
execute. It exists for one consumer protocol: an approver pins it when the plan is
reviewed, and an executor recomputes it at apply time and refuses on mismatch — that is how
"the plan a reviewer approves is the plan that executes" survives storage and forwarding.
The engine computes and reports the fingerprint on every plan; enforcing the pin at apply
time is the consumer's side of the contract.

The serialization is exact and pinned by test: for each statement in plan order, hash the
canonical `sql`, `route`, `backend`, and `disposition`, then each `exec_sql` entry — every
field followed by a unit separator (`0x1F`) — and close each statement with a record
separator (`0x1E`). Explanatory fields (`decisions`, `kind`, `destructive`) are excluded: a
reworded reason does not change identity, but a rerouted, resequenced, or rewritten plan
does. An empty plan has a defined identity (the digest of no input).

This is a **plan identity, not a schema fingerprint**. The engine's schema-state comparisons
only ever compare server-decompiled output against server-decompiled output (see
`pkg/statement`); the plan fingerprint never participates in them.

## Examples

Both examples are generated by the real classify-and-route pipeline and pinned by a test in
`pkg/plan` — if the code drifts from this page, CI fails.

### `source: alter` — `migrate --dry-run --json`

`ALTER TABLE app.orders DROP COLUMN legacy_status` against a live table:

```json
{
  "format_version": 1,
  "source": "alter",
  "schema": "app",
  "table": "orders",
  "server_version": "16.4",
  "disposition": "execute",
  "fingerprint": "sha256:acca39fb0630089005cb0ce6519406b1c1cfa8e122aeef044f5502a6b16accbc",
  "statements": [
    {
      "sql": "ALTER TABLE app.orders DROP legacy_status",
      "destructive": true,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "DROP COLUMN legacy_status",
          "destructive": true,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "ALTER TABLE app.orders DROP legacy_status"
      ]
    }
  ]
}
```

Note the canonical rendering: the deparser spells `DROP legacy_status` (the grammar treats
`COLUMN` as optional noise), and there is no `table_exists` — the alter source does not
introspect for existence.

### `source: diff` — `diff --json`

A desired state that drops an index and adds a column with a constant default:

```json
{
  "format_version": 1,
  "source": "diff",
  "schema": "app",
  "table": "orders",
  "server_version": "16.4",
  "table_exists": true,
  "disposition": "execute",
  "fingerprint": "sha256:cb7ec645948ff1d239ba4ce3f0e051e4e2bcebec799fdd6f8401399d4a246f53",
  "statements": [
    {
      "sql": "DROP INDEX app.orders_legacy_idx",
      "kind": "drop-index",
      "destructive": true,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "DROP INDEX app.orders_legacy_idx",
          "destructive": true,
          "route": "native",
          "reason": "safer-idiom",
          "safer_sql": [
            "DROP INDEX CONCURRENTLY app.orders_legacy_idx"
          ]
        }
      ],
      "exec_sql": [
        "DROP INDEX CONCURRENTLY app.orders_legacy_idx"
      ]
    },
    {
      "sql": "ALTER TABLE app.orders ADD COLUMN region text DEFAULT 'emea'",
      "kind": "add-column",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "ADD COLUMN region",
          "destructive": false,
          "route": "native",
          "reason": "fast-default"
        }
      ],
      "exec_sql": [
        "ALTER TABLE app.orders ADD COLUMN region text DEFAULT 'emea'"
      ]
    }
  ]
}
```

Note `kind` on each statement (diff source only), the blocking `DROP INDEX` replaced by its
CONCURRENTLY form in `exec_sql`, and `table_exists: true`.
