# Refusal classes: a routing contract above reasons

**Decision: every refusal carries a machine-readable `class` that tells a consumer what
kind of boundary it reached.** `reason` continues to identify the immediate cause; `class`
answers the next question: wait for engine capability, hand the work to its owner, use the
named safer idiom, or change the run environment.

Every refused statement has an `outcome`, a typed `reason`, a typed `class`, explanatory `detail`, and,
where one exists, a `safer_idiom`. Exit code 2 means nothing committed. That is enough to explain a
single refusal, but not enough to route it: `unsupported-statement` alone covers a data
backfill, an imperative `CREATE TABLE`, a permanently unsafe `CREATE INDEX IF NOT EXISTS`,
and an admitted `ALTER TABLE` operation for which the planner has no route. Those are not
the same kind of answer.

## The decided vocabulary

The refusal verdict carries `class`, with exactly five values:

| Class | Meaning | Consumer action |
| --- | --- | --- |
| `capability-boundary` | The engine cannot do this yet, or has no implemented safe route. This is a T2 capability. | Wait for the capability or escalate. |
| `no-online-safety-problem` | There is nothing for an online schema-change engine to make safe; another tool class owns the work, or the operator runs it directly. | Hand the statement to the named owner. |
| `by-design` | pg-sprite permanently refuses this form — because PostgreSQL offers no online mechanism in any supported version, or because a safer idiom exists; the verdict names the idiom or the deliberate path. This is [RF-5](invariants.md#refusals-and-preflight-rf) seen from the consumer's side. | Use the safer idiom. For a single-relation plain `DROP INDEX` (not `CASCADE`), `REINDEX INDEX`, or `REINDEX TABLE`, `migrate --accept-blocking SCHEMA.TABLE` runs the blocking form under bounded budgets and exits 3 ([lock-budgeted-passthrough.md](lock-budgeted-passthrough.md)); otherwise run it outside pg-sprite in a maintenance window. |
| `environmental` | The change is supportable, but not here, now, or as this role: a size policy, exhausted budget, privilege, server version, stale plan, or catalog collision stopped it. | Retry, provision, upgrade, re-plan, or escalate. |
| `invariant-violation` | pg-sprite refused because its own input or state is incoherent — a report this build cannot have produced. This is a bug in pg-sprite, not a boundary of it; it is the refusal-verdict face of the fail-closed `ErrInvariantViolation` rule in [SAFETY.md](../SAFETY.md#rules-inside-the-core). | Report it with the verdict; do not retry, wait, or route elsewhere. |

`environmental` and `invariant-violation` have no capabilities-matrix tier. They describe the
run, not the operation. A supported operation can produce either without changing tier.

```text
┌──────────────────────────┬──────────────────────────────┐
│ class                    │ consumer route               │
├──────────────────────────┼──────────────────────────────┤
│ capability-boundary      │ wait / escalate              │
│ no-online-safety-problem │ hand to owner tool           │
│ by-design                │ use safer idiom              │
│ environmental            │ retry / provision / escalate │
│ invariant-violation      │ report a pg-sprite bug       │
└──────────────────────────┴──────────────────────────────┘
```

## The current reason-to-class map

The closed reason set is `verdict.Reasons()`. The tables below classify every current
emission, including desired-state admission. A reason with several rows is deliberately not a
class.

**The class is a property of the typed cause where one exists, and of the refusal site only
where none does.** Three reasons carry a second, typed discriminator beneath them —
`unsupported-partitioned-parent` carries a `preflight.PartitionRefusalCause`, and
`unsupported-statement` carries an `executor.CreateShapeCause` on the create path or one of a
closed set of admission sentinel errors on the imperative path — and in each case the values
of that discriminator span more than one class. A single site can therefore emit several
classes (`admissionRefusalVerdict` in `pkg/migrate/verdicts.go` mints refusals for three
sentinels across two classes), so keying on the site would leave the class ambiguous and let a
completeness test pass with the wrong answer. Where a reason has no such discriminator, the
site is the key. The same rule applies when a broad reason gains a new cause or site: the
author classifies it explicitly; nothing is ever derived from the reason string.

One site still covers several rows: the imperative front door refuses every statement kind it
does not admit through a single catch-all in `pkg/migrate/verdicts.go` (`statement.KindOther`),
which today cannot tell a backfill from a `GRANT`. Classifying that site means the parse
boundary in `pkg/statement` distinguishes the kinds the rows below name, so that a `GRANT` is
routed to provisioning and never to the data change runner.

### Reasons keyed on the site

| Existing `reason` | Refusal site or shape | `class` | `owner`, when present |
| --- | --- | --- | --- |
| `unsupported-statement` | DML such as an `UPDATE` backfill | `no-online-safety-problem` | `data-change-runner` |
| `unsupported-statement` | Grants, roles, row-level-security policies, publications, subscriptions | `no-online-safety-problem` | `provisioning` |
| `unsupported-statement` | `DROP TABLE`, views, functions, triggers, extensions, standalone sequences, and other catalog work the matrix marks ⚪ | `no-online-safety-problem` | `direct-operator` |
| `unsupported-statement` | Imperative `CREATE TABLE`; the detail already points to `diff` | `no-online-safety-problem` | `declarative-front-door` |
| `unsupported-statement` | An admitted `ALTER TABLE` operation for which the planner has no route (`routeRefusalVerdict`) | `capability-boundary` | — |
| `unsupported-statement` | Imperative admission sentinel `ErrUnsupportedSequenceStep` or `ErrUnnamedIndex`; create admission sentinel `ErrUnsupportedCreateStep` | `capability-boundary` | — |
| `unsupported-statement` | Imperative or create admission sentinel `ErrIfNotExistsUnsupported`; create admission sentinel `ErrDuplicateCreateName` (the sentinel forms of the `if-not-exists` and `duplicate-name` causes below) | `by-design` | — |
| `unsupported-statement` | Create admission sentinel `ErrPartitionOfUnsupported` (the sentinel form of the `partition-of` cause below) | `capability-boundary` | — |
| `unsupported-statement` | `planRefusal` in `pkg/migrate/desired.go`: a statement whose disposition this build does not know, or a plan whose aggregate disposition no statement carries | `invariant-violation` | — |
| `index-statement` | Plain `DROP INDEX` or `REINDEX`, for which the verdict names the concurrent form; also a `DROP INDEX CONCURRENTLY` that PostgreSQL rejects (with `CASCADE`, or naming several indexes) | `by-design` | — |
| `index-statement` | An already-concurrent maintenance statement that pg-sprite need not wrap and PostgreSQL accepts | `no-online-safety-problem` | `direct-operator` |
| `not-native-safe-table-too-large` | The size policy prevents a blind bounded attempt | `environmental` | — |
| `insufficient-privileges` | The connected role lacks a required grant | `environmental` | — |
| `not-native-safe-budget-exceeded` | The lock or statement budget cancels an attempt | `environmental` | — |
| `not-native-safe-rewrite-required` | The planner cannot construct the required safer sequence | `capability-boundary` | — |
| `backend-unavailable` | The plan routes to copy-and-swap for an operation the engine does not carry yet | `capability-boundary` | — |
| `destructive-change` | Desired-state execution will not infer permission to discard live structure | `by-design` | — |
| `plan-fingerprint-mismatch` | The recomputed plan is not the reviewed plan | `environmental` | — |
| `create-collision` | A target or claimed name is occupied at apply time by a relation or a standalone type | `environmental` | — |
| `declarative-foreign-key` | A desired-state plan adds or drops a foreign key, or creates a table that declares one; desired-state execution does not run foreign-key changes yet | `capability-boundary` | — |

### `unsupported-partitioned-parent`, keyed on `PartitionRefusalCause`

The four causes span three classes. [RF-6](invariants.md#refusals-and-preflight-rf) already
keeps the not-implemented case and the version-gated case apart in one sentence; this table
agrees with it rather than collapsing them. An importer reads the cause from the `Cause` field of
`*UnsupportedPartitionedParentError` (`errors.As`), and `PartitionRefusalCause.Class()` answers
the `class` column below as a `refusal.Class`, which converts to `verdict.Class` with a plain
conversion.

| `PartitionRefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `parent-concurrent-index-build` | The `CREATE INDEX ON ONLY` → per-partition `CONCURRENTLY` → `ATTACH PARTITION` flow is not yet implemented | `capability-boundary` | A planned engine capability; the matrix marks it 🟡. |
| `parent-blocking-index-build` | pg-sprite will not substitute a blocking parent build for the missing partition-aware flow | `capability-boundary` | Same missing flow; the refusal of the blocking substitute is the policy half of the same gap. The eligibility registry marks this cause acceptable, but `migrate --accept-blocking` does not reach it yet: the unforced imperative path refuses the parent build with the concurrent cause, and the flag rejects `--force`. |
| `parent-index-adoption` | PostgreSQL does not support adopting an existing index as a constraint on a partitioned parent in any supported version | `by-design` | No online mechanism exists; the matrix marks it ❌. Waiting for a pg-sprite release would wait for nothing. |
| `parent-not-valid-foreign-key` | PostgreSQL before version 18 cannot add a `NOT VALID` foreign key on a partitioned table | `environmental` | The same statement, table, and pg-sprite build runs on a newer server; the action that unblocks it is a server upgrade, not an engine release. The matrix marks this row ✅ with a server-version precondition. |

### `copy-and-swap-refused`, keyed on `CopySwapRefusalCause`

The closed set is `preflight.CopySwapRefusalCauses()`, raised before the copy-and-swap route
writes anything ([ST-6](invariants.md#st-6--preflight-before-the-first-write),
[RF-1](invariants.md#refusals-and-preflight-rf), [RF-2](invariants.md#refusals-and-preflight-rf)).
`CheckCopySwapShape` raises the shape causes as an `*UnsupportedCopySwapShapeError` and
`CheckCopySwapEnvironment` raises the cluster and volume causes as a
`*CopySwapEnvironmentError`; `CopySwapRefusalCauseOf` reads the cause through either. An
environment refusal also carries the typed `CopySwapSetting` the operator must change
(`wal_level`, or `rds.logical_replication` where the server defines that parameter;
`max_replication_slots` or `max_wal_senders`; none for disk), so an adapter never parses
the prose detail to learn which knob to turn.

An adapter that calls `CheckCopySwap`, which folds the privilege, shape, and environment checks
into one call, sees refusals from all three in that order, and must read each in its own
vocabulary — `CopySwapRefusalCauseOf` answers only for the shape and environment refusals:

| Outcome of `CheckCopySwap` | How to read it | What it means |
| --- | --- | --- |
| `*PrivilegeError` | `errors.As`; `Grant` is the exact statement to run | The engine role lacks the copy-and-swap tier against this table, or (for a run that decodes WAL) replication access. Raised before any shape or cluster read. |
| `*UnsupportedCopySwapShapeError` | `CopySwapRefusalCauseOf` → a shape cause from the table below | The table's shape is outside what the route supports. |
| `*CopySwapEnvironmentError` | `CopySwapRefusalCauseOf` → a cluster or volume cause from the table below, plus `Setting` | The cluster or the volume cannot carry the run. |
| `ErrCopySwapProofMismatch` | `errors.Is` | The proofs handed in do not fit together: a shape minted for another database or dropped relation, or a decoding run on a shape whose privilege proof did not verify replication access. An adapter that calls the fold never produces this. |
| any other error | wrapped query error | A catalog read failed; retry or surface it as an engine failure. |

`CopySwapRefusalCause.Class()` answers the `class` column below as a `refusal.Class`, which
converts to `verdict.Class` with a plain conversion. The imperative front door carries every
one of these causes on a verdict under the single reason `copy-and-swap-refused`: the verdict's
`cause` is the preflight cause unchanged, so an adapter that switches on this table's first
column reads the same token off the verdict, and its `class` is the class fixed here
(`pkg/migrate/copy_swap_refusal.go`, pinned against this closed set by the registry's
completeness test).

| `CopySwapRefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `copy-and-swap-pk-unsupported` | The table has no single non-deferrable `smallint`, `integer`, or `bigint` primary-key column for the chunker to range over and the buffer to key on | `capability-boundary` | Wider key shapes are a planned engine capability ([D4](copy-and-swap-design.md#d4--restrict-the-chunk-key-to-one-integer-family-primary-key)). |
| `copy-and-swap-replica-identity` | The table's replica identity is `NOTHING` or a named index; `DEFAULT` or `FULL` is required | `environmental` | The same table is admitted after `ALTER TABLE … REPLICA IDENTITY DEFAULT` or `FULL`; the action that unblocks it is a catalog change, not an engine release. |
| `copy-and-swap-foreign-keys` | A foreign key references the table or leaves it | `capability-boundary` | An OID-bound dependent the rename swap would strand on the old table; re-pointing it is a planned capability. |
| `copy-and-swap-triggers` | The table has a user trigger or a rewrite rule | `capability-boundary` | As above. |
| `copy-and-swap-partitioned` | The table is a partitioned parent, a partition, or part of an inheritance tree | `capability-boundary` | The per-partition copy-and-swap flow is a planned capability. |
| `copy-and-swap-unlogged` | The table is UNLOGGED, while the shadow would be permanent | `capability-boundary` | Preserving persistence requires an explicit shadow-creation path. |
| `copy-and-swap-force-rls` | The table has `FORCE ROW LEVEL SECURITY`, so the owner-run copier would be filtered reading the source and rejected filling the policy-carrying shadow | `capability-boundary` | Copying under a `BYPASSRLS` role or deferring the policies to cutover is a planned capability; either needs a decision the engine has not made. |
| `copy-and-swap-dependent-views` | A view or materialized view selects from the table | `capability-boundary` | The view's rewrite rule is bound to the table's OID and would keep reading the retained old table after the swap; recreating dependents at cutover is a planned capability. |
| `copy-and-swap-publication-member` | A publication other than the engine's own for this table publishes it, whether by explicit membership, `FOR ALL TABLES`, or `FOR TABLES IN SCHEMA` | `capability-boundary` | Explicit membership is bound to the table's OID, so subscribers would keep following the old table; a database- or schema-scoped publication would publish the shadow's copy writes to subscribers that have no such relation. Re-adding the swapped-in table, or excluding the shadow, under the publication's lock is a planned capability. |
| `copy-and-swap-subscription-target` | A subscription on this database applies changes into the table | `capability-boundary` | The subscription binds the table by OID; after the swap its apply worker finds no state for the new relation and skips its changes without an error. Re-pointing the subscription at cutover is a planned capability. |
| `copy-and-swap-dependents` | An object none of the causes above name depends on the table's OID or its row type: a rule on another table writing into it, a SQL-standard function body reading it, a policy on another table consulting it, a column of its row type | `capability-boundary` | The swap carries none of them to the new table, so each would follow the retained old table ([RF-2](invariants.md#refusals-and-preflight-rf)); recreating or re-pointing them at cutover is a planned capability. |
| `copy-and-swap-logical-decoding-unavailable` | A run that decodes WAL needs `wal_level = logical`, and the server reports another level. The refusal names `rds.logical_replication = 1` on Aurora and RDS, where the server defines that parameter and `wal_level` follows it, and `wal_level` on every other server; a managed service other than RDS that drives `wal_level` from a switch of its own is not detected, so the operator maps `wal_level` to that switch | `environmental` | The same table is admitted after the static setting is changed and the server restarted; a quiesced run on the same server is admitted as is. |
| `copy-and-swap-slot-collision` | A run that decodes WAL derives its slot name from the target, and a replication slot of that name already exists for another database or as a physical slot | `environmental` | Slot names are cluster-wide, so the route can neither share nor drop a slot it does not own ([D11](copy-and-swap-design.md#d11--bound-and-reap-logical-decoding-state)); the operator renames or drops the foreign slot, and no setting is named. |
| `copy-and-swap-slot-headroom` | A run that decodes WAL needs one free replication slot and two free WAL senders (the slot's snapshot connection and the decoding stream), and `max_replication_slots` or `max_wal_senders` cannot supply them | `environmental` | Dropping a stale slot or raising the setting admits the same run; nothing about the table changes. |
| `copy-and-swap-disk-headroom` | The free space the caller measured on the database volume is below the shadow copy's headroom, a fixed multiple of the table's total size, or was not measured at all | `environmental` | Freeing or adding space admits the same run. An unmeasured volume is refused rather than assumed sufficient, because the shadow and retained old table would otherwise fill it mid-copy. |

### Shadow operation refusals, keyed on `RefusalCause`

The closed set is `schemachange.RefusalCauses()`, carried by the `*schemachange.RefusalError`
that `BuildShadow`, `InspectShadow`, `DropShadow`, `GateCutover`, `Cutover`, `InspectSwapped`,
`DropOldTable`, `InspectResume`, and `Reconciler.Reconcile` return; `schemachange.RefusalCauseOf`
reads it through wrapping. Every one of these refusals is fail-closed under
`ErrInvariantViolation` ([SAFETY.md](../SAFETY.md#rules-inside-the-core)): the sentinel is the
mechanism that stops the operation, and the cause is what tells the importer which way to
react. Only the causes that mean the caller handed the engine an incoherent input, or that the
engine's own write did not take, are `invariant-violation`; one, `shadow-statement-conversion`,
is a `capability-boundary`, since it describes a statement the engine declines to carry rather
than a fault; the rest describe a lock, a proof, or a relation whose state moved under the
operation, and the importer retries, re-plans, or cleans up. `RefusalCause.Class()` answers
the `class` column below as a `refusal.Class` (`pkg/refusal`, the core's declaration of the
classes the cause tables in this document use), so an importer triages on the class without
reading the cause's text, and a periphery adapter converts it to `verdict.Class` with a plain
conversion, since the verdict declares its tokens from the same constants. The class is
triage, not the action: an `environmental` cause such as `cutover-outcome-ambiguous` or
`shadow-foreign-relation` is resolved by an operator reading the catalog, as its *Why* says,
and no class licenses an automatic retry on its own. The imperative front door carries
these causes on a verdict under the same `copy-and-swap-refused` reason as the preflight
causes above: the verdict's `cause` is the shadow cause unchanged and its `class` is
`RefusalCause.Class()` converted by token (`pkg/migrate/copy_swap_refusal.go`).

| `RefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `shadow-lock-unproven` | The operation was handed no table lock session, or one whose proof is empty or names a different table ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `invariant-violation` | A caller passed a lock that cannot cover this table; no retry against the server changes that. |
| `shadow-lock-lost` | The table lock session reported that it lost the lock, before or during the operation ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The operation's transaction is aborted and nothing is written; re-acquire the lock and repeat. |
| `shadow-lock-held-elsewhere` | `pg_locks` shows the table lock granted to a backend other than the lock session's own ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | Another engine instance holds the table; this one yields and the operator decides which run continues. |
| `shadow-lock-unconfirmed` | `pg_locks` shows no session holding the table lock, although the lock session has not reported loss ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The lock is gone from the server's point of view before the session noticed; re-acquire and repeat. |
| `shadow-proof-empty` | The copy-and-swap target proof is the zero value ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | Only the shape check mints a populated proof; a zero one was constructed, not earned. |
| `shadow-source-shape` | The source's catalog is outside the shape the proof admits: an identity column without an internally owned sequence, or a replica identity other than `DEFAULT` or `FULL` ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `environmental` | The catalog changed after preflight admitted the table; re-run preflight against the table as it is now. |
| `shadow-statement-target` | The gated statement is not an `ALTER TABLE` on the proven table, or its retargeted form does not name the shadow with the same operations ([ST-7](invariants.md#st-7--the-executor-runs-exactly-the-statement-that-was-gated)) | `invariant-violation` | The caller gated one statement and handed the build another; the pairing is the caller's, not the server's. |
| `shadow-statement-conversion` | The gated statement retypes a column with a `USING` expression the copy cannot carry as written: the deparser cannot prove it rendered the expression faithfully, the expression refers to any column but the one it retypes (another column, a table-qualified name, or a whole-row `t.*`) — the catch-up converts one column value at a time and has no neighbouring column to hand it — the statement retypes the same column twice, the expression converts the primary key, whose values the copy's chunk ranges, watermark, and resume clear are expressed in, or the server does not classify the expression as `IMMUTABLE` — it reads the clock, the session, or a setting, or calls a `STABLE` or `VOLATILE` function — so the copy's evaluation, the verifier's recopy, and the catch-up's flush of the same source value need not agree ([ST-7](invariants.md#st-7--the-executor-runs-exactly-the-statement-that-was-gated)) | `capability-boundary` | The engine has no proof it would reproduce the conversion, so it refuses before any shadow is committed: the parse-level refusals before the build's transaction opens, the immutability refusal inside it, by trying the expression as a stored generated column's expression on the empty shadow and rolling the attempt back, so the build rolls back with it. Rewrite the expression in terms of the retyped column alone and of immutable functions, or retype the key without `USING` so the server's assignment cast converts it. `CauseStatementConversion` is the branch point a caller matches on; the sentinel wrapped for `errors.Is` refines which rule refused — `statement.ErrConversionRendering`, `ErrConversionForeignColumn`, or `ErrConversionRepeated` from the parse boundary, `schemachange.ErrConversionNotImmutable` from the build's probe. The message names the column, the expression's text, and the rewrite; the cause carries none of them. |
| `shadow-owner-mismatch` | The shadow the build created is not owned by the source's owner ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The owner the proof carries is no longer the owner the catalog reports; the shadow is dropped and preflight re-run. |
| `shadow-foreign-relation` | The relation wearing the shadow's name is not a plain table owned by the source's owner ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The engine did not build it and will not touch it; an operator removes or renames the relation. |
| `shadow-grants-differ` | The shadow's grants still differ from the source's after the build synchronised them ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `invariant-violation` | The build wrote the grants under the table lock and read back something else; that is the engine's write, not the environment. |
| `shadow-identity-handoff` | A source identity column the change kept on the shadow does not carry `DEFAULT nextval(<source sequence>)` there, or the shadow declares an identity of its own ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The change itself, or a later edit to the shadow, replaced the handoff; change the statement or drop the shadow and rebuild. |
| `cutover-unverified` | The cutover gate was handed an empty built-shadow or verified-shadow proof, two proofs naming different relations or different relation OIDs (a shadow rebuilt under the same name after the checksum ran), or a verified watermark that does not cover the whole key space ([CO-1](invariants.md#co-1--the-checksum-gate-is-non-skippable)) | `invariant-violation` | Only the copier and verifier mint those proofs for the same table; a caller that reaches the gate without them skipped the checksum, which no retry corrects. |
| `cutover-undrained` | The cutover was handed a verified-shadow proof a live pass minted — the shadow equal to the source as of a position the stream had reached, with every change since still in the stream — and no drain to apply those changes under the swap lock ([CO-1](invariants.md#co-1--the-checksum-gate-is-non-skippable)) | `invariant-violation` | Pass a `DrainFunc` that applies the stream through the final position under the swap lock, or stop the stream and re-verify with a quiesced `Check`; the proof is sound, the hand-off to the swap is not. |
| `cutover-drain-deferred` | The shadow refused a batch the swap's drain had to apply under the swap lock: a unique-key move among the changes committed since the live verification pass that cannot be ordered into the shadow, with no copy left to change the outcome ([CO-6](invariants.md#co-6--unique-secondary-key-moves-must-converge-postgresql-specific-gap)) | `environmental` | The swap transaction is rolled back and the source stays live; the row is left in `cutover` with the position the drain reopened from, so a resume drains the same changes again once the rows the move touched have settled. A move the shadow cannot ever order is re-planned. |
| `cutover-unfenced` | The quiesced `Runner`'s swap, under its `ACCESS EXCLUSIVE` lock, did not find the engine's write fence on the source enabled `ALWAYS`: the trigger was disabled, dropped, or replaced by one of another shape — a `WHEN` condition, an `UPDATE OF` column list, arguments, or deferral — since the run installed it ([CO-1](invariants.md#co-1--the-checksum-gate-is-non-skippable)) | `environmental` | Something other than the engine changed the source's triggers under the run, so a write may have reached it after the verification pass. The swap rolled back and nothing is swapped; the next `Run` installs the fence again, verifies again, and swaps, and a write that did land makes that pass stop on a divergence. |
| `cutover-relation-replaced` | The source or the shadow under its name is no longer the relation the build proved: its OID moved; or, after the swap, the derived `_old` name is borne by a relation other than the source the swap retained, or by nothing while the retained source still exists under another name ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `environmental` | The proof describes a table that no longer exists; before the swap, drop the shadow and start the change over from preflight; after it, an operator reads the catalog to learn what took the `_old` name, or where the retained source went, before anything is dropped. |
| `cutover-schema-drift` | The source's or the shadow's introspected model no longer matches the fingerprint the build recorded: a column, constraint, index, storage parameter, or column-owned sequence declaration changed since ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | Someone changed a table under the run; the shadow no longer embodies the gated statement against this source, so it is dropped and the change re-planned. |
| `cutover-fidelity-drift` | The source's or the shadow's owner, grants, policies, comment, replica identity, per-column or extended statistics targets, unvalidated checks, or identity columns and their sequences no longer match the snapshot the build recorded, or the source's grants no longer match the shadow's ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The metadata the swap would carry forward moved after it was copied; re-synchronising it under the lock is a planned capability, and until then the shadow is rebuilt. |
| `cutover-index-invalid` | A shadow index is marked invalid in `pg_index`, so the swap would put an index the planner never uses under the user's name ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | A concurrent build on the shadow failed or was cancelled; reindex or rebuild the index and gate again. |
| `cutover-name-taken` | A name cutover must assign is already worn: a derived `_old` name by a relation or statistics object in the schema, or a source dependent's name by a shadow constraint that is not the one being renamed to it ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | A leftover from an earlier run, or a constraint the gated statement named after a source index, occupies the name; an operator removes or renames it and gates again. |
| `cutover-swap-mismatch` | The catalog re-read inside the swap transaction, after the renames and handoffs, does not show the shadow under the source's name, the source under its `_old` name, every paired dependent under its new name, or the live table's sequences and identity columns as the source had them; or, on a resume, the live or the old table is owned by a role other than the one the build recorded, or an identity column the swap recreated is missing from the live table, generated differently, or drawing from another sequence ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | The swap's own statements produced a catalog other than the one they were written to produce, so the transaction is rolled back and nothing is swapped; on a resume, the live table is not the one the swap left, and an operator reads the catalog before anything is dropped. |
| `cutover-outcome-ambiguous` | After a failed swap attempt or on a resume, the source's name is borne by neither the source nor the shadow the build proved, or the attempt's backend did not exit within the attempt's own `lock_timeout` plus `statement_timeout` so the catalog could not yet be trusted to show the outcome ([LK-4](invariants.md#lk-4--an-ambiguous-cutover-outcome-is-resolved-by-inspection-never-assumed)) | `environmental` | Something other than the engine renamed or dropped a table under the run, or a `COMMIT` is still deciding on the server; an operator reads the catalog to learn which relation is live before anything is retried or dropped. |
| `resume-row-terminal` | The checkpoint row handed to a resume is in a terminal phase — the run it records is done or failed — so there is nothing to resume or reconcile ([ST-4](invariants.md#st-4--slot-loss-is-a-modeled-state-transition-not-a-crash)) | `invariant-violation` | A caller asked to continue a run that has already ended; the row is read, not resumed, and a new run starts fresh. |
| `resume-row-cutover` | The checkpoint row handed to a resume or a reconcile is in the `cutover` phase: a swap attempt was underway when the run stopped, and whether it committed is written in the catalog, not in the row ([LK-4](invariants.md#lk-4--an-ambiguous-cutover-outcome-is-resolved-by-inspection-never-assumed)) | `invariant-violation` | A caller asked to resume past a swap whose outcome it has not read; `InspectSwapped` answers it — the shadow bears the source's name and the change is complete, or `ErrNotSwapped` and the resume runs `Cutover` — and nothing is reconciled either way, since a recopy into the shadow's name after a committed swap would write over the live table. |
| `restart-swap-committed` | A `Runner` asked to start over (`RunOptions.Restart`) met a `cutover` row whose swap the catalog shows committed: the shadow already bears the source's name, so there is nothing to discard and the change is past the point of starting over ([LK-4](invariants.md#lk-4--an-ambiguous-cutover-outcome-is-resolved-by-inspection-never-assumed)) | `environmental` | The operator asked to discard a run that has already swapped; the row is left as found, and a run of the same statement without the restart finishes it — drops the slot of a live run, drops or keeps the retained source, and writes the row done. Nothing is written by the refusal. |
| `resume-row-mismatch` | The checkpoint row handed to a resume does not describe the proven target and built shadow: it is keyed on another table, names another shadow or a slot other than the target's derived name, carries the fingerprints of another statement, carries a shadow proof that is missing, does not decode, or differs from the proof the shadow under its name re-derives, or (from the quiesced `Runner`) records no statement or a statement that, compared as parsed, is not the one `Run` was given ([ST-2](invariants.md#st-2--an-incompatible-checkpoint-is-distinguishable-from-a-transient-read-error)) | `invariant-violation` | A caller paired a row with a target or shadow it was not written for; no retry against the server changes that. |
| `resume-row-decoding` | The checkpoint row handed to a `Runner` was written by the other kind of run: the quiesced run was handed a row that names a slot or publication, or is in `catching_up` or `reconciling`, phases only a run that decodes WAL enters; or the live run was handed a row that names no slot ([ST-3](invariants.md#st-3--slot-cleanup-is-guaranteed-on-success-failure-and-crash)) | `invariant-violation` | A caller paired a row with a target proof of the other kind; continuing a decoding row without its slot would leave the slot as nobody's and the changes it carried unapplied, and a quiesced row names no position a stream could start from. The row is resumed under the proof kind that made it. |
| `shadow-unrecorded` | A relation already bears the target's derived shadow name while no checkpoint row records a run for the target, so nothing says which statement the shadow embodies ([ST-2](invariants.md#st-2--an-incompatible-checkpoint-is-distinguishable-from-a-transient-read-error)) | `environmental` | A leftover from a run whose row was deleted, or a relation someone else named; `DropShadow` removes the engine's own leftover and the run starts over. The wrapped `ErrShadowExists` is reachable with `errors.Is`. |
| `resume-shadow-gone` | The checkpoint row handed to a `Runner` resume is one whose swap has not committed — `copying`, `catching_up`, `reconciling`, `verifying`, or a `cutover` whose swap rolled back — and records a shadow under which no relation stands ([ST-2](invariants.md#st-2--an-incompatible-checkpoint-is-distinguishable-from-a-transient-read-error)) | `environmental` | Something other than the engine dropped the shadow; the run can neither continue on it nor start over on its own while the row stands. The refusal writes nothing. Running the statement again with a restart (`--restart`, `RunOptions.Restart`) discards the row and what it holds and starts over; by hand: A quiesced row is left for the operator to `Store.Delete`, and the next run builds a fresh shadow; the source keeps its write fence until that run's swap or `DropWriteFence`. A live row also names a slot, which retains WAL while it stands and which the next run would meet when it creates its own: the operator drops it with `decode.DropSlot` and then deletes the row; a live source has no fence. The wrapped `ErrShadowNotFound` is reachable with `errors.Is`. |
| `resume-slot-foreign` | The slot wearing the row's derived name is not this route's — a physical slot, or a logical slot of another database — so neither a clean resume nor a reconcile may decode from it or replace it, whatever its WAL status says ([ST-3](invariants.md#st-3--slot-cleanup-is-guaranteed-on-success-failure-and-crash)) | `environmental` | Something else took the engine's derived name after the row was written; an operator renames or drops that slot, and the resume is inspected again. The refusal also answers `errors.Is` for `decode.ErrForeignDecodingState`. |
| `resume-slot-held` | The row's slot is intact and the server still serves it, but another backend holds it, so no stream can be opened on it now; the holder's PID is named ([ST-4](invariants.md#st-4--slot-loss-is-a-modeled-state-transition-not-a-crash)) | `environmental` | A walsender of this engine's earlier process that has not yet noticed its client is gone lets go within `wal_sender_timeout`; anything else streaming from the engine's slot is an operator's to end. The resume is inspected again once the slot is free; a row already in `reconciling` is not refused, since the reconcile's drop waits the holder out. |
| `copy-shadow-mismatch` | The `Runner`'s re-mint of the copier cause of the same name ([ST-6](invariants.md#st-6--preflight-before-the-first-write)); the copier table below says what it means | `invariant-violation` | The copier table below. |
| `copy-frontier-broken` | The `Runner`'s re-mint of the copier cause of the same name ([CO-4](invariants.md#co-4--the-copyapply-ordering-invariants)), and the `Runner`'s own refusal of a copy that returned without error at a watermark short of the key space; the copier table below says what it means | `invariant-violation` | The copier table below. |
| `copy-chunks-in-flight` | The `Runner`'s re-mint of the copier cause of the same name ([LK-3](invariants.md#lk-3--pending-work-is-claimed-exactly-once-and-wait-means-finished)); the copier table below says what it means | `invariant-violation` | The copier table below. |
| `copy-chunk-unknown` | The `Runner`'s re-mint of the copier cause of the same name ([ST-6](invariants.md#st-6--preflight-before-the-first-write)); the copier table below says what it means | `invariant-violation` | The copier table below. |

Not every error a shadow operation returns is a refusal. `BuildShadow` applies the gated
statement to the empty shadow, and a statement the server itself rejects there — an index
whose access method has no operator class for a retyped column (SQLSTATE `42804`), a
constraint the new type cannot satisfy — surfaces as the server's `*pgconn.PgError`, wrapped
with the shadow's name and reachable with `errors.As`; `RefusalCauseOf` returns the empty cause
for it. An importer routes those by SQLSTATE, as it does for any other statement it runs, and
need not expect a `RefusalCause` for a change the server would have rejected on the source too.
`InspectSwapped` answers two catalog states with sentinels rather than refusals, because each
names the resume's next step rather than a fault: `ErrNotSwapped` (the source still bears its
name; no swap committed, so the resume runs `Cutover`) and `ErrOldTableNotFound` (the shadow
bears the source's name and the retained source's OID is gone from the catalog; the drop already
committed, so the change is complete). Both are reached with `errors.Is`, and `RefusalCauseOf`
returns the empty cause for them.

The four `resume-row-*` causes are `invariant-violation` because they describe the caller's
dispatch, not the database: a row's phase says which operation reads it. An importer that
loads a row reads `Phase.Terminal()` first — `done` and `failed` rows are reported, never
resumed — then hands a `cutover` row to `InspectSwapped`, and only a row in any other phase
to `InspectResume`. A caller that dispatches in that order never sees `resume-row-terminal`
or `resume-row-cutover`; one that does has a dispatch bug to fix, not a retry to make. The
`Runner` does that dispatch itself, so from it only `resume-row-terminal`,
`resume-row-mismatch`, and `resume-row-decoding` can surface: a caller that hands it a
`done` row, a row written for another shadow, or a row that the other kind of run made.

The four `copy-*` causes are the shadow-cause face of the copier's refusals: the `Runner`
re-mints every `copier.RefusalCause` as a shadow cause (the table in the next section maps
them) so a caller of `Runner.Run` reads one vocabulary whether the refusal came from a shadow
operation or from the copy. The copier's lock, proof, and relation refusals map onto the
`shadow-lock-*`, `shadow-proof-empty`, and `cutover-relation-replaced` rows above; the four
`copy-*` rows name the ledger and frontier faults that have no shadow-operation counterpart,
and are described once, in the copier table. The same literal in both vocabularies means the
same fault, so a log line that carries only the string is unambiguous.

Only the copier, the shadow operations, and the `Runner` carry typed causes today. The
applier's and the checksum pass's refusals — the same lock, proof, shadow, and relation
checks, made before a catch-up or a verification pass writes or reads — are fail-closed
under `ErrInvariantViolation` with the invariant tagged in the prose (`(LK-1)`, `(ST-6)`)
and no cause, so `RefusalCauseOf` returns the empty cause for them; an importer routes those
on the sentinel alone.

### Copier refusals, keyed on `copier.RefusalCause`

The closed set is `copier.RefusalCauses()`, carried by the `*copier.RefusalError` that
`NewCopier`, `Copier.Run`, and the chunker return; `copier.RefusalCauseOf` reads it through
wrapping. Every one of these refusals is fail-closed under `copier.ErrInvariantViolation`, the
same sentinel the shadow operations use. The `Runner` re-mints each as the shadow cause in the
last column, wrapping the copier's refusal so both `RefusalCauseOf` readers answer and
`errors.Is` still reaches whatever the copier wrapped — the lock session's loss, the server's
error. A refusal from `Runner.Run` therefore carries both causes; an importer that calls the
copier directly reads the copier's. A lock or relation fault during the copy phase surfaces
from `Runner.Run` under the shadow-operation name — `shadow-lock-lost`,
`cutover-relation-replaced` — by design: the fault is the same one a shadow operation would
refuse on, and the reaction is the same, whichever phase noticed it first.
`copier.RefusalCause.Class()` answers the `class` column below as a `refusal.Class`, and every
re-mint keeps the copier cause's class, so an importer reading either vocabulary routes the
same way.

| `copier.RefusalCause` | What the refusal says | `class` | Why | Re-minted by `Runner` as |
| --- | --- | --- | --- | --- |
| `copy-proof-empty` | The copy-and-swap target proof is the zero value ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | Only the preflight checks mint a populated proof; a zero one was constructed, not earned. | `shadow-proof-empty` |
| `copy-shadow-mismatch` | The shadow handed to the copier does not describe a copy into the proven target's shadow: it is nil or empty, built for another table, the source itself by name or by OID, missing the relation OIDs the per-chunk check compares against, or its copy columns do not carry the primary key ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | The caller paired a target with a shadow built for something else; no retry against the server changes that. | `copy-shadow-mismatch` |
| `copy-lock-unproven` | The copier was handed no table lock session, or one whose proof is empty or names a different table ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `invariant-violation` | A caller passed a lock that cannot cover this table; refused before any connection opens. | `shadow-lock-unproven` |
| `copy-lock-lost` | The table lock session reported that it lost the lock, before the copy began or while a chunk was in flight ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The interrupted chunk's transaction is rolled back and the landed watermark stands; re-acquire the lock and resume from it. The refusal also answers `errors.Is` for `dbconn.ErrTableLockLost`. | `shadow-lock-lost` |
| `copy-lock-held-elsewhere` | `pg_locks`, read from the connection about to write a chunk, shows the table lock granted to a backend other than the lock session's own ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | Another engine instance holds the table; this copy yields and the operator decides which run continues. | `shadow-lock-held-elsewhere` |
| `copy-lock-unconfirmed` | `pg_locks`, read from the connection about to write a chunk, shows no session holding the table lock, although the lock session has not reported loss ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The lock is gone from the server's point of view before the session noticed; re-acquire and resume. The refusal also answers `errors.Is` for `dbconn.ErrTableLockNotHeld`. | `shadow-lock-unconfirmed` |
| `copy-relation-replaced` | The source or the shadow is no longer the relation its proof was minted for: the name no longer resolves, resolves to another OID, or the key column the proof says is a primary key answered a boundary query with `NULL` ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `environmental` | The proof describes a table that no longer exists; drop the shadow and start the change over from preflight. | `cutover-relation-replaced` |
| `copy-frontier-broken` | The copier's own accounting of the key space broke: a chunk was cut with its lower bound above its upper, a claimed chunk does not start at the cut frontier, or the copy returned without error at a watermark short of the key space ([CO-4](invariants.md#co-4--the-copyapply-ordering-invariants)) | `invariant-violation` | The chunker produced a sequence it cannot have produced; report it, since the watermark it would have saved cannot be trusted. | `copy-frontier-broken` |
| `copy-chunks-in-flight` | The copy's ledger and its workers disagree about what landed: a worker landed a chunk the ledger had not in flight, or the copy stopped without error with chunks still in flight ([LK-3](invariants.md#lk-3--pending-work-is-claimed-exactly-once-and-wait-means-finished)) | `invariant-violation` | The ledger is the only record of what the watermark covers; a disagreement means the watermark cannot be trusted either way. | `copy-chunks-in-flight` |
| `copy-chunk-unknown` | A chunk no chunker cut — a zero `Chunk`, or one built with `NewChunk` — was fed back to the chunker as if it had been copied ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | The caller marked work the chunker never handed out; the chunker's frontier is the only authority on what was cut. | `copy-chunk-unknown` |

Not every error the copier returns is a refusal. A chunk `INSERT` the shadow itself rejects —
a `CHECK` the gated statement added that the source's rows violate (SQLSTATE `23514`), a value
the retyped column cannot hold (class `22`) — surfaces as the server's `*pgconn.PgError`,
wrapped with the chunk's bounds and reachable with `errors.As`; `RefusalCauseOf` returns the
empty cause for it, and the `Runner` passes it through unchanged. That is the copy-time face of
the same rule `BuildShadow` follows above: a change the server rejects against the data is the
server's answer, routed by SQLSTATE, not a fault in the engine's state.

### `unsupported-statement` on the create path, keyed on `CreateShapeCause`

The closed set is `executor.CreateShapeCauses()`. The cause travels with the plan statement
and reaches the verdict through `planRefusal`. In process, `executor.CreateShapeCauseOf` reads
the cause from an error and `CreateShapeCause.Class()` answers the `class` column below as a
`refusal.Class`. A plan decoded from JSON already carries the producer's class in
`Statement.Class`, which is authoritative there: `Statement.Cause.Class()` answers `""` for a
cause added in a newer engine than the importer's pinned library.

| `CreateShapeCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `partition-of` | Attaching a partition locks the partitioned parent, which the absence proof does not cover | `capability-boundary` | A proof the engine could mint but does not yet. |
| `inherits` | Binding to an existing parent is outside the absence proof | `capability-boundary` | As above. |
| `like` | Reading an existing source table is outside the absence proof | `capability-boundary` | As above. |
| `of-type` | Binding to an existing composite type is outside the absence proof | `capability-boundary` | As above. |
| `unsupported-kind` | The statement kind is outside the plain `CREATE TABLE` and `CREATE INDEX` shapes the create path runs | `capability-boundary` | A route that may be modeled later. |
| `foreign-key` | A `CREATE TABLE` that declares a foreign key would lock each referenced live table | `capability-boundary` | The same planned capability as a foreign key added to a live table, so it carries the `declarative-foreign-key` reason rather than `unsupported-statement`. |
| `if-not-exists` | A name-only no-op cannot prove the existing relation has the requested shape or is valid | `by-design` | A permanent decision with a deliberate path: declare the shape and let `diff` converge it. |
| `duplicate-name` | A desired set claims the same relation name twice | `by-design` | A permanent decision about input coherence; the fix is in the desired set. |
| `concurrently` | A table born this run has no traffic to protect, and a plain build cannot leave an invalid index behind a failure | `by-design` | A permanent, reasoned decision that names the better idiom: the plain build. |
| `multiple-operations` | The statement and operation parse boundaries disagree about the statement's operation count | `invariant-violation` | A defensive check against a state the build should not produce; nothing is missing and no environment is at fault. |

## Keep reason and class orthogonal

`class` is a separate field, not a prefix or suffix on `reason`. Existing
`outcome`/`reason`/`detail`/`safer_idiom` fields and the exit-code contract are unchanged.
The addition preserves the useful specificity and stability of existing reason tokens while
giving consumers one closed routing vocabulary.

Encoding the class into `reason` lost because it would rename every existing token, multiply
otherwise identical reasons, and make consumers parse a compound convention. Assigning a new
exit code to each class lost because exit code 2 has one valuable process-level meaning:
refused, nothing committed. Shell status is too small a surface for the reason, class, and owner
axes, and changing it would break the existing gate.

For example, an `UPDATE` backfill changes only by additive fields:

```json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "statement": "UPDATE accounts SET normalized_name = lower(name)",
  "detail": "only ALTER TABLE and CREATE INDEX statements are supported by the imperative front door"
}
```

```json
{
  "outcome": "refused",
  "reason": "unsupported-statement",
  "class": "no-online-safety-problem",
  "owner": "data-change-runner",
  "statement": "UPDATE accounts SET normalized_name = lower(name)",
  "detail": "only ALTER TABLE and CREATE INDEX statements are supported by the imperative front door"
}
```

## Name the owner, do not hide it in prose

`no-online-safety-problem` refusals also carry an optional structured `owner` field. Its
closed values are:

| Owner | Work it names |
| --- | --- |
| `data-change-runner` | Data changes and backfills owned by the application's data change runner (the tool that runs its versioned SQL or ORM changes). |
| `declarative-front-door` | Catalog bootstrap and convergence from a desired `CREATE TABLE`; `CREATE TABLE` keeps its pointer to `diff`. |
| `direct-operator` | Nobody else's tool: the statement has no online-safety problem, so whoever operates the table runs it directly, through whatever review the object warrants. This is the matrix's ⚪ half of T3; the three owners above are its 🔵 half. |
| `provisioning` | Access control and replication provisioning — grants, roles, row-level-security policies, publications, subscriptions — owned by infrastructure-as-code. |

Prose in `detail` remains: it explains the concrete command or constraint to a human. Prose
alone lost because machine routing is the purpose of this contract; parsing a sentence would
recreate the problem that `class` solves. `owner` is absent outside
`no-online-safety-problem` and required for every refusal in that class. `direct-operator` is
a named value rather than an absent one so that the ⚪ and 🔵 halves stay legible apart in
the JSON: a consumer that hands 🔵 refusals to another tool must not hand ⚪ refusals to
nobody.

## Make an unclassified refusal unrepresentable

The implementation follows the repository's proof-type idiom: unexported fields and a
validating constructor make a valid refusal the only value downstream renderers can receive.
Refusal constructors take a non-zero class (and require a valid owner exactly for
`no-online-safety-problem`) rather than constructing a `Verdict` and filling fields later.
`pkg/verdict` implements it as the `Refusal` proof type: `NewRefusal(class, reason, owner)`
rejects the zero or an unknown class, an unknown reason, and an owner outside
`no-online-safety-problem`; the per-class constructors (`CapabilityBoundary`,
`NoOnlineSafetyProblem`, `ByDesign`, `Environmental`, `InvariantViolation`) are total for
valid inputs; and `Verdict.WithRefusal` is the only way a verdict acquires its
`outcome: refused`, `reason`, `class`, and `owner` together.

A registry names every refusal key — a typed cause where one exists, a site where none does —
together with its reason, class, and optional owner. A completeness test derives the keys
from production (`verdict.Reasons()`, `executor.CreateShapeCauses()`,
`preflight.PartitionRefusalCauses()`, the admission sentinel sets, and a walk of the remaining
refusal sites) and fails if a key is absent, carries the zero class, or violates the owner
rule. The registry has two halves: `pkg/plan/refusal.go` classifies the keys that travel with
a planned statement (`CreateShapeRefusal`, `PartitionRefusal`, `RouteRefusal`,
`DestructiveChangeRefusal`, `RowSecurityReviewRefusal`, `RowSecurityMissingTableRefusal`), and
`pkg/migrate/refusal_registry.go` classifies statement kinds at the gate, the admission
sentinel sets, and the imperative sites; `TestRefusalRegistryIsComplete` in `pkg/migrate`
covers both halves. Keying on causes is what gives the test correspondence rather than presence: a registry
keyed on sites alone would go green with `admissionRefusalVerdict` classified
`capability-boundary` while it minted a `by-design` refusal for every `CREATE ... IF NOT
EXISTS`.

RLS preview uses the plan registry's `capability-boundary` refusal for review-only
policy deltas. A missing target uses the shared `RowSecurityMissingTableRefusal`
in both preview and apply, with class `environmental`. Atomic RLS execution uses `migrate.RowSecurityRefusal`:
missing owner or database CREATE privileges (including PostgreSQL SQLSTATE 42501)
carry `insufficient-privileges` / `environmental`; an absent target table carries
`unsupported-statement` / `environmental`; unsupported declarations or target
shapes carry `unsupported-statement` / `capability-boundary`. Operational failures
remain failures. Typed privilege causes take precedence over the executor's general
admission sentinel. The registry tests cover these mappings and refusal classes.

The test also pins a sentinel set of keys it must find — at least one cause from each closed
set and the `KindOther` catch-all — so that a change to how refusal verdicts are constructed
cannot make the deriver find zero sites and pass vacuously on an empty set. This is the same
house pattern as the proof types in
[the TCB model](tcb-model.md#make-illegal-states-unrepresentable) and the derived proof-type
registry completeness harness in `internal/safety`, whose `sentinelProofTypes` exists for the
same reason: do not maintain a test-only shadow list that can drift from production, and do
not let the derivation's own failure look like success.

The map in this document is pinned: the `docs_test.go` guards
in `pkg/verdict`, `pkg/executor`, `pkg/preflight`, `pkg/schemachange`, and `pkg/copier` fail
when a `Reason`, `CreateShapeCause`, `PartitionRefusalCause`, `CopySwapRefusalCause`, or either
package's `RefusalCause` exists in the code without a row here, so a new discriminator value
cannot land unclassified. The shadow, copier, preflight, and create-path guards go one step
further and compare each cause's `Class()` to its row's `class` cell, so the two cannot
disagree, and each asserts that every class a cause answers is a member of
`refusal.Classes()` — every typed refusal cause the core mints (`copier.RefusalCause`,
`schemachange.RefusalCause`, `preflight.PartitionRefusalCause`,
`preflight.CopySwapRefusalCause`, `executor.CreateShapeCause`) answers a `Class()` held this
way, and the guards read the cause tables through one shared reader
(`internal/testutil`), so each table is located and parsed the same way in every package;
`pkg/refusal`'s guards hold its classes to the vocabulary table above and every `class`
cell of every cause table in this document to its closed set, so a cause table cannot
classify a cause under a class the engine cannot name; `pkg/verdict`'s holds every one of
them to be a `verdict.Class`, the verdict adding only `no-online-safety-problem`; and
`pkg/plan`'s holds the registry's class for each create-shape and partition cause to that
cause's own `Class()`, so the registry's explicit classification and the cause's cannot
drift apart.

## Shared vocabulary with the capabilities matrix

The [capabilities matrix](capabilities.md) tiers every operation and marks each row, and the
[capabilities contract](capabilities-contract.md) defines its machine-readable form. Tier
and mark map to refusal class as follows; the contract's
[shared refusal vocabulary](capabilities-contract.md#shared-refusal-vocabulary) reuses these
words rather than defining its own:

| Matrix tier or mark | Refusal class |
| --- | --- |
| T2 — planned | `capability-boundary` |
| T3 ⚪ — no online-safety problem (`owner: direct-operator`), or T3 🔵 — another tool class owns it (any other owner) | `no-online-safety-problem` |
| T3 ❌ — no online mechanism / deliberately refused form | `by-design` |
| T1 — supported today | No capability refusal; a run may still be `environmental` |
| — | `invariant-violation` has no tier: it reports a defect in pg-sprite, not a property of the operation |

The matrix, its contract, and this document define one vocabulary and must change together.
The matrix describes the operation independent of a run; the verdict reports how one run met
that contract. The cause tables above are the decision; the matrix mark for the `NOT VALID` foreign key
on a partitioned parent was corrected to agree with them.

## Compatibility and rollout

This is an additive JSON change. Consumers that ignore unknown fields are unaffected;
consumers that understand `class` stop maintaining their own reason buckets. Text renderers
show the class and, when present, owner. Exit code 2 retains its meaning, and every existing
reason string remains byte-for-byte unchanged. `demo/tour.sh` asserts the class on the JSON
surface alongside the reason and cause.

The work landed in three steps:

1. This contract, with the docs guards that pin its cause tables to the code. *(done)*
2. Engine classification, CLI surfaces, and the documentation sweep as one change. The
   capability-statement sync rule moved `capabilities.md`, `limitations.md`, the root README,
   and demo assertions together: `limitations.md` no longer claims every refusal means an
   online-safety guarantee cannot be provided, the root README says some refusals mean there
   is no online-safety problem here, and the matrix mark for the `NOT VALID` foreign key on a
   partitioned parent was corrected. This step added
   [RF-7](invariants.md#refusals-and-preflight-rf) — every refusal carries a non-zero class,
   and `owner` is present exactly for `no-online-safety-problem` — because the registry
   describes shipped behavior, and the constructor and completeness test enforce it. RF-5 and
   RF-6 are unchanged and are cited above because the map must agree with them. *(done)*
3. Make the replay corpus assert the engine-emitted class — and, where RF-7 requires one, the
   owner — instead of curating its own classification. Only the three operation-scoped
   classes are pinnable; `environmental` and `invariant-violation` describe the run and are a
   mismatch by definition. *(done)*

Non-goals are changing exit codes, changing any existing reason string, implementing a missing
backend, or changing the capability tier of an operation (the one matrix-mark correction above
is a fix to a mark that already disagrees with `limitations.md`, not a re-tiering).
