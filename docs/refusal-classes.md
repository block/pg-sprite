# Refusal classes: a routing contract above reasons

**Decision: every refusal carries a machine-readable `class` that tells a consumer what
kind of boundary it reached.** `reason` continues to identify the immediate cause; `class`
answers the next question: wait for engine capability, hand the work to its owner, use the
named safer idiom, or change the run environment.

Today every refused statement has an `outcome`, a typed `reason`, explanatory `detail`, and,
where one exists, a `safer_idiom`. Exit code 2 means nothing ran. That is enough to explain a
single refusal, but not enough to route it: `unsupported-statement` currently covers a data
backfill, an imperative `CREATE TABLE`, a permanently unsafe `CREATE INDEX IF NOT EXISTS`,
and an admitted `ALTER TABLE` operation for which the planner has no route. Those are not
the same kind of answer.

## The decided vocabulary

The refusal verdict adds `class`, with exactly four values:

| Class | Meaning | Consumer action |
| --- | --- | --- |
| `capability-boundary` | The engine cannot do this yet, or has no implemented safe route. This is a T2 capability. | Wait for the capability or escalate. |
| `no-online-safety-problem` | There is nothing for an online schema-change engine to make safe; another tool class owns the work. | Hand the statement to the named owner. |
| `by-design` | pg-sprite permanently refuses this form; the verdict names a safer idiom or deliberate path. | Use the safer idiom. |
| `environmental` | The change is supportable, but not here, now, or as this role: a size policy, exhausted budget, privilege, stale plan, or catalog collision stopped it. | Retry, provision, re-plan, or escalate. |

`environmental` has no capabilities-matrix tier. It describes the run site, not the
operation. A supported operation can produce it without changing tier.

```text
┌──────────────────────────┬──────────────────────────────┐
│ class                    │ consumer route               │
├──────────────────────────┼──────────────────────────────┤
│ capability-boundary      │ wait / escalate              │
│ no-online-safety-problem │ hand to owner tool           │
│ by-design                │ use safer idiom              │
│ environmental            │ retry / provision / escalate │
└──────────────────────────┴──────────────────────────────┘
```

## The current reason-to-class map

The closed reason set is `verdict.Reasons()`. The table below is the classification of every
current emission site, including desired-state admission. A reason with several rows is
deliberately not a class.

| Existing `reason` | Refusal site or shape | `class` | `owner`, when present |
| --- | --- | --- | --- |
| `unsupported-statement` | DML such as an `UPDATE` backfill, or another non-DDL owner operation | `no-online-safety-problem` | `data-change-runner` |
| `unsupported-statement` | Imperative `CREATE TABLE`; the detail already points to `diff` | `no-online-safety-problem` | `declarative-front-door` |
| `unsupported-statement` | An admitted ALTER operation for which the planner has no route; an unnamed index; an unsupported sequence step; a greenfield create shape that needs a future modeled route | `capability-boundary` | — |
| `unsupported-statement` | `CREATE INDEX IF NOT EXISTS`, or a duplicate claimed relation name in one desired set | `by-design` | — |
| `index-statement` | Plain `DROP INDEX` or `REINDEX`, for which the verdict names the concurrent form | `by-design` | — |
| `index-statement` | An already-concurrent maintenance statement that pg-sprite need not wrap | `no-online-safety-problem` | `owner-tooling` |
| `not-native-safe-table-too-large` | The size policy prevents a blind bounded attempt | `environmental` | — |
| `insufficient-privileges` | The connected role lacks a required grant | `environmental` | — |
| `unsupported-partitioned-parent` | The partition-aware online sequence is not implemented | `capability-boundary` | — |
| `not-native-safe-budget-exceeded` | The lock or statement budget cancels an attempt | `environmental` | — |
| `not-native-safe-rewrite-required` | The planner cannot construct the required safer sequence | `capability-boundary` | — |
| `backend-unavailable` | The plan routes to an execution backend this build lacks | `capability-boundary` | — |
| `destructive-change` | Desired-state execution will not infer permission to discard live structure | `by-design` | — |
| `plan-fingerprint-mismatch` | The recomputed plan is not the reviewed plan | `environmental` | — |
| `create-collision` | A target or claimed relation name is occupied at apply time | `environmental` | — |

Both `unsupported-statement` and `index-statement` span multiple classes. Therefore **the
class is assigned at each refusal site and is never derived from the reason**. The same rule
applies when a broad reason gains a new site: the author must classify the site explicitly.

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
  "detail": "data backfills belong to the application's data change runner; pg-sprite changes table shape"
}
```

## Name the owner, do not hide it in prose

`no-online-safety-problem` refusals also carry an optional structured `owner` field. Its
closed values are:

| Owner | Work it names |
| --- | --- |
| `data-change-runner` | Data changes and backfills owned by the application's data change runner (the tool that runs its versioned SQL or ORM changes). |
| `declarative-front-door` | Catalog bootstrap and convergence from a desired `CREATE TABLE`; `CREATE TABLE` keeps its pointer to `diff`. |
| `owner-tooling` | Safe direct maintenance and catalog operations owned by the table or database operator's tooling. |

Prose in `detail` remains: it explains the concrete command or constraint to a human. Prose
alone lost because machine routing is the purpose of this contract; parsing a sentence would
recreate the problem that `class` solves. `owner` is absent outside
`no-online-safety-problem` and required for every refusal in that class.

## Make an unclassified refusal unrepresentable

The implementation follows the repository's proof-type idiom: unexported fields and a
validating constructor make a valid refusal the only value downstream renderers can receive.
Refusal constructors take a non-zero class (and require a valid owner exactly for
`no-online-safety-problem`) rather than constructing a `Verdict` and filling fields later.
In outline, not as the final API:

```go
type RefusalClass string

type Refusal struct {
	class  RefusalClass
	reason Reason
	owner  Owner
}

func NewRefusal(class RefusalClass, reason Reason, owner Owner) (Refusal, error) {
	// Reject zero/unknown class and invalid class-owner combinations.
	return Refusal{class: class, reason: reason, owner: owner}, nil
}
```

A registry names every refusal site together with its reason, class, and optional owner. A
completeness test derives the production refusal sites and fails if a site is absent, carries
the zero class, or violates the owner rule. This is the same house pattern as the proof types
in [the TCB model](tcb-model.md#make-illegal-states-unrepresentable) and the derived
proof-type registry completeness harness in `internal/safety`: do not maintain a test-only
shadow list that can drift from production.

## Shared vocabulary with the capabilities matrix

The sibling [capabilities contract](capabilities-contract.md) defines the machine-readable
matrix. Its tier and mark map to refusal class as follows:

| Matrix tier or mark | Refusal class |
| --- | --- |
| T2 — planned | `capability-boundary` |
| T3 ⚪ — no online-safety problem, or T3 🔵 — another tool class owns it | `no-online-safety-problem` |
| T3 ❌ — no online mechanism / deliberately refused form | `by-design` |
| T1 — supported today | No capability refusal; a run may still be `environmental` |

These two documents define one vocabulary and must change together. The matrix describes the
operation independent of a run; the verdict reports how one run met that contract.

## Compatibility and rollout

This is an additive JSON change. Consumers that ignore unknown fields are unaffected;
consumers that understand `class` stop maintaining their own reason buckets. Text renderers
show the class and, when present, owner. Exit code 2 retains its meaning, and every existing
reason string remains byte-for-byte unchanged. When the field ships, `demo/tour.sh` assertions
are updated with the JSON surface.

Sequence the work in three steps:

1. Land this contract first.
2. Add engine classification, CLI surfaces, and the documentation sweep as one change. The
   capability-statement sync rule moves `capabilities.md`, `limitations.md`, the root README,
   and demo assertions together. In particular, that sweep updates `limitations.md`'s opening
   claim that all refusals mean an online-safety guarantee cannot be provided, and the root
   README's refusal and “What pg-sprite does not do yet” descriptions: some refusals instead
   mean there is no online-safety problem here.
3. Make the replay corpus assert the engine-emitted class instead of curating its own
   classification.

Non-goals are changing exit codes, changing any existing reason string, implementing a missing
backend, or changing the capability tier of an operation.
