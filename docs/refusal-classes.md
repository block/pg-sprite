# Refusal classes: a routing contract above reasons

**Decision: every refusal carries a machine-readable `class` that tells a consumer what
kind of boundary it reached.** `reason` continues to identify the immediate cause; `class`
answers the next question: wait for engine capability, hand the work to its owner, use the
named safer idiom, or change the run environment.

Every refused statement has an `outcome`, a typed `reason`, a typed `class`, explanatory `detail`, and,
where one exists, a `safer_idiom`. Exit code 2 means nothing ran. That is enough to explain a
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
| `by-design` | pg-sprite permanently refuses this form — because PostgreSQL offers no online mechanism in any supported version, or because a safer idiom exists; the verdict names the idiom or the deliberate path. This is [RF-5](invariants.md#refusals-and-preflight-rf) seen from the consumer's side. | Use the safer idiom, or run the blocking form outside pg-sprite in a maintenance window. |
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
| `parent-blocking-index-build` | pg-sprite will not substitute a blocking parent build for the missing partition-aware flow | `capability-boundary` | Same missing flow; the refusal of the blocking substitute is the policy half of the same gap. |
| `parent-index-adoption` | PostgreSQL does not support adopting an existing index as a constraint on a partitioned parent in any supported version | `by-design` | No online mechanism exists; the matrix marks it ❌. Waiting for a pg-sprite release would wait for nothing. |
| `parent-not-valid-foreign-key` | PostgreSQL before version 18 cannot add a `NOT VALID` foreign key on a partitioned table | `environmental` | The same statement, table, and pg-sprite build runs on a newer server; the action that unblocks it is a server upgrade, not an engine release. The matrix marks this row ✅ with a server-version precondition. |

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
refused, nothing ran. Shell status is too small a surface for the reason, class, and owner
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
a planned statement (`CreateShapeRefusal`, `PartitionRefusal`, `RouteRefusal`), and
`pkg/migrate/refusal_registry.go` classifies statement kinds at the gate, the admission
sentinel sets, and the imperative sites; `TestRefusalRegistryIsComplete` in `pkg/migrate`
covers both halves. Keying on causes is what gives the test correspondence rather than presence: a registry
keyed on sites alone would go green with `admissionRefusalVerdict` classified
`capability-boundary` while it minted a `by-design` refusal for every `CREATE ... IF NOT
EXISTS`.

The test also pins a sentinel set of keys it must find — at least one cause from each closed
set and the `KindOther` catch-all — so that a change to how refusal verdicts are constructed
cannot make the deriver find zero sites and pass vacuously on an empty set. This is the same
house pattern as the proof types in
[the TCB model](tcb-model.md#make-illegal-states-unrepresentable) and the derived proof-type
registry completeness harness in `internal/safety`, whose `sentinelProofTypes` exists for the
same reason: do not maintain a test-only shadow list that can drift from production, and do
not let the derivation's own failure look like success.

The map in this document is pinned: the `docs_test.go` guards
in `pkg/verdict`, `pkg/executor`, and `pkg/preflight` fail when a `Reason`,
`CreateShapeCause`, or `PartitionRefusalCause` exists in the code without a row here, so a
new discriminator value cannot land unclassified.

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
3. Make the replay corpus assert the engine-emitted class instead of curating its own
   classification. *(done)*

Non-goals are changing exit codes, changing any existing reason string, implementing a missing
backend, or changing the capability tier of an operation (the one matrix-mark correction above
is a fix to a mark that already disagrees with `limitations.md`, not a re-tiering).
