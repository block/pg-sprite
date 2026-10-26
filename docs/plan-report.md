# The plan report contract

The plan report is the machine-readable dry-run plan both front doors emit — `migrate
--dry-run --json` (imperative) and `diff --json` (declarative). It is the one JSON shape an
operator or orchestrator consumes to decide whether and how a change would execute. This
document is the contract: the fields, the closed vocabularies, the identity rules, and the
behavior required of a consumer. The Go source of truth is `pkg/plan`; tests in `pkg/plan`,
`pkg/planner`, `pkg/router`, and `pkg/schemadiff` pin everything documented here, including
the examples at the end of this page.

`migrate --dry-run --json` emits a refusal verdict instead of a report when the apply would
refuse the statement before planning it, or refuse its routed steps from their shape alone:
an unsupported statement kind, an unnamed or `IF NOT EXISTS` index build, a `DETACH
PARTITION`. That verdict exits 2 and carries no `format_version`, `fingerprint`, or
`exec_sql`; a consumer branches on the exit code and on whether `format_version` is present
before reading the output as a report. ([CLI output examples](cli-output-examples.md) shows
both shapes.)

## Versioning: `format_version`

Every report carries `format_version`. A consumer that does not recognize the version must
**reject the report** — never guess at field semantics. The version covers more than the
field shape: the closed vocabularies below (sources, routes, backends, dispositions,
kinds, guidance, causes, classes, owners) and the fingerprint serialization are all pinned to it. Adding a vocabulary value or
changing the fingerprint definition is a contract change and bumps `format_version`, even if
no field is added or renamed.

The current version is **13**: version 13 added the `coordinated-constraint-rename` guidance code and classifies the rename of an index that backs a primary-key, unique, or exclusion constraint as `app-breaking-rename`; version 12 added the `expand-contract-column` and `coordinated-table-rename` guidance codes and made `app-breaking-rename` decisions yield the `rewrite-required` disposition; version 11 added the `cascading-drop` reason; version 10 added the `dependent-view` reason and the `dependent_views` decision field; version 9 added the `validate-constraint` and `validate-foreign-key` kinds; version 8 added the `replica-identity` reason; version 7 added the `set-generated-expression` kind; version 6 added the `add-foreign-key` and `drop-foreign-key`
kinds, the `foreign-key` cause, and the `declarative-foreign-key` reason; version 5 added `blocking_passthrough_eligible` to every refused
statement; version 4 added the `class` and `owner` fields on the report
and on refused statements, each drawn from a closed vocabulary (see Classes and Owners);
version 3 added the statement-level `cause` field on greenfield statements the create path
refuses by shape; version 2 added the statement-level `guidance` field on `rewrite-required`
statements. The fingerprint definition is unchanged from version 1.

The [lint report](lint-report.md) is a separate contract with its own `format_version`; the
two version independently. Lint findings embed this contract's Reasons vocabulary — the lint
`format_version` pins the reason set for lint consumers, as this one does for plan consumers.

## Consumer behavior for unknown values

Except for target-dependent refusal `reason` fields, every enum field in the report draws
from a closed vocabulary listed here. Refusal reasons are open and owned by `pkg/verdict`.
A consumer that meets any value it does not recognize must **treat the statement as unknown
and refuse it** — never ignore it and proceed. This is the same fail-closed posture the
engine itself takes with SQL it does not fully understand.

The typed codes are also the only strings safe to render verbatim into a shared surface
(a PR comment, a chat notification): they come from the closed sets below. `sql` is the
submitter's statement and `decisions[].operation` interpolates catalog identifiers, so a
consumer rendering either into a shared surface must clamp and escape them.

## Report fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `format_version` | int | always | Contract version; reject unknown versions. |
| `source` | string | always | Front door that derived the plan (see Sources). |
| `schema` | string | when resolved | Target schema. The **resolved** name the engine planned against — an unqualified alter reports the schema the session `search_path` resolves its table to, the one execution acts on (`public` when the `search_path` shows no such table), never an empty echo of the submitted text. Absent only when the statement has no single table target. |
| `table` | string | when targeted | Target table; absent for statements with no single table target (index maintenance). |
| `server_version` | string | when connected | The PostgreSQL `server_version` the plan was derived against. Classification is version-sensitive; a stored or forwarded report names the server whose rules produced it. |
| `table_exists` | bool | when introspected | Whether the live table was found. Set by both sources that introspect the target (diff, and the alter dry run); absent means the plan has no single table target to introspect. For diff, `false` means the plan is the full desired schema; for an alter dry run, `false` means the plan was classified from zero facts, executing it would fail, and the dry run exits with the refusal code. |
| `disposition` | string | always | Aggregate disposition across all statements (see Dispositions). |
| `reason` | string | target-dependent refusal only | Aggregate typed refusal cause when target facts override an otherwise executable route. |
| `class` | string | target-dependent refusal only | Aggregate refusal class (see Classes): how a consumer routes the refusal. Present exactly when `reason` is; a planner-level refusal carries its class on the refused statement instead. |
| `owner` | string | `no-online-safety-problem` refusals only | Who owns the aggregate work pg-sprite has no online-safety reason to run (see Owners). Present exactly when `class` is `no-online-safety-problem`. |
| `fingerprint` | string | always | The plan's stable identity (see Fingerprint). |
| `statements` | array | always | The ordered plan; `[]` (never `null`) means nothing to do. |

## Statement fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `sql` | string | always | The statement in the engine's **canonical rendering**: parsed and reprinted through the PostgreSQL deparser, whichever front door derived it. Never a verbatim echo of submitted text — the same change carries the same string through either door. Commented input is refused rather than silently stripped; optional noise words follow the grammar's canonical spelling. |
| `kind` | string | diff source only | Classifies a diff-derived statement (see Kinds) so a consumer can gate whole classes of change. Absent for the alter source: a submitted statement may carry several operations and has no single kind. |
| `destructive` | bool | always | Marks statements that discard live structure — a dropped column, constraint, index, or `NOT NULL`. Derived from the classifier's decisions, so both sources report it identically by construction. Always emitted, never omitted: a safety flag a consumer gates on must be explicit even when false. |
| `route` | string | always | The planner's aggregate route for the statement (see Routes). |
| `backend` | string | except refusals | The assigned execution strategy (see Backends); absent for refusals. |
| `disposition` | string | always | What execution would do with this statement now (see Dispositions). |
| `reason` | string | refusals only | Typed refusal cause for this statement: `unsupported-statement` for a planner-level refusal or, on a greenfield plan, a create shape the create path refuses (`cause` names which); `declarative-foreign-key` for a foreign-key change; `unsupported-partitioned-parent` when target facts refuse it. An unknown value must be treated as refused. |
| `cause` | string | greenfield create-shape refusals only | The create path's typed shape refusal (see Causes): why a table born in the run cannot carry this statement. Present exactly when the create path refused the statement — on a `diff`-source report with `table_exists: false`, that is every statement whose `disposition` is `refuse` and `reason` is `unsupported-statement`, plus a `CREATE TABLE` that declares a foreign key (`reason` `declarative-foreign-key`, `cause` `foreign-key`). Absent for every other refusal, including an `alter`-source refusal against a table that does not exist. Explanatory: excluded from the fingerprint. |
| `class` | string | refusals only | This statement's refusal class (see Classes): how a consumer routes it — wait for a capability, hand to an owner, use a safer idiom, fix the environment, or report a bug. Present exactly when `disposition` is `refuse`. Explanatory: excluded from the fingerprint. |
| `owner` | string | `no-online-safety-problem` refusals only | Who owns the work (see Owners). Present exactly when `class` is `no-online-safety-problem`. Explanatory: excluded from the fingerprint. |
| `blocking_passthrough_eligible` | bool | refusals only | Whether this typed refusal is in the closed accepted-blocking registry. Independent of flags and catalog lookups; explanatory and excluded from the fingerprint. |
| `decisions` | array | always | The planner's per-operation classifications (below). |
| `exec_sql` | array | native route | The ordered SQL the native backend would run — the safer sequence when the planner constructed one, or the statement as written for a table that does not exist yet (the greenfield create path runs plain builds; see Fingerprint). Absent for non-native routes: an `execute` statement whose `backend` is `copy-and-swap` runs no literal SQL a consumer could replay — the shadow-table change is one engine step — so a consumer that runs statements itself must hand such a statement back to pg-sprite rather than treat the absence as "nothing to run". |
| `execution` | string | with `exec_sql` | The typed execution contract for `exec_sql` (see Execution contracts). A consumer that runs the statements itself branches on this — it is what says the steps must not be wrapped in a transaction block. Present exactly when `exec_sql` is. |
| `guidance` | string | `rewrite-required` only | The typed manual path for a rewrite-required refusal, drawn from the [suggest report's Guidance vocabulary](suggest-report.md#guidance-guidance). The engine will not run the statement; this names what to do instead (`split-statement`, `add-column-then-constraint`, …). Present exactly when `disposition` is `rewrite-required`. Explanatory: excluded from the fingerprint. |

## Decision fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `operation` | string | always | Operator-facing label (`DROP COLUMN legacy_status`). Display only — never branch on it. |
| `destructive` | bool | always | Whether this operation discards live structure. Always emitted, never omitted. |
| `route` | string | always | Where the operation goes (see Routes). |
| `reason` | string | always | The typed cause of the routing decision (see Reasons). Automation branches on this, never on prose. |
| `unverified` | bool | when true | The planner failed closed to this route for lack of live facts — the route is what the engine would do, not a proven property of the change. With facts (a live introspection or a supplied column type) the same operation may classify as native. Absent means the decision is proven. |
| `safer_sql` | array | safer-idiom only | The ordered safer native sequence, when the planner could construct it. A safer form of the submitted operation, not a semantic equivalent: it converges on the same declared end state with different locking, transactionality, and failure modes. |
| `safer_sql_execution` | string | with `safer_sql` | The typed execution contract for `safer_sql` (see Execution contracts). Present exactly when `safer_sql` is. |
| `dependent_views` | array | dependent-view only | The views and materialized views that read the column, each schema-qualified with every identifier quoted, sorted. Present exactly when `reason` is `dependent-view`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |

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

### Planner decision reasons (`decisions[].reason`)

| Value | Meaning |
|---|---|
| `metadata-only` | A brief ACCESS EXCLUSIVE catalog change, no scan and no rewrite. Also the classification of every executable build on a table that does not exist yet (a `diff` greenfield plan): each create step commits in its own transaction under the brief `lock_timeout` / `statement_timeout` budget, so `CREATE TABLE` is visible before its indexes build and a concurrent writer that already knows the name makes the step fail fast rather than block. The classification is `metadata-only` because the cost is bounded by that budget on a table born in the run; a `safer-idiom` decision is reclassified here and `exec_sql` is the statement as written. |
| `online-idiom` | Already the safe native form (CONCURRENTLY, NOT VALID, VALIDATE, USING INDEX). |
| `fast-default` | ADD COLUMN whose default the catalog stores, so no row is rewritten: a constant, or an expression the server proved it stores, such as `now()`. |
| `binary-coercible` | A type change PostgreSQL relabels without a rewrite (widen varchar, varchar to text, widen numeric precision). |
| `safer-idiom` | Native, but the submitted form blocks; `safer_sql` carries the online rewrite when one can be constructed. Never appears on an executable statement of a greenfield plan — see `metadata-only`. |
| `app-breaking-rename` | A column or table rename, or (live plans only) the rename of an index that backs a primary-key, unique, or exclusion constraint, which renames the constraint — metadata-only for PostgreSQL, but running application code still referencing the old name breaks the instant it commits. The statement's disposition is `rewrite-required` with guidance `expand-contract-column`, `coordinated-table-rename`, or `coordinated-constraint-rename`; `migrate` runs a column or table rename as written only when `--force` names the table, and never forces an index rename. For a column the safe sequence is expand/contract: add the new column, dual-write and backfill, switch reads, then drop the old column as its own reviewed change. For a table, coordinate the rename with the application deploy that adopts the new name. The rename of a plain index stays `metadata-only`: ordinary SQL names tables and columns, not indexes. An offline plan (`lint`, `suggest`) has no catalog to tell which index backs a constraint, so it routes every index rename `metadata-only`; a live plan reads the catalog and classifies the constraint-backed case here. |
| `volatile-default` | ADD COLUMN whose default PostgreSQL computes for every row, rewriting the table. Proven by the server, or, with `unverified`, assumed for an expression default nothing proved. |
| `generated-stored` | Adding a stored generated column computes every row — a full rewrite. |
| `type-rewrite` | A type PostgreSQL must convert or check row by row: a type change it cannot relabel (rewrite plus reindex), a type change that moves the column's collation (every index on it rebuilt, no heap rewrite), or an added column of a domain with constraints. With `unverified`, assumed for an added column the server could not prove. |
| `relocation` | SET TABLESPACE moves the heap — a rewrite-scale copy. |
| `partition-parent-lock` | Creating a partition (`CREATE TABLE … PARTITION OF`): a brief `ACCESS EXCLUSIVE` on the parent, no scan. |
| `replica-identity` | The operation drops what a `REPLICA IDENTITY USING INDEX` of the table, or of a partition or inheritance child below it, is built on: the index, the constraint that owns it, or one of its columns, `INCLUDE` columns too. The setting outlives the index, so every `UPDATE` and `DELETE` on the table fails while a publication publishes them. Refused; move the identity first (`ALTER TABLE … REPLICA IDENTITY`), then re-plan. |
| `dependent-view` | The operation retypes or drops a column that a view or materialized view reads, on the table or, without `ONLY`, on a partition or inheritance child below it whose copy of the column the change reaches: every copy for a type change, and for a drop only the copies the server drops, which leaves out a child that declares the column itself or also inherits it from a table outside the drop. PostgreSQL refuses this while the view exists, even for a type change it would relabel. Refused; `dependent_views` names the views. Drop or redefine them first, then re-plan. `DROP COLUMN … CASCADE`, which would drop the views with the column, is refused as `cascading-drop`. |
| `cascading-drop` | A `DROP COLUMN`, `DROP CONSTRAINT`, or `DROP INDEX` with `CASCADE`, which also drops whatever depends on the target, such as a view that reads the column or another table's foreign key on the constraint. Those objects are outside the statement, so the plan cannot show them. Refused; drop each dependent object as its own change, then resubmit without `CASCADE`. |
| `unsupported-operation` | The planner does not recognize the operation or knows no safe path for it. |

### Target-dependent refusal reasons (`reason`, `statements[].reason`)

This vocabulary is owned by `pkg/verdict` and is open: new values may appear without changing
the meaning of existing values. A consumer that sees an unknown reason must fail closed and
treat the statement and report as refused.

| Value | Meaning |
|---|---|
| `unsupported-statement` | The planner knows no safe path for the statement (planner-level refusal), or — on a greenfield plan, where the table does not exist — the create path refuses the statement's shape, in which case `cause` names which shape (see Causes). The same token the run path's refusal verdict carries, so a dry-run report and a run receipt for the same statement match on the typed field alone. |
| `unsupported-partitioned-parent` | Target facts show that the statement cannot run safely on a partitioned parent. |
| `declarative-foreign-key` | The statement adds or drops a foreign key, or creates a table that declares one, and desired-state execution does not run foreign-key changes yet. The plan still shows the statement so the diff is reviewable; `migrate --alter` runs `ADD FOREIGN KEY` online. |

On the apply path, refusal checks have deterministic precedence: table size, then partition
support, then privileges.
On the greenfield create path, a decidable shape refusal takes precedence over the table-absence
and privilege checks because it needs no connection.

### Backends (`backend`)

| Value | Meaning |
|---|---|
| `native` | Direct PostgreSQL DDL (the safer sequence when one exists). |
| `copy-and-swap` | Shadow-table copy with checksum-gated cutover. |

### Execution contracts (`execution`, `safer_sql_execution`)

| Value | Meaning |
|---|---|
| `autocommit-each-step` | The steps run one at a time, in order, each in its own implicit transaction — never inside an enclosing transaction block. The CONCURRENTLY forms refuse an enclosing block outright, and a multi-step sequence inside one block holds every earlier step's locks across the steps designed to avoid them. A failed step leaves partial state the runner must detect and recover before retrying (a failed CONCURRENTLY build leaves an invalid index, `pg_index.indisvalid = false`). |

### Dispositions (`disposition`)

| Value | Meaning |
|---|---|
| `execute` | The engine would run it now — natively, or through the copy-and-swap backend when that is the statement's `backend`. The disposition is decided from the statement and the table's shape as planned; it does not run the copy-and-swap preflight (free disk, logical decoding, triggers, dependent views, publication membership), which execution reads before any statement of the plan runs and which can refuse the whole plan with `copy-and-swap-refused` and nothing executed. |
| `rewrite-required` | Native but not executable as submitted — it blocks the table, or it is a rename that breaks every running application instance still using the old name — and no safer sequence could be constructed; `guidance` names the manual path. `migrate` runs it as written only when `--force` names the table. |
| `unavailable` | Routed to copy-and-swap for an operation the engine does not carry yet; nothing executes. |
| `refuse` | The planner refused the statement; no backend is assigned. |

### Guidance (`guidance`, rewrite-required statements only)

The typed manual path for a statement the engine refuses to rewrite. The vocabulary is
owned by the [suggest report](suggest-report.md#guidance-guidance), which documents each
code's full manual path; this contract embeds it, so a new guidance code bumps this
`format_version` too.

| Value | Meaning |
|---|---|
| `split-statement` | Split the multi-operation statement into one operation per statement, then plan again. |
| `add-column-then-constraint` | Add the plain column first, then build the inline constraint as a separate, named ADD CONSTRAINT with its online pattern. |
| `pre-add-validated-check` | Pre-add a validated CHECK matching the partition bound before attaching. |
| `not-null-scaffold` | Prove the invariant with a NOT VALID CHECK plus online VALIDATE, then flip NOT NULL. |
| `name-constraint-then-validate` | Name the constraint, add it NOT VALID, then VALIDATE it online. |
| `unique-index-then-constraint` | Build the unique index with CREATE UNIQUE INDEX CONCURRENTLY, then attach it with ADD CONSTRAINT … USING INDEX. |
| `expand-contract-column` | Add the new column, dual-write and backfill it, switch reads, then drop the old column as its own change — instead of renaming the column under running application code. |
| `coordinated-table-rename` | Coordinate the table rename with the application deploy that adopts the new name, then run it with `--force` naming the table. |
| `coordinated-constraint-rename` | The renamed index backs a constraint, which the rename renames: coordinate the rename with the application deploy that stops naming the old constraint (`ON CONFLICT ON CONSTRAINT`, error handling). |

### Causes (`cause`, greenfield create-shape refusals only)

The create path's typed reason for refusing a statement by shape on a plan whose table does
not exist. The vocabulary is owned by `pkg/executor` (`executor.CreateShapeCauses()`) and
documented in full in the [execution model](execution-model.md#create-shape-causes); this
contract embeds it, so a new cause bumps this `format_version` too. A renderer prints the
cause's description instead of recomputing the shape check; `migrate.RunDesired`'s refusal
detail and the text diff both read this field.

| Value | Meaning |
|---|---|
| `partition-of` | Attaching a partition locks a parent the absence proof does not cover. |
| `inherits` | `INHERITS` binds to an existing parent the absence proof does not cover. |
| `like` | `LIKE` reads an existing source table the absence proof does not cover. |
| `of-type` | `OF` binds to an existing composite type the absence proof does not cover. |
| `foreign-key` | A foreign key locks each referenced table, and desired-state execution does not run foreign-key changes yet. |
| `if-not-exists` | A name-only no-op cannot prove the existing relation has the requested shape or is valid. |
| `concurrently` | A table born this run needs no concurrent index build. |
| `duplicate-name` | The desired set claims the same relation name twice. |
| `multiple-operations` | The statement and operation parse boundaries disagree about the operation count. |
| `unsupported-kind` | The statement is not a create kind the create path can run. |

`concurrently`, `multiple-operations`, and `unsupported-kind` re-verify preconditions
`statement.ParseDesired` enforces before a report exists — a desired file that passed
admission cannot produce them. They are published so the vocabulary is closed, not because a
consumer should branch on them; one appearing in a report means the desired schema bypassed
admission.

### Classes (`class`, `statements[].class`)

This vocabulary is owned by `pkg/verdict` and closed: a new class is a contract change that
bumps `format_version`. [docs/refusal-classes.md](refusal-classes.md) is the authoritative
statement of each class and its consumer action; this table is a copy of its rows.

| Value | Meaning |
|---|---|
| `capability-boundary` | The engine cannot do this yet, or has no implemented safe route. Wait for the capability or escalate. |
| `no-online-safety-problem` | There is nothing for an online schema-change engine to make safe; another tool class owns the work, or the operator runs it directly. Hand the statement to the named `owner`. |
| `by-design` | pg-sprite permanently refuses this form — PostgreSQL offers no online mechanism in any supported version, or a safer idiom exists and the verdict names it. Use the safer idiom; for a single-relation plain `DROP INDEX` (not `CASCADE`), `REINDEX INDEX`, or `REINDEX TABLE`, `migrate --accept-blocking SCHEMA.TABLE` runs the blocking form under bounded budgets and exits 3; otherwise run it outside pg-sprite in a maintenance window. |
| `environmental` | The change is supportable, but not here, now, or as this role: a size policy, exhausted budget, privilege, server version, stale plan, or catalog collision stopped it. Retry, provision, upgrade, re-plan, or escalate. |
| `invariant-violation` | pg-sprite refused because its own input or state is incoherent — a report this build cannot have produced. Report it with the verdict; do not retry, wait, or route elsewhere. |

### Owners (`owner`, `statements[].owner`)

This vocabulary is owned by `pkg/verdict` and closed: a new owner is a contract change that
bumps `format_version`. Present exactly when `class` is `no-online-safety-problem`.

| Value | Meaning |
|---|---|
| `data-change-runner` | Data changes and backfills owned by the application's data change runner (the tool that runs its versioned SQL or ORM changes). |
| `declarative-front-door` | Catalog bootstrap and convergence from a desired `CREATE TABLE`; `CREATE TABLE` keeps its pointer to `diff`. |
| `direct-operator` | Nobody else's tool: the statement has no online-safety problem, so whoever operates the table runs it directly, through whatever review the object warrants. |
| `provisioning` | Access control and replication provisioning — grants, roles, row-level-security policies, publications, subscriptions — owned by infrastructure-as-code. |

### Kinds (`kind`, diff source only)

| Value | Meaning |
|---|---|
| `create-table` | Creates the table (missing-table plans only). |
| `drop-index` | Drops an index. |
| `drop-constraint` | Drops a table constraint other than a foreign key. |
| `drop-foreign-key` | Drops a foreign-key constraint. Always refused for execution (`declarative-foreign-key`). |
| `drop-column` | Drops a column. |
| `add-column` | Adds a column. |
| `alter-type` | Changes a column's type. |
| `set-generated-expression` | Replaces a stored generated column's expression. Always refused for execution (`unsupported-statement`): it recomputes every row under an exclusive lock, and no online path exists. The statement needs PostgreSQL 17 or later. |
| `set-default` | Sets or replaces a column default. |
| `drop-default` | Drops a column default. |
| `set-not-null` | Adds the NOT NULL attribute. |
| `drop-not-null` | Removes the NOT NULL attribute. |
| `add-constraint` | Adds a table constraint other than a foreign key. |
| `add-foreign-key` | Adds a foreign-key constraint. Always refused for execution (`declarative-foreign-key`). |
| `validate-constraint` | Validates a live check constraint left `NOT VALID` whose definition otherwise matches the desired one, the state an interrupted online add leaves behind. Not destructive. |
| `validate-foreign-key` | Validates a live foreign key left `NOT VALID` whose definition otherwise matches the desired one. Always refused for execution (`declarative-foreign-key`). |
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
separator (`0x1E`). Explanatory fields (`decisions`, `kind`, `destructive`, `reason`, `cause`,
`guidance`) are excluded: a reworded reason does not change identity, but a rerouted,
resequenced, or rewritten plan does. An empty plan has a defined identity (the digest of no input).

For a table that does not exist yet, `exec_sql` is the plain canonical build that the
greenfield create path runs. It never substitutes `CONCURRENTLY` for an index on a table
born in that run, and the fingerprint therefore commits to the plain build.

Upgrade caveat: earlier `format_version` 2 reports disclosed the planner's online rewrite
(`CREATE INDEX CONCURRENTLY …`) as the greenfield `exec_sql` — a form the create path
refuses and never ran. Correcting the disclosure changed the greenfield fingerprint's
*value* without changing its *definition* (the serialization above is unchanged, so the
version is not bumped): the fingerprint now commits to what actually runs. A consumer
holding a greenfield fingerprint from an earlier report gets one
`plan-fingerprint-mismatch` on its next apply; re-plan and pin the new value. Fingerprints
for tables that already exist are unaffected.

This is a **plan identity, not a schema fingerprint**. The engine's schema-state comparisons
only ever compare server-decompiled output against server-decompiled output (see
`pkg/statement`); the plan fingerprint never participates in them.

## Examples

All three examples are generated by the real classify-and-route pipeline and pinned by a test
in `pkg/plan` — if the code drifts from this page, CI fails.

### `source: alter` — `migrate --dry-run --json`

`ALTER TABLE app.orders DROP COLUMN legacy_status` against a live table:

```json
{
  "format_version": 13,
  "source": "alter",
  "schema": "app",
  "table": "orders",
  "server_version": "16.4",
  "table_exists": true,
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
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

Note the canonical rendering: the deparser spells `DROP legacy_status` (the grammar treats
`COLUMN` as optional noise). `table_exists: true` records that the dry run introspected the
live table; had it been missing, the report would carry `false` and the dry run would exit
with the refusal code.

### `source: diff` — `diff --json`

A desired state that drops an index and adds a column with a constant default:

```json
{
  "format_version": 13,
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
          ],
          "safer_sql_execution": "autocommit-each-step"
        }
      ],
      "exec_sql": [
        "DROP INDEX CONCURRENTLY app.orders_legacy_idx"
      ],
      "execution": "autocommit-each-step"
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
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

Note `kind` on each statement (diff source only), the blocking `DROP INDEX` replaced by its
CONCURRENTLY form in `exec_sql`, and `table_exists: true`.

### `source: diff`, greenfield — `diff --json` with a create-shape refusal

A desired state for a table that does not exist yet, whose `CREATE TABLE` carries
`IF NOT EXISTS`:

```json
{
  "format_version": 13,
  "source": "diff",
  "schema": "app",
  "table": "gadgets",
  "server_version": "16.4",
  "table_exists": false,
  "disposition": "refuse",
  "reason": "unsupported-statement",
  "class": "by-design",
  "fingerprint": "sha256:342c51305b0be702f1fdcb879a7e43f81748639011b8e6782538dea1c5a2e8e6",
  "statements": [
    {
      "sql": "CREATE TABLE IF NOT EXISTS app.gadgets (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, name text NOT NULL)",
      "kind": "create-table",
      "destructive": false,
      "route": "native",
      "disposition": "refuse",
      "reason": "unsupported-statement",
      "class": "by-design",
      "blocking_passthrough_eligible": false,
      "cause": "if-not-exists",
      "decisions": [
        {
          "operation": "CREATE TABLE",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only"
        }
      ]
    },
    {
      "sql": "CREATE INDEX gadgets_name_idx ON app.gadgets USING btree (name)",
      "kind": "create-index",
      "destructive": false,
      "route": "native",
      "backend": "native",
      "disposition": "execute",
      "decisions": [
        {
          "operation": "CREATE INDEX gadgets_name_idx",
          "destructive": false,
          "route": "native",
          "reason": "metadata-only"
        }
      ],
      "exec_sql": [
        "CREATE INDEX gadgets_name_idx ON app.gadgets USING btree (name)"
      ],
      "execution": "autocommit-each-step"
    }
  ]
}
```

Note `table_exists: false`, the refused `CREATE TABLE` carrying `reason` and `cause` with its
`backend` and `exec_sql` withdrawn, and the admitted `CREATE INDEX` disclosing the plain build
the create path would run. The report-level `disposition` is `refuse`: one refused statement
refuses the plan, and nothing runs until the desired file drops `IF NOT EXISTS`.
