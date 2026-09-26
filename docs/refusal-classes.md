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
| `by-design` | pg-sprite permanently refuses this form — because PostgreSQL offers no online mechanism in any supported version, or because a safer idiom exists; the verdict names the idiom or the deliberate path. This is [RF-5](invariants.md#refusals-and-preflight-rf) seen from the consumer's side. | Use the safer idiom. For a single-relation plain `DROP INDEX`, `REINDEX INDEX`, or `REINDEX TABLE`, `migrate --accept-blocking SCHEMA.TABLE` runs the blocking form under bounded budgets and exits 3 ([lock-budgeted-passthrough.md](lock-budgeted-passthrough.md)); otherwise run it outside pg-sprite in a maintenance window. |
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
| `index-statement` | Plain `DROP INDEX` or `REINDEX`, for which the verdict names the concurrent form | `by-design` | — |
| `index-statement` | An already-concurrent maintenance statement that pg-sprite need not wrap | `no-online-safety-problem` | `direct-operator` |
| `not-native-safe-table-too-large` | The size policy prevents a blind bounded attempt | `environmental` | — |
| `insufficient-privileges` | The connected role lacks a required grant | `environmental` | — |
| `not-native-safe-budget-exceeded` | The lock or statement budget cancels an attempt | `environmental` | — |
| `not-native-safe-rewrite-required` | The planner cannot construct the required safer sequence | `capability-boundary` | — |
| `backend-unavailable` | The plan routes to an execution backend this build lacks | `capability-boundary` | — |
| `destructive-change` | Desired-state execution will not infer permission to discard live structure | `by-design` | — |
| `plan-fingerprint-mismatch` | The recomputed plan is not the reviewed plan | `environmental` | — |
| `create-collision` | A target or claimed name is occupied at apply time by a relation or a standalone type | `environmental` | — |

### `unsupported-partitioned-parent`, keyed on `PartitionRefusalCause`

The four causes span three classes. [RF-6](invariants.md#refusals-and-preflight-rf) already
keeps the not-implemented case and the version-gated case apart in one sentence; this table
agrees with it rather than collapsing them.

| `PartitionRefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `parent-concurrent-index-build` | The `CREATE INDEX ON ONLY` → per-partition `CONCURRENTLY` → `ATTACH PARTITION` flow is not yet implemented | `capability-boundary` | A planned engine capability; the matrix marks it 🟡. |
| `parent-blocking-index-build` | pg-sprite will not substitute a blocking parent build for the missing partition-aware flow | `capability-boundary` | Same missing flow; the refusal of the blocking substitute is the policy half of the same gap. The eligibility registry marks this cause acceptable, but `migrate --accept-blocking` does not reach it yet: the unforced imperative path refuses the parent build with the concurrent cause, and the flag rejects `--force`. |
| `parent-index-adoption` | PostgreSQL does not support adopting an existing index as a constraint on a partitioned parent in any supported version | `by-design` | No online mechanism exists; the matrix marks it ❌. Waiting for a pg-sprite release would wait for nothing. |
| `parent-not-valid-foreign-key` | PostgreSQL before version 18 cannot add a `NOT VALID` foreign key on a partitioned table | `environmental` | The same statement, table, and pg-sprite build runs on a newer server; the action that unblocks it is a server upgrade, not an engine release. The matrix marks this row ✅ with a server-version precondition. |

### Copy-and-swap shape refusals, keyed on `CopySwapRefusalCause`

The closed set is `preflight.CopySwapRefusalCauses()`, raised by `CheckCopySwapShape` before
the copy-and-swap route writes anything ([ST-6](invariants.md#st-6--preflight-before-the-first-write),
[RF-1](invariants.md#refusals-and-preflight-rf), [RF-2](invariants.md#refusals-and-preflight-rf)).
The verdict reason that carries these causes lands with the copy-and-swap route itself; the
classification is fixed here first so the route inherits it.

| `CopySwapRefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `copy-and-swap-pk-unsupported` | The table has no single `smallint`, `integer`, or `bigint` primary-key column for the chunker to range over | `capability-boundary` | Wider key shapes are a planned engine capability ([D4](copy-and-swap-design.md#d4--restrict-the-chunk-key-to-one-integer-family-primary-key)). |
| `copy-and-swap-replica-identity` | The table's replica identity is `NOTHING` or a named index; `DEFAULT` or `FULL` is required | `environmental` | The same table is admitted after `ALTER TABLE … REPLICA IDENTITY DEFAULT` or `FULL`; the action that unblocks it is a catalog change, not an engine release. |
| `copy-and-swap-foreign-keys` | A foreign key references the table or leaves it | `capability-boundary` | An OID-bound dependent the rename swap would strand on the old table; re-pointing it is a planned capability. |
| `copy-and-swap-triggers` | The table has a user trigger or a rewrite rule | `capability-boundary` | As above. |
| `copy-and-swap-partitioned` | The table is a partitioned parent, a partition, or part of an inheritance tree | `capability-boundary` | The per-partition copy-and-swap flow is a planned capability. |
| `copy-and-swap-unlogged` | The table is UNLOGGED, while the shadow would be permanent | `capability-boundary` | Preserving persistence requires an explicit shadow-creation path. |
| `copy-and-swap-force-rls` | The table has `FORCE ROW LEVEL SECURITY`, so the owner-run copier would be filtered reading the source and rejected filling the policy-carrying shadow | `capability-boundary` | Copying under a `BYPASSRLS` role or deferring the policies to cutover is a planned capability; either needs a decision the engine has not made. |

### Shadow operation refusals, keyed on `RefusalCause`

The closed set is `schemachange.RefusalCauses()`, carried by the `*schemachange.RefusalError`
that `BuildShadow`, `InspectShadow`, and `DropShadow` return; `schemachange.RefusalCauseOf`
reads it through wrapping. Every one of these refusals is fail-closed under
`ErrInvariantViolation` ([SAFETY.md](../SAFETY.md#rules-inside-the-core)): the sentinel is the
mechanism that stops the operation, and the cause is what tells the importer which way to
react. Only the causes that mean the caller handed the engine an incoherent input, or that the
engine's own write did not take, are `invariant-violation`; the rest describe a lock, a proof,
or a relation whose state moved under the operation, and the importer retries, re-plans, or
cleans up. No verdict reason carries these causes yet; the classification is fixed here first
so the cutover route inherits it.

| `RefusalCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `shadow-lock-unproven` | The operation was handed no table lock session, or one whose proof is empty or names a different table ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `invariant-violation` | A caller passed a lock that cannot cover this table; no retry against the server changes that. |
| `shadow-lock-lost` | The table lock session reported that it lost the lock, before or during the operation ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The operation's transaction is aborted and nothing is written; re-acquire the lock and repeat. |
| `shadow-lock-held-elsewhere` | `pg_locks` shows the table lock granted to a backend other than the lock session's own ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | Another engine instance holds the table; this one yields and the operator decides which run continues. |
| `shadow-lock-unconfirmed` | `pg_locks` shows no session holding the table lock, although the lock session has not reported loss ([LK-1](invariants.md#lk-1--at-most-one-migration-runs-per-table)) | `environmental` | The lock is gone from the server's point of view before the session noticed; re-acquire and repeat. |
| `shadow-proof-empty` | The copy-and-swap target proof is the zero value ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `invariant-violation` | Only the shape check mints a populated proof; a zero one was constructed, not earned. |
| `shadow-source-shape` | The source's catalog is outside the shape the proof admits: an identity column without an internally owned sequence, or a replica identity other than `DEFAULT` or `FULL` ([ST-6](invariants.md#st-6--preflight-before-the-first-write)) | `environmental` | The catalog changed after preflight admitted the table; re-run preflight against the table as it is now. |
| `shadow-statement-target` | The gated statement is not an `ALTER TABLE` on the proven table, or its retargeted form does not name the shadow with the same operations ([ST-7](invariants.md#st-7--the-executor-runs-exactly-the-statement-that-was-gated)) | `invariant-violation` | The caller gated one statement and handed the build another; the pairing is the caller's, not the server's. |
| `shadow-owner-mismatch` | The shadow the build created is not owned by the source's owner ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The owner the proof carries is no longer the owner the catalog reports; the shadow is dropped and preflight re-run. |
| `shadow-foreign-relation` | The relation wearing the shadow's name is not a plain table owned by the source's owner ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The engine did not build it and will not touch it; an operator removes or renames the relation. |
| `shadow-grants-differ` | The shadow's grants still differ from the source's after the build synchronised them ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `invariant-violation` | The build wrote the grants under the table lock and read back something else; that is the engine's write, not the environment. |
| `shadow-identity-handoff` | A source identity column the change kept on the shadow does not carry `DEFAULT nextval(<source sequence>)` there, or the shadow declares an identity of its own ([ST-5](invariants.md#st-5--the-swap-is-gated-on-a-fidelity-checklist-not-just-the-checksum)) | `environmental` | The change itself, or a later edit to the shadow, replaced the handoff; change the statement or drop the shadow and rebuild. |

### `unsupported-statement` on the create path, keyed on `CreateShapeCause`

The closed set is `executor.CreateShapeCauses()`. The cause travels with the plan statement
and reaches the verdict through `planRefusal`.

| `CreateShapeCause` | What the refusal says | `class` | Why |
| --- | --- | --- | --- |
| `partition-of` | Attaching a partition locks the partitioned parent, which the absence proof does not cover | `capability-boundary` | A proof the engine could mint but does not yet. |
| `inherits` | Binding to an existing parent is outside the absence proof | `capability-boundary` | As above. |
| `like` | Reading an existing source table is outside the absence proof | `capability-boundary` | As above. |
| `of-type` | Binding to an existing composite type is outside the absence proof | `capability-boundary` | As above. |
| `unsupported-kind` | The statement kind is outside the plain `CREATE TABLE` and `CREATE INDEX` shapes the create path runs | `capability-boundary` | A route that may be modeled later. |
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
in `pkg/verdict`, `pkg/executor`, `pkg/preflight`, and `pkg/schemachange` fail when a `Reason`,
`CreateShapeCause`, `PartitionRefusalCause`, `CopySwapRefusalCause`, or `RefusalCause` exists
in the code without a row here, so a new discriminator value cannot land unclassified.

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
