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
PARTITION` from a table with a `DEFAULT` partition, a statement routed to copy-and-swap without
`--copy-and-swap`, which keeps the preview route closed (cause `copy-and-swap-not-enabled`), or
one routed there, with the route opened, on a partitioned parent, a partition, or a member of an
inheritance tree (cause
`copy-and-swap-partitioned`, or `copy-and-swap-unlogged` or `copy-and-swap-force-rls` when the
table is also UNLOGGED or forces row security). `diff --json` and `migrate --desired --dry-run --json` do the same when the desired
file itself is refused before a plan exists: a statement the file may not hold (a type, a data
change), a second table, partitions of a table, a table shape the planner cannot build, or a
difference the diff does not converge (storage parameters, persistence, a collation, an identity,
a generated column, a serial column). That verdict exits 2 and carries no `format_version`, `fingerprint`, or `exec_sql`; a consumer
branches on the exit code and on whether `format_version` is present before reading the output
as a report. ([CLI output examples](cli-output-examples.md) shows
both shapes.) `migrate --desired --dry-run --json` also ends, exit 2, in the run's whole-plan
refusal result instead of a report when a planned statement routes to copy-and-swap without
`--copy-and-swap` (cause `copy-and-swap-not-enabled`), or, with the route opened, on a
partitioned parent, a partition, or a member of an inheritance tree (cause
`copy-and-swap-partitioned`); that result embeds this report under `plan`, while `diff --json`
prints the report with the statement's `execute` disposition.

## Versioning: `format_version`

Every report carries `format_version`. A consumer that does not recognize the version must
**reject the report** — never guess at field semantics. The version covers more than the
field shape: the closed vocabularies below (sources, routes, backends, dispositions,
kinds, guidance, causes, classes, owners, advisory kinds) and the fingerprint serialization are all pinned to it. Adding a vocabulary value or
changing the fingerprint definition is a contract change and bumps `format_version`, even if
no field is added or renamed.

The current version is **32**: version 32 added the `dependent-publication` reason and the `dependent_publications` decision field; version 31 also carries `replaced_by` on the drop of the old object when a unique constraint and a unique index of the same name swap kinds, a `drop-constraint` whose replacement is a standalone index or a `drop-index` whose replacement a constraint takes over; version 30 runs the `drop-foreign-key` kind instead of refusing it, and narrows the `declarative-foreign-key` reason to the drop of a foreign key a desired file cannot declare, or changes under the same name on a partitioned table or in a plan that also drops or retypes a column; version 29 added the `rename-index` kind and the statement-level `replaced_by` field, which a changed index the diff replaces carries on the drop of the old index, and which the fingerprint does not cover, and a `rename-index` decision carries `proven_catalog_only: true`; version 28 added the `partitioned-foreign-key-build` execution contract, which an `ALTER TABLE ... ADD FOREIGN KEY` on a partitioned table carries in a `migrate --dry-run` or `diff` plan on PostgreSQL 14–17 in place of its refusal and which the fingerprint covers; version 27 added the `partitioned-key-build` execution contract, which an `ALTER TABLE ... ADD PRIMARY KEY` or `ADD UNIQUE` on a partitioned table carries in a `migrate --dry-run` plan, and a key a desired file adds carries in a `diff` plan, in place of its refusal and which the fingerprint covers; version 26 runs the `add-foreign-key` and `validate-foreign-key` kinds instead of refusing them, so `declarative-foreign-key` now refuses only `drop-foreign-key`, and retires the `foreign-key` cause, since the create path creates a table with its foreign keys; version 25 added the `proven_catalog_only` decision field, which marks an operation PostgreSQL runs on the catalog alone and which the fingerprint does not cover; version 24 added the `blocking-index-drop` execution contract, which a refused `drop-index` of a partitioned table's index carries with its plain `DROP INDEX` as `exec_sql`, and which the fingerprint covers; version 23 added the `partitioned-index-build` execution contract, which a `CREATE INDEX` on a partitioned table carries in place of its refusal and which the fingerprint covers; version 22 added the statement-level `drops` field, with its `dependents`, which the fingerprint covers when it is present, and the report-level `pinnable_fingerprint` field; version 21 added the `add-integer-primary-key` guidance code (carried by a `copy-and-swap-pk-unsupported` refusal verdict for a table with no primary key, never by a report statement; no field changed, so a consumer that pins the version only moves its pin); version 20 added the `add-enum-value` kind and the report-level `type` field, which a plan for a desired enum type file carries in place of `table`; version 19 added the report-level `extension_schemas` field, which the fingerprint covers when it is present; version 18 added the `dependent-trigger`, `dependent-generated-column`, and `partition-key` reasons and the `dependent_triggers`, `dependent_generated_columns`, and `partition_key_tables` decision fields, and made each `dependent_*` field present whenever its dependents block the change, whatever the decision's reason; version 17 added the `not-null-without-default` reason; version 16 added the `dependent-policy` reason and the `dependent_policies` decision field; version 15 added the report-level `advisories` field and its
closed kind vocabulary — the invalid indexes a `diff` observed and declined to drop; version 14 added the `alter-column-type-in-place` guidance code (carried by a `destructive-change` refusal verdict, never by a report statement); version 13 added the `coordinated-constraint-rename` guidance code and classifies the rename of an index that backs a primary-key, unique, or exclusion constraint as `app-breaking-rename`; version 12 added the `expand-contract-column` and `coordinated-table-rename` guidance codes and made `app-breaking-rename` decisions yield the `rewrite-required` disposition; version 11 added the `cascading-drop` reason; version 10 added the `dependent-view` reason and the `dependent_views` decision field; version 9 added the `validate-constraint` and `validate-foreign-key` kinds; version 8 added the `replica-identity` reason; version 7 added the `set-generated-expression` kind; version 6 added the `add-foreign-key` and `drop-foreign-key`
kinds, the `foreign-key` cause, and the `declarative-foreign-key` reason; version 5 added `blocking_passthrough_eligible` to every refused
statement; version 4 added the `class` and `owner` fields on the report
and on refused statements, each drawn from a closed vocabulary (see Classes and Owners);
version 3 added the statement-level `cause` field on greenfield statements the create path
refuses by shape; version 2 added the statement-level `guidance` field on `rewrite-required`
statements. The fingerprint definition is unchanged from version 1 for a plan without `extension_schemas` and without a statement that carries `drops`.

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
| `table` | string | when targeted | Target table; absent for statements with no single table target (index maintenance) and for a desired enum type file. |
| `type` | string | `diff` source, enum type file | The enum type a desired enum type file declares, unqualified; `schema` names where it lives. Present exactly when the plan is for an enum type, and then `table` is absent. Not covered by the fingerprint. |
| `server_version` | string | when connected | The PostgreSQL `server_version` the plan was derived against. Classification is version-sensitive; a stored or forwarded report names the server whose rules produced it. |
| `table_exists` | bool | when introspected | Whether the live table was found. Set by both sources that introspect the target (diff, and the alter dry run); absent means the plan has no single table target to introspect. For diff, `false` means the plan is the full desired schema; for an alter dry run, `false` means the plan was classified from zero facts, executing it would fail, and the dry run exits with the refusal code. |
| `disposition` | string | always | Aggregate disposition across all statements (see Dispositions). |
| `reason` | string | target-dependent refusal only | Aggregate typed refusal cause when target facts override an otherwise executable route. |
| `class` | string | target-dependent refusal only | Aggregate refusal class (see Classes): how a consumer routes the refusal. Present exactly when `reason` is; a planner-level refusal carries its class on the refused statement instead. |
| `owner` | string | `no-online-safety-problem` refusals only | Who owns the aggregate work pg-sprite has no online-safety reason to run (see Owners). Present exactly when `class` is `no-online-safety-problem`. |
| `fingerprint` | string | always | The plan's stable identity (see Fingerprint). |
| `pinnable_fingerprint` | string | desired-state preview refused for being destructive | The fingerprint of the plan before the destructive statements were refused: the value `--expect-fingerprint` pins and a run with `--allow-destructive` accepts. `fingerprint` on that report covers the refused plan and cannot be pinned. Excluded from the fingerprint. |
| `extension_schemas` | array | `diff` source, when the desired file named extension schemas | The schemas the desired file's unqualified names also resolved against, in search order (`--extension-schema` or `WithExtensionSchemas`). A table that does not exist yet is created from the file's own statements under them. Covered by the fingerprint (see Fingerprint). |
| `statements` | array | always | The ordered plan; `[]` (never `null`) means nothing to do. |
| `advisories` | array | `diff` source, existing table, when non-empty | Typed observations the diff made about the live table that produced no statement (see Advisory fields): the invalid indexes it saw and declined to drop. Advisory only — the statements and the `destructive` gate are unaffected, and nothing in it executes. Read it alongside `statements`: an empty plan with an advisory present means there is nothing to run **and** the table is not clean. Absent when there is nothing to report. Also absent for a desired file that declares row security: that review path collects no advisories, so there an absent field does not mean the table has no invalid index. Explanatory: excluded from the fingerprint. |

## Advisory fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `kind` | string | always | What the diff observed and what completes the plan (see Advisory kinds). Automation branches on this, never on prose. |
| `index` | string | always | The live index the observation is about. A catalog identifier — clamp and escape it before rendering into a shared surface. |

## Statement fields

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `sql` | string | always | The statement in the engine's **canonical rendering**: parsed and reprinted through the PostgreSQL deparser, whichever front door derived it. Never a verbatim echo of submitted text — the same change carries the same string through either door. Commented input is refused rather than silently stripped; optional noise words follow the grammar's canonical spelling. |
| `kind` | string | diff source only | Classifies a diff-derived statement (see Kinds) so a consumer can gate whole classes of change. Absent for the alter source: a submitted statement may carry several operations and has no single kind. |
| `destructive` | bool | always | Marks statements that discard live structure — a dropped column, constraint, index, or `NOT NULL`. Derived from the classifier's decisions, so both sources report it identically by construction. Always emitted, never omitted: a safety flag a consumer gates on must be explicit even when false. |
| `route` | string | always | The planner's aggregate route for the statement (see Routes). |
| `backend` | string | except refusals | The assigned execution strategy (see Backends); absent for refusals. |
| `disposition` | string | always | What execution would do with this statement now (see Dispositions). |
| `reason` | string | refusals only | Typed refusal cause for this statement: `unsupported-statement` for a planner-level refusal or, on a greenfield plan, a create shape the create path refuses (`cause` names which); `declarative-foreign-key` for the drop of a foreign key a desired file cannot declare, or changes under the same name on a partitioned table or in a plan that also retypes a column or drops one beside an added column that may rewrite the table; `unsupported-partitioned-parent` when target facts refuse it. An unknown value must be treated as refused. |
| `cause` | string | greenfield create-shape refusals only | The create path's typed shape refusal (see Causes): why a table born in the run cannot carry this statement. Present exactly when the create path refused the statement — on a `diff`-source report with `table_exists: false`, that is every statement whose `disposition` is `refuse` and `reason` is `unsupported-statement`. Absent for every other refusal, including an `alter`-source refusal against a table that does not exist. Explanatory: excluded from the fingerprint. |
| `class` | string | refusals only | This statement's refusal class (see Classes): how a consumer routes it — wait for a capability, hand to an owner, use a safer idiom, fix the environment, or report a bug. Present exactly when `disposition` is `refuse`. Explanatory: excluded from the fingerprint. |
| `owner` | string | `no-online-safety-problem` refusals only | Who owns the work (see Owners). Present exactly when `class` is `no-online-safety-problem`. Explanatory: excluded from the fingerprint. |
| `blocking_passthrough_eligible` | bool | refusals only | Whether this typed refusal is in the closed accepted-blocking registry. Independent of flags and catalog lookups; explanatory and excluded from the fingerprint. |
| `decisions` | array | always | The planner's per-operation classifications (below). |
| `drops` | object | `diff` source, statements that drop an index, constraint, column, or `NOT NULL` | The live object the statement removes, read when the plan was made: `kind` (`index`, `constraint`, `column`, or `not-null`), `name`, and `definition` (an index's or constraint's catalog definition; a column's attribute number, type, collation, generation, default, and nullability; for `not-null`, the attribute number and `NOT NULL`), and for a column drop `dependents`: the objects the drop also removes, each as `kind name` with its definition when it has one (`index`, `constraint`, `owned sequence` and `identity sequence` with the sequence's options, or `other`). The fingerprint covers all of it, so a pin binds what the drop removes and not only its name, and a run re-reads it under the statement's own lock and refuses with `dropped-object-drift`, or `dropped-index-drift` for an index, dropping nothing, if the live definition differs or the column has a dependent the plan did not list. A dependent the plan listed that an earlier statement of the plan or another session has since removed does not refuse. A `NOT NULL` drop is planned with the drops, after the column drops and before any statement that alters or adds, so a table rewrite later in the plan cannot renumber the column it names. When the plan replaces the table's primary key in place, the `NOT NULL` drop comes after the swap instead, since the old key owns its columns' `NOT NULL` until the swap drops it. |
| `replaced_by` | string | `diff` source, a `drop-index` of a changed index, or the `drop-index` or `drop-constraint` of a same-name kind swap | The index an earlier statement of the plan built under the desired definition, under its temporary name. The drop reads it again on the session that drops the old object, and refuses with `replacement-index-drift`, dropping nothing, when it is gone, invalid, or no longer has the definition the plan built it with. The `rename-index` after the drop gives it the dropped object's name, so the table has an index with the desired definition before the old one goes; when a unique index becomes a unique constraint of the same name, the `ADD CONSTRAINT ... USING INDEX` after the drop takes it over instead, and when a unique constraint becomes a unique index, the drop is a `drop-constraint`. A unique index whose rule is unchanged or wider keeps rejecting duplicates throughout. The drop still removes whatever the desired definition no longer asks for, such as uniqueness, or the rows a narrower predicate leaves out, so it stays destructive. The fingerprint covers the statements that build and rename the replacement, not this field. The definition the drop checks travels in the statement's `Replacement`, which the JSON leaves out, so a plan rebuilt from this report has no replacement to check, and a drop that names `replaced_by` without one is refused as an invariant violation rather than run unchecked. |
| `exec_sql` | array | native route; a refused `drop-index` of a partitioned table's index | The ordered SQL the native backend would run — the safer sequence when the planner constructed one, or the statement as written for a table that does not exist yet (the greenfield create path runs plain builds; see Fingerprint). A refused statement carries it only as the plain `DROP INDEX` of a partitioned table's index, with `execution` `blocking-index-drop`: the one form a run that accepts the blocking lock executes. Absent for non-native routes: an `execute` statement whose `backend` is `copy-and-swap` runs no literal SQL a consumer could replay — the shadow-table change is one engine step — so a consumer that runs statements itself must hand such a statement back to pg-sprite rather than treat the absence as "nothing to run". |
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
| `proven_catalog_only` | bool | when true | PostgreSQL runs this operation on the catalog alone: it never rewrites or reads the table's rows, so it costs the same on a table of any size. Set for `DROP COLUMN`, `DROP DEFAULT`, `DROP NOT NULL`, and `DROP CONSTRAINT`, and for an `ADD COLUMN` the server's probe proved adds without a rewrite or a scan. Absent means not established, not that the operation reads rows. A statement whose decisions all carry it is exempt from the [size guard](optimistic-attempt.md#who-gets-size-checked). |
| `safer_sql` | array | safer-idiom only | The ordered safer native sequence, when the planner could construct it. A safer form of the submitted operation, not a semantic equivalent: it converges on the same declared end state with different locking, transactionality, and failure modes. |
| `safer_sql_execution` | string | with `safer_sql` | The typed execution contract for `safer_sql` (see Execution contracts). Present exactly when `safer_sql` is. |
| `dependent_views` | array | when a view blocks the change | The views and materialized views that read the column, each schema-qualified with every identifier quoted, sorted. Present whenever one blocks the change, which makes `reason` `dependent-view`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |
| `dependent_policies` | array | when a policy blocks the change | The row-level security policies that read the column, each rendered `"policy" ON "schema"."table"` with every identifier quoted, sorted by schema, table, then policy. Present whenever one blocks the change, whatever the decision's `reason`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |
| `dependent_triggers` | array | when a trigger blocks the change | The triggers whose `UPDATE OF` list or `WHEN` condition names the column, each rendered `"trigger" ON "schema"."table"` with every identifier quoted, sorted by schema, table, then trigger. A partition's clone of its parent's trigger is named only as the parent's. Present whenever one blocks the change, whatever the decision's `reason`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |
| `dependent_publications` | array | when a publication blocks the change | The publications whose column list or row filter for the table names the column, each rendered `"publication"` with its identifier quoted, sorted. A publication that publishes the table without a column list or row filter is not named. Present whenever one blocks the change, whatever the decision's `reason`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |
| `dependent_generated_columns` | array | when a generated column blocks the change | The generated columns whose expression reads the column, each rendered `"schema"."table"."column"` with every identifier quoted, sorted. A copy that dropping the table's own generated column removes is left out. Present whenever one blocks the change, whatever the decision's `reason`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |
| `partition_key_tables` | array | partition-key only | The partitioned tables whose partition key includes the column, each schema-qualified with every identifier quoted, sorted. Present exactly when `reason` is `partition-key`. Catalog identifiers: a consumer rendering them into a shared surface must clamp and escape them. |

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
| `binary-coercible` | A type change PostgreSQL relabels without a rewrite (widen varchar, varchar to text, widen numeric precision, raise or remove a timestamp, timestamptz, time, or timetz precision, widen varbit, cidr to inet, timestamp to or from timestamptz in a UTC session for a column no index or foreign key reads). |
| `safer-idiom` | Native, but the submitted form blocks; `safer_sql` carries the online rewrite when one can be constructed. Never appears on an executable statement of a greenfield plan — see `metadata-only`. |
| `app-breaking-rename` | A column or table rename, or (live plans only) the rename of an index that backs a primary-key, unique, or exclusion constraint, which renames the constraint — metadata-only for PostgreSQL, but running application code still referencing the old name breaks the instant it commits. The statement's disposition is `rewrite-required` with guidance `expand-contract-column`, `coordinated-table-rename`, or `coordinated-constraint-rename`; `migrate` runs a column or table rename as written only when `--force` names the table, and never forces an index rename. For a column the safe sequence is expand/contract: add the new column, dual-write and backfill, switch reads, then drop the old column as its own reviewed change. For a table, coordinate the rename with the application deploy that adopts the new name. The rename of a plain index stays `metadata-only`: ordinary SQL names tables and columns, not indexes. An offline plan (`lint`, `suggest`) has no catalog to tell which index backs a constraint, so it routes every index rename `metadata-only`; a live plan reads the catalog and classifies the constraint-backed case here. |
| `volatile-default` | ADD COLUMN whose default PostgreSQL computes for every row, rewriting the table. Proven by the server, or, with `unverified`, assumed for an expression default nothing proved. |
| `generated-stored` | Adding a stored generated column computes every row — a full rewrite. |
| `type-rewrite` | A type PostgreSQL must convert or check row by row: a type change it cannot relabel (rewrite plus reindex), a type change that moves the column's collation (every index on it rebuilt, no heap rewrite), or an added column of a domain with constraints. With `unverified`, assumed for an added column the server could not prove. |
| `relocation` | SET TABLESPACE moves the heap — a rewrite-scale copy. A partitioned parent has no heap to move, so a live plan classifies SET TABLESPACE on one as `metadata-only`; an offline plan (`lint`, `suggest`) has no catalog to tell, and keeps `relocation` as `unverified`. |
| `partition-parent-lock` | Creating a partition (`CREATE TABLE … PARTITION OF`): a brief `ACCESS EXCLUSIVE` on the parent, no scan. |
| `replica-identity` | The operation drops what a replica identity index of the table, or of a partition or inheritance child below it, is built on: the index, the constraint that owns it, or one of its columns, `INCLUDE` columns too. The index is the one `REPLICA IDENTITY USING INDEX` selects, or the primary key under `REPLICA IDENTITY DEFAULT` while a publication publishes the table's updates or deletes. The setting outlives the index, so every `UPDATE` and `DELETE` on the table fails while a publication publishes them. Refused; move the identity first (`ALTER TABLE … REPLICA IDENTITY`), then re-plan. |
| `dependent-view` | The operation retypes or drops a column that a view or materialized view reads, on the table or, without `ONLY`, on a partition or inheritance child below it whose copy of the column the change reaches: every copy for a type change, and for a drop only the copies the server drops, which leaves out a child that declares the column itself or also inherits it from a table outside the drop. PostgreSQL refuses this while the view exists, even for a type change it would relabel. Refused; `dependent_views` names the views. The text output's help names a materialized view by its kind and gives the manual path, chosen as for `dependent-policy`. `DROP COLUMN … CASCADE`, which would drop the views with the column, is refused as `cascading-drop`. |
| `dependent-policy` | The operation retypes or drops a column that a row-level security policy reads in its `USING` or `WITH CHECK` expression, wherever the policy is defined: on the table, on a partition or inheritance child the change reaches (judged as for `dependent-view`), or on another table whose policy reads the column through a subquery. PostgreSQL refuses this while the policy exists, even for a type change it would relabel. Refused; `dependent_policies` names the policies. The text output's help gives the manual path: rewrite the policies before a drop, recreate them around an in-place type change in one transaction, or move them to a new column for a type change that rewrites the table. A column that a view also reads is refused as `dependent-view` first, with `dependent_policies` still set. |
| `dependent-trigger` | The operation retypes or drops a column that a trigger's `UPDATE OF` list or `WHEN` condition names, on a table the change reaches (judged as for `dependent-view`). PostgreSQL refuses this while the trigger exists, even for a type change it would relabel; a trigger whose function reads the column does not count. Refused; `dependent_triggers` names the triggers. The text output's help gives the manual path, chosen as for `dependent-policy`. A column that a view or a policy also reads is refused for those first, with `dependent_triggers` still set. |
| `dependent-publication` | The operation retypes or drops a column that a publication's column list or row filter for the table names, on a table the change reaches (judged as for `dependent-view`). PostgreSQL refuses this while the publication names the column. Refused; `dependent_publications` names the publications. The text output's help gives the manual path: remove the column from the publications with `ALTER PUBLICATION ... SET TABLE` first. A column that a view, a policy, or a trigger also depends on is refused for those first, with `dependent_publications` still set. |
| `dependent-generated-column` | The operation retypes or drops a column that a generated column's expression reads, on a table the change reaches. PostgreSQL refuses this while the generated column exists, even for a type change it would relabel, except that PostgreSQL 14 drops the generated column along with a dropped column. Refused; `dependent_generated_columns` names the generated columns. A column that a view, a policy, or a trigger also depends on is refused for those first, with `dependent_generated_columns` still set. |
| `partition-key` | The operation retypes or drops a column in the partition key of the table or of a partitioned table below it that the change reaches (judged as for `dependent-view`). PostgreSQL never does either. Refused; `partition_key_tables` names the partitioned tables. Judged before every other dependent, since no object can be moved aside to let the change run. |
| `not-null-without-default` | The operation adds a `NOT NULL` column that nothing fills (no default, identity, or generation expression of its own, and no `DEFAULT` on its domain) to a table that holds rows. The column is `NOT NULL` whether the statement spells it or it comes from the column's domain or an inline `PRIMARY KEY`. The rows counted are the table's and those of its partitions and inheritance children that store rows locally; a foreign table's rows are not read, since PostgreSQL does not check them. Every existing row would hold `NULL` in the new column, so PostgreSQL refuses the statement (SQLSTATE `23502`) after taking the table's exclusive lock. Refused. The text output's help gives the manual path: a constant default, which PostgreSQL stores without a rewrite, or adding the column without `NOT NULL`, backfilling it, and then setting `NOT NULL`. An empty table, or a column the table already has and the statement does not drop first, is not refused. |
| `cascading-drop` | A `DROP COLUMN`, `DROP CONSTRAINT`, or `DROP INDEX` with `CASCADE`, which also drops whatever depends on the target, such as a view that reads the column or another table's foreign key on the constraint. Those objects are outside the statement, so the plan cannot show them. Refused; drop each dependent object as its own change, then resubmit without `CASCADE`. |
| `unsupported-operation` | The planner does not recognize the operation or knows no safe path for it. |

### Target-dependent refusal reasons (`reason`, `statements[].reason`)

This vocabulary is owned by `pkg/verdict` and is open: new values may appear without changing
the meaning of existing values. A consumer that sees an unknown reason must fail closed and
treat the statement and report as refused.

| Value | Meaning |
|---|---|
| `unsupported-statement` | The planner knows no safe path for the statement (planner-level refusal), or — on a greenfield plan, where the table does not exist — the create path refuses the statement's shape, in which case `cause` names which shape (see Causes). The same token the run path's refusal verdict carries, so a dry-run report and a run receipt for the same statement match on the typed field alone. |
| `unsupported-partitioned-parent` | Target facts show that the statement cannot run safely on a partitioned parent. The report carries no partition cause: a `drop-index` statement with this reason is a drop PostgreSQL cannot run concurrently, whose plain form a `diff`-source statement carries as `exec_sql` with `execution` `blocking-index-drop`, and which `migrate --desired` runs from the pinned plan, and `migrate --alter` as submitted, under `--accept-blocking` and `--allow-destructive` (`blocking_passthrough_eligible` is true), and any other `kind` is an index build or a constraint the parent cannot take online. |
| `declarative-foreign-key` | The statement drops a live foreign key the desired file changes under the same name on a partitioned table or in a plan that also retypes a column or drops one beside an added column that may rewrite the table, which would leave writes checked against no key until the new key is added; declaring the changed key under a new name adds it first. Or it drops a live foreign key into another schema, or a key from a table to itself whose referenced columns are unique only through a standalone unique index. A desired file cannot declare such a key, so the diff plans its drop whatever the file says; the plan still shows the statement so the diff is reviewable, and `migrate --alter` runs the drop. Every other foreign-key drop executes. |

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
| `partitioned-index-build` | The one step is a `CREATE INDEX` on a partitioned table, and the executor runs it as the partition-aware index build, never as the statement: the parent's index is created with `ONLY`, each partition's index is built concurrently and attached, and the parent's index turns valid with the last attach. The partitions are listed when the build runs, not when the plan is made, so they are not in `exec_sql`. The statement must not be run as written, which would build every partition's index under a lock that blocks writes to the whole tree. A failed or interrupted run leaves an invalid parent index and the partition indexes built so far; running the same statement again resumes from them. |
| `partitioned-foreign-key-build` | The one step is an `ALTER TABLE ... ADD FOREIGN KEY` on a partitioned table on PostgreSQL 14–17, and the executor runs it as the partitioned foreign key build, never as the statement: each partition is given the foreign key `NOT VALID` and validated while writes continue, then the parent's `ADD FOREIGN KEY` adopts every partition's in one brief transaction that validates nothing and holds `ACCESS EXCLUSIVE` on the referenced table. The partitions are listed when the build runs, not when the plan is made, so they are not in `exec_sql`. The statement must not be run as written, which would validate every partition while it blocks writes to the whole tree. A failed or interrupted run leaves the partition foreign keys finished so far; running the same statement again resumes from them. A `migrate --dry-run` plan carries it for a submitted statement, and a `diff` plan for a foreign key a desired file adds, whose `exec_sql` is the planned `add-foreign-key` statement, its referenced table qualified with the target schema; on PostgreSQL 18 the same statement plans as `autocommit-each-step` (`NOT VALID`, then `VALIDATE`). |
| `blocking-index-drop` | Only on a refused `drop-index` statement whose table is partitioned: the one step is the plain `DROP INDEX` of the parent's index, which PostgreSQL cannot drop concurrently (`DROP INDEX CONCURRENTLY` fails with SQLSTATE `0A000`). It takes `ACCESS EXCLUSIVE` on the parent, every partition, and every partition's index for as long as it runs, so the statement stays `refuse` and the report's disposition with it. `migrate --desired` runs it only when the run pins this plan's fingerprint and names the table in `--allow-destructive` and `--accept-blocking`, with an explicit `--statement-timeout` longer than `--lock-timeout`: in one engine-owned transaction it requests the parent's lock under `lock_timeout`, reads the index again (still the parent's partitioned index, with the `drops` definition), drops it under `statement_timeout`, and reads back that the parent's index and every attached partition index are gone before it commits. It is never run `CONCURRENTLY` and never falls back to another form. A consumer that runs statements itself must not run it as an online step. |
| `partitioned-key-build` | The one step is an `ALTER TABLE ... ADD PRIMARY KEY` or `ADD UNIQUE` on a partitioned table, and the executor runs it as the partitioned key build, never as the statement: each partition's unique index is built concurrently and given the constraint with `ADD CONSTRAINT ... USING INDEX`, then the parent's `ADD CONSTRAINT` adopts every partition's constraint in one brief transaction that builds nothing. The partitions are listed when the build runs, not when the plan is made, so they are not in `exec_sql`. The statement must not be run as written, which would build every partition's index under `ACCESS EXCLUSIVE` on the whole tree. A failed or interrupted run leaves the partition constraints finished so far; running the same statement again resumes from them. A `migrate --dry-run` plan carries it, and so does a `diff` plan for a key a desired file adds whose shape the build accepts; a desired key the build cannot carry, such as a `DEFERRABLE` one, is refused with `unsupported-partitioned-parent`. |

### Dispositions (`disposition`)

| Value | Meaning |
|---|---|
| `execute` | The engine would run it now — natively, or through the copy-and-swap backend when that is the statement's `backend`. The disposition is decided from the statement and the table's shape as planned; it does not run the copy-and-swap preflight (free disk, logical decoding, triggers, dependent views, publication membership), which execution reads before any statement of the plan runs and which can refuse the whole plan with `copy-and-swap-refused` and nothing executed. (A `migrate` dry run reads one shape fact of that preflight, a table in a partition or inheritance tree, and ends in the refusal instead of a report; see the top of this page.) Nor does a `diff` report say whether a run will open the route: copy-and-swap is in preview, and a run without `--copy-and-swap` (`CopySwapOptions.Enabled`) refuses the plan the same way with cause `copy-and-swap-not-enabled`. A `migrate` dry run takes `--copy-and-swap` as its run does and, without it, ends in that refusal instead of a report, so a report it prints with this disposition is one whose run opens the route. |
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
| `pre-add-validated-check` | Pre-add a validated CHECK matching the partition bound on the child, and one excluding it on the `DEFAULT` partition, and build the parent's indexes on the child concurrently (a unique one backing its constraint through `USING INDEX`); attach, then drop the CHECKs. |
| `not-null-scaffold` | Prove the invariant with a NOT VALID CHECK plus online VALIDATE, then flip NOT NULL. |
| `name-constraint-then-validate` | Name the constraint, add it NOT VALID, then VALIDATE it online. |
| `unique-index-then-constraint` | Build the unique index with CREATE UNIQUE INDEX CONCURRENTLY, then attach it with ADD CONSTRAINT … USING INDEX. |
| `expand-contract-column` | Add the new column, dual-write and backfill it, switch reads, then drop the old column as its own change — instead of renaming the column under running application code. |
| `coordinated-table-rename` | Coordinate the table rename with the application deploy that adopts the new name, then run it with `--force` naming the table. |
| `coordinated-constraint-rename` | The renamed index backs a constraint, which the rename renames: coordinate the rename with the application deploy that stops naming the old constraint (`ON CONFLICT ON CONSTRAINT`, error handling). |
| `alter-column-type-in-place` | Never on a report statement: a `migrate` refusal verdict with reason `destructive-change` carries it when one ALTER TABLE drops a column and adds it back under the same name. Change the column in place with ALTER COLUMN ... TYPE ... USING ..., which keeps its values. |
| `add-integer-primary-key` | Never on a report statement: a `migrate` refusal verdict with reason `copy-and-swap-refused` and cause `copy-and-swap-pk-unsupported` carries it when the table has no primary key. Add a single-column integer primary key online, as [primary-keys.md](primary-keys.md#add-a-primary-key-online) shows, then run the change again, which checks the table's other shape facts, such as its replica identity and foreign keys. |

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
| `drop-index` | Drops an index. One that carries `replaced_by` drops a changed index the plan replaces, or a unique index a unique constraint of the same name replaces, after the replacement is built. |
| `drop-constraint` | Drops a table constraint other than a foreign key. One that carries `replaced_by` drops a unique constraint a unique index of the same name replaces, after the index is built. |
| `drop-foreign-key` | Drops a foreign-key constraint. Destructive; it runs under the brief lock budget and takes `ACCESS EXCLUSIVE` on both the table and the referenced table. A key the file moves to a new name is dropped after the new key is added and validated. A key changed under the same name is dropped by the `add-foreign-key` statement that adds its new definition `NOT VALID`. The drop of a key a desired file cannot declare, or of one changed under the same name on a partitioned table or in a plan that also retypes a column or drops one beside an added column that may rewrite the table, is refused (`declarative-foreign-key`). |
| `drop-column` | Drops a column. |
| `add-column` | Adds a column. |
| `alter-type` | Changes a column's type. |
| `set-generated-expression` | Replaces a stored generated column's expression. Always refused for execution (`unsupported-statement`): it recomputes every row under an exclusive lock, and no online path exists. The statement needs PostgreSQL 17 or later. |
| `set-default` | Sets or replaces a column default. |
| `drop-default` | Drops a column default. |
| `set-not-null` | Adds the NOT NULL attribute. |
| `drop-not-null` | Removes the NOT NULL attribute. |
| `add-constraint` | Adds a table constraint other than a foreign key. |
| `add-foreign-key` | Adds a foreign-key constraint. Runs as `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE CONSTRAINT`, which checks the existing rows while reads and writes continue. |
| `validate-constraint` | Validates a live check constraint left `NOT VALID` whose definition otherwise matches the desired one, the state an interrupted online add leaves behind. Not destructive. |
| `validate-foreign-key` | Validates a live foreign key left `NOT VALID` whose definition otherwise matches the desired one. Runs as written; the scan takes `SHARE UPDATE EXCLUSIVE`, so reads and writes continue. |
| `create-index` | Creates an index. A changed index the plan replaces is built under its temporary name, `<name>_new`. |
| `rename-index` | Renames an index (`ALTER INDEX ... RENAME TO`): a replacement index from its temporary name to the name of the index or unique constraint it replaced, after the old one is dropped, or a live index the desired table declares under another name with an otherwise identical definition. Not destructive. Execution reads the index again under the table's lock and refuses with `renamed-index-drift`, renaming nothing, if it is invalid, backs a constraint, or no longer has the definition the plan carried. |
| `add-enum-value` | Adds a label to an enum type (`ALTER TYPE ... ADD VALUE`), placed `BEFORE` the next label the type already carries when the file lists it ahead of one. The only kind a desired enum type file plans. Not destructive. |

### Advisory kinds (`advisories[].kind`, diff source only)

Both kinds describe an invalid index on a plain table — a concurrent build that did not
finish, abandoned or still running — which the diff never drops: a `DROP INDEX` by name
cannot tell the two apart ([OC-2, LK-5](invariants.md)). A partitioned parent's invalid
index means unattached partition indexes, not an unfinished build, and is never advised. A
consumer that meets a kind it does not recognize must surface it, never discard it.

| Value | Meaning |
|---|---|
| `invalid-index-rebuild` | The desired schema names the index (same or redefined definition), so the plan carries its `create-index`. That create cannot run as-is against the occupied name; the plan completes only through the proven-removal path — `executor.RebuildAbandonedIndex` or the [invalid-index runbook](invalid-index-recovery.md) — which drops the entry after proving it abandoned. |
| `invalid-index-undeclared` | The desired schema does not name the index. The diff plans no drop for it, so no statement touches it: an empty plan is not evidence the table is clean. Clear an abandoned entry per the [runbook](invalid-index-recovery.md), or wait for a build still running to finish. |

## Fingerprint

`fingerprint` is the plan's stable identity: `sha256:` plus the hex digest over what would
execute. It exists for one consumer protocol: an approver pins it when the plan is
reviewed, and an executor recomputes it at apply time and refuses on mismatch — that is how
"the plan a reviewer approves is the plan that executes" survives storage and forwarding.
The engine computes and reports the fingerprint on every plan; enforcing the pin at apply
time is the consumer's side of the contract.

The serialization is exact and pinned by test: for each statement in plan order, hash the
canonical `sql`, `route`, `backend`, and `disposition`, then each `exec_sql` entry, then — only
for a statement whose `execution` is `partitioned-index-build`, `partitioned-key-build`,
`partitioned-foreign-key-build`, or `blocking-index-drop` — that `execution` value. Every field is
followed by a unit separator (`0x1F`), and each statement closes with a record separator
(`0x1E`). The `execution` is hashed for those four values because their `exec_sql` is the
statement as written: each build's statement run literally (a `CREATE INDEX CONCURRENTLY` or an
`ALTER TABLE ... ADD PRIMARY KEY` on a plain table, an `ALTER TABLE ... ADD FOREIGN KEY` that
validates every partition under the tree's lock) is a different plan, and
a reviewed drop of a partitioned table's index pins the blocking form a run that accepts it
executes, so the plan cannot switch drop forms unnoticed; a statement with any other
`execution` hashes as it did before. Explanatory fields (`decisions`, `kind`, `destructive`, `reason`, `cause`,
`guidance`, and the report-level `advisories`) are excluded: a reworded reason does not
change identity, but a rerouted, resequenced, or rewritten plan does. An empty plan has a
defined identity (the digest of no input) — with or without advisories, since nothing in
an advisory executes.

When the report carries `extension_schemas`, a group separator (`0x1D`) follows the last
statement, then each schema in order, each followed by a unit separator. A plan without
them hashes exactly as before. A table that does not exist yet is created from the desired
file's own statements, whose unqualified type and function names resolve through those
schemas at apply time, so the same SQL resolved against other schemas is a different plan.

A statement that carries `drops` adds a file separator (`0x1C`) after its `exec_sql`, then
the dropped object's `kind`, `name`, and `definition`, each followed by a unit separator,
then each of its `dependents` in order, each followed by a unit separator, before its record
separator. A drop with no dependents adds nothing for them. The same SQL that drops a different live object, such as an
index of the same name with another definition, is a different plan.

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
  "format_version": 32,
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
          "reason": "metadata-only",
          "proven_catalog_only": true
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
  "format_version": 32,
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
          "reason": "fast-default",
          "proven_catalog_only": true
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
  "format_version": 32,
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
