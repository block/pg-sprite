# Lock-budgeted passthrough

**Decision: pg-sprite will add `--accept-blocking` as an explicit, imperative-front-door
acknowledgement that a specifically eligible refused schema change may run without an
online-safety guarantee, but only after normal refusal analysis and only in an engine-owned
session with required `lock_timeout` and `statement_timeout` bounds.** A successful run has
outcome `executed-without-online-safety` and exit code 3; it never borrows the online-safe
`executed-natively` outcome or exit code 0.

This is deliberately narrower than arbitrary SQL passthrough. The engine must understand the
statement, identify the exact typed refusal, decide that a lock budget materially improves its
failure mode, and retain that refusal identity after execution. The feature provides a safer
front door for an operator who has already decided to accept blocking behavior. It does not
make the behavior online-safe.

## Table of contents

- [The contract to preserve](#the-contract-to-preserve)
- [Decision flow](#decision-flow)
- [Pinned acceptance criteria](#pinned-acceptance-criteria)
- [Flag and front door](#flag-and-front-door)
- [Typed eligibility](#typed-eligibility)
- [Engine-owned session and budgets](#engine-owned-session-and-budgets)
- [Verdict, reports, and exit code](#verdict-reports-and-exit-code)
- [Failure and interruption semantics](#failure-and-interruption-semantics)
- [`--force` interaction](#--force-interaction)
- [Orchestrator exposure](#orchestrator-exposure)
- [Alternatives considered](#alternatives-considered)
- [Compatibility and rollout](#compatibility-and-rollout)
- [Non-goals](#non-goals)

## The contract to preserve

The current [capabilities matrix](capabilities.md#why-typed-refusal-not-passthrough) makes
refusal valuable: the engine says what it cannot vouch for instead of silently falling back
to a blocking form. The root README reserves exit code 0 for a schema change that ran through
an online-safe path. Exit code 2 means refused and nothing ran; exit code 1 means execution
failed. Those meanings remain useful and remain unchanged.

The new path does not weaken that contract. It splits “the engine refused to call this
online-safe” from “the operator may execute this known form under bounded lock acquisition.”
The former analysis always happens first. The latter is a separate, loud decision with a
separate outcome and process status.

A bounded `lock_timeout` is the concrete improvement over copying the statement into raw
`psql`. An `ACCESS EXCLUSIVE` request that cannot acquire its lock inside the budget aborts
instead of waiting at the head of PostgreSQL's lock queue and causing every later reader to
queue behind it. The budget limits lock acquisition; it does not make a lock harmless after
it is acquired.

## Decision flow

```text
┌───────────────┐
│ statement     │
└───────┬───────┘
        ▼
┌───────────────┐
│ normal gate   │
└───────┬───────┘
        ▼
┌────────────────────────────┐
│ refusal (reason/class/cause)│
└──────────────┬─────────────┘
               ▼
        ┌─────────────┐  no   ┌────────────────────┐
        │ eligible?   │──────▶│ refused, exit 2    │
        └──────┬──────┘       └────────────────────┘
               │ yes
               ▼
        ┌─────────────┐  no   ┌────────────────────┐
        │ flag present│──────▶│ refused, exit 2    │
        └──────┬──────┘       └────────────────────┘
               │ yes
               ▼
┌─────────────────────────────────┐  mismatch  ┌────────────────────┐
│ resolve the locked table        │───────────▶│ usage error, no run│
│ (index → pg_index.indrelid)     │            └────────────────────┘
└──────────────┬──────────────────┘
               │ matches flag value
               ▼
┌─────────────────────────────────┐
│ engine-owned bounded session    │
│ lock_timeout + statement_timeout│
└──────────────┬──────────────────┘
               ▼
┌──────────────────────────────────────────────┐
│ marked executed verdict, exit 3              │
│ or typed refusal/failure, exit 2/1           │
└──────────────────────────────────────────────┘
```

## Pinned acceptance criteria

| # | Criterion |
| --- | --- |
| 1 | `--accept-blocking` is a dedicated flag. `--force` continues not to bypass policy refusals. |
| 2 | Both the imperative `migrate` and declarative `diff` planning paths perform normal classification. The refusal's reason, class, cause, detail, and safer idiom remain visible before any eligible execution. |
| 3 | Every executed statement uses an engine-owned session with a non-zero `lock_timeout` and a non-zero `statement_timeout` the operator supplied explicitly on that invocation; no caller-owned, defaulted, or unbounded session is eligible. |
| 4 | Success is `executed-without-online-safety` in text and JSON and exits 3. `executed-natively` and exit 0 remain exclusive to online-safe paths. |
| 5 | Interruption makes no resume promise. The verdict and documentation describe the state PostgreSQL left rather than manufacturing a checkpoint. |
| 6 | Eligibility is selected from a closed registry keyed by typed class, reason, and cause or refusal site. No renderer text or SQL substring participates in the decision. |
| 7 | A dry-run reports the original refusal and a per-statement `blocking_passthrough_eligible` boolean, but executes nothing even when the flag is present. |

The full typed refusal proof is available while its verdict remains in process, and
`Refusal()` returns it only when it validates as a classified refusal and still matches the
verdict's exported fields. JSON carries class, reason, owner, and cause, but not the internal
refusal site. Calling `Refusal()` on a decoded verdict therefore reconstructs the JSON fields
but cannot restore the site: the site-keyed row (single-relation `index-statement`) fails
closed after decoding, while the cause-keyed row (`unsupported-partitioned-parent` with
`parent-blocking-index-build`) is decidable from the wire fields. Decoding is not the
fail-closed boundary; the front door is. Eligibility is consumed only from the proof of the
verdict the same `migrate` or `diff` invocation produced, never from a verdict read back
from JSON.

“Prints before execution” is an ordering requirement for human output and an information
requirement for JSON. Human mode prints the refusal analysis, then a separate acceptance line,
then starts the session. JSON remains one final machine-readable object; its executed verdict
retains the original refusal fields, so no streaming JSON preamble is needed. An unconditional
audit event records the accepted refusal before execution, as the current `--force` audit does.

## Flag and front door

The final name is `--accept-blocking`. It says what the operator accepts, not what the engine
claims. “Blocking” is intentionally stronger and clearer than “unsafe”: the eligible cases
request locks that can make a live table unavailable, and the flag must remain uncomfortable
to type.

Like `--force`, the flag requires the resolved schema-qualified table as its value rather than
a bare boolean:

```sh
pg-sprite migrate \
  --alter 'DROP INDEX app.orders_created_at_idx' \
  --accept-blocking app.orders \
  --lock-timeout 3s \
  --statement-timeout 10m
```

The value names the table whose lock the operator accepts, because that is the relation the
blocking form makes unavailable: a plain `DROP INDEX` takes `ACCESS EXCLUSIVE` on the index's
table, and a parent index build locks the parent. The partitioned-parent statements and
`REINDEX TABLE` name that table themselves. `DROP INDEX` and `REINDEX INDEX` name only the
index, and the imperative front door's parse-only target resolution knows no table for those
kinds, so the acknowledgement needs one catalog lookup the current flow does not make: resolve
the index name against `search_path`, then read its owning table from `pg_index.indrelid`.

That lookup runs after the refusal is produced and found eligible and only when the flag is
present, so the statement-kind gate stays parse-only and a refusal is produced exactly where it
is produced today. The sequence is: gate refuses, registry says eligible, flag present, dial,
resolve the accepted table, compare it with the flag's value, then execute. A mismatch or an
index that no longer exists is a usage error and nothing runs. The value prevents a copied
command from silently accepting a different relation's lock.

Eligibility itself never depends on that lookup. The registry keys row 1 on the refusal site,
and the site distinguishes the single-relation forms — one `DROP INDEX`, `REINDEX INDEX`,
`REINDEX TABLE` — from the forms that name several relations or none: `DROP INDEX a, b`,
`REINDEX SCHEMA`, `REINDEX DATABASE`, and `REINDEX SYSTEM`. Those multi-relation forms are
ineligible in v1 because one flag value cannot acknowledge several tables' locks.

Execution is exposed only by the imperative `migrate` front door. Declarative `diff` still
classifies and reports eligibility, but it cannot accept the flag or execute this path. A
desired-state plan may contain inferred destructive work or several statements; accepting a
single blocking statement must instead be explicit SQL at `migrate`.

Rejected names:

| Name | Why it lost |
| --- | --- |
| `--apply-anyway` | Says nothing about the accepted availability risk and sounds like a universal bypass. |
| `--unsafe` | Labels risk but not the concrete blocking behavior, and is easily mistaken for disabling all checks. |
| `--passthrough` | Describes implementation, suggests arbitrary unclassified SQL, and hides the engine-owned budget benefit. |
| `--force-blocking` | Incorrectly presents the feature as a stronger form of `--force`; the two acknowledgements govern different decisions. |

## Typed eligibility

Eligibility is not equal to “class is `by-design`” or “class is
`capability-boundary`.” Those classes route consumers; they are wider than this execution
policy. A closed registry keys each eligible entry by the refusal class plus its typed reason
and, where a reason spans meanings, its cause or refusal site. New reasons and causes default
to ineligible until deliberately classified and tested.

The v1 set is:

| Class | Typed refusal shape | v1 | Rationale |
| --- | --- | --- | --- |
| `by-design` | `index-statement` at the plain single-relation `DROP INDEX`, `REINDEX INDEX`, or `REINDEX TABLE` site, with the concurrent safer idiom | eligible | This is the core maintenance-window case: the submitted form is understood, the safer idiom is known, one table's lock is being accepted, and bounding `ACCESS EXCLUSIVE` acquisition prevents lock-queue pile-up. The multi-relation sites (`DROP INDEX a, b`, `REINDEX SCHEMA`, `DATABASE`, `SYSTEM`) stay ineligible. `REINDEX` on a partitioned table or index is eligible by shape but cannot run inside the engine-owned transaction; the executor reports it as `unsupported-accepted-blocking` (see [Engine-owned session and budgets](#engine-owned-session-and-budgets)). |
| `capability-boundary` | `unsupported-partitioned-parent` with cause `parent-blocking-index-build` | eligible | The missing partition-aware flow does not make the plain PostgreSQL statement unknown. Its principal pre-execution hazard is acquiring the parent lock, which `lock_timeout` bounds. |
| `capability-boundary` | `not-native-safe-rewrite-required` | ineligible | A rewrite holds its strong lock for the full rewrite. Bounding acquisition alone does not bound the outage, and v1 must not imply otherwise. |
| `capability-boundary` | `backend-unavailable` and all other missing routes or backends | ineligible | A missing execution backend is not permission to substitute an unrelated blocking implementation. |
| `no-online-safety-problem` | data changes, provisioning, imperative `CREATE TABLE`, and owner catalog work | ineligible | The lock budget is not pg-sprite's reason to own these jobs. Another tool or front door owns them, and broad admission would become arbitrary SQL passthrough. |
| `environmental` | size, privilege, version, stale plan, collision, or exhausted budget | ineligible | Acceptance cannot repair the environment or override a safety precondition. |
| `invariant-violation` | any cause | ineligible | A state the engine says is incoherent must fail closed, never become executable by flag. |

Other `by-design` refusals are not implicitly eligible. `destructive-change`, index adoption
on a partitioned parent, `IF NOT EXISTS`, duplicate names, and unsupported desired create
shapes each encode a different policy or incoherent input. None is merely a known lock-bound
blocking form. The initial registry therefore contains only the two rows marked eligible.

Rewrite-class statements stay out of v1. A rewrite that acquires `ACCESS EXCLUSIVE` quickly
and runs for an hour blocks readers and writers for that hour; `lock_timeout` has already
finished its job. A required `statement_timeout` limits the maximum outage, but cancellation
near that limit can spend substantial time rolling back and still imposes an operator-sized
risk. Eligibility can be reconsidered only with a separate rewrite policy that estimates
work, sets an explicit outage bound, documents cancellation and rollback behavior under
load, and proves with integration tests that the limit is operationally meaningful. Landing
copy-and-swap is not a reason to add passthrough eligibility; it removes the refusal instead.

## Engine-owned session and budgets

An eligible statement runs as one statement in one engine-owned session and transaction, in
exactly one attempt. The runner sets `lock_timeout` and `statement_timeout` in the transaction
before the statement. It does not hand SQL to a shell, inherit an unbounded caller session, or
use the caller-owned concurrent-index exception.

The single attempt is deliberate and differs from brief native execution, which retries a
lock-budget miss up to three times. The operator accepted one bounded `ACCESS EXCLUSIVE`
acquisition; a second attempt would re-queue behind the same holder, stack another
`lock_timeout` of blocked readers and writers behind the engine's request, and turn the stated
budget into a multiple of itself. An exhausted lock budget is therefore the final typed outcome
([AB-2](invariants.md#ab-2--lock-budget-exhaustion-executes-nothing)); the operator decides
whether to run the command again.

The engine-owned transaction is also the path's boundary. `REINDEX TABLE` and `REINDEX INDEX`
on a partitioned relation are single-relation by shape, but PostgreSQL reindexes each partition
in its own transaction and refuses to start inside a transaction block (SQLSTATE `25001`)
before touching any partition. The executor reports that server refusal as
`unsupported-accepted-blocking`, a permanent outcome, rather than as an execution failure an
adapter might retry.

Both bounds are required and non-zero. `--accept-blocking` rejects an omitted, zero, or
disabled `statement_timeout`; it does not silently inherit an unbounded value. The ordinary
CLI default may pre-populate budgets for safe paths, but accepting blocking execution
requires the operator to state `--statement-timeout` explicitly on that invocation. This is
the maximum server-side execution time the operator is accepting after lock acquisition.

Explicit is a stronger requirement than non-zero, and the flag's current shape cannot express
it: `--statement-timeout` is a shared connection flag with a non-zero default, so the command
receives the same duration whether the operator typed it or not. The implementation must
therefore read whether the flag was supplied from the parsed command line — the argument
parser records that per flag — rather than inspecting the duration's value, and it must not
change the shared flag's type or default for every other command to get there. Downgrading the
requirement to "non-zero" because that is what the value alone can show is not an acceptable
implementation shortcut.

The required bound wins over an unbounded default with an optional override. An unbounded
statement can protect lock acquisition yet take a table out of service indefinitely after
acquiring the lock. That trades the exact fleet hazard this feature should constrain for
convenience. A fixed short default also loses: the right maximum differs between a brief
index drop and index maintenance, and cancellation at an arbitrary default is not safer than
an operator choosing a reviewed window.

The statement timeout is not advertised as an outage stopwatch. Server cancellation begins
at the bound, PostgreSQL then rolls the transaction back, and client or server failure can
make the commit boundary ambiguous until catalog inspection. The lock timeout remains the
genuine improvement over raw `psql`; the statement timeout is a second, required containment
limit rather than proof of online safety.

## Verdict, reports, and exit code

Successful passthrough uses the outcome `executed-without-online-safety`. The name is long on
purpose: `executed-blocking` could falsely claim that observable blocking definitely occurred,
while the contract is that the engine cannot vouch for online safety. Text begins:

```text
executed without online safety (accepted blocking refusal)
  table:     app.orders
  refusal:   by-design / index-statement
  statement: DROP INDEX app.orders_created_at_idx
  safer:     DROP INDEX CONCURRENTLY
  budgets:   lock 3s, statement 10m
```

JSON preserves the accepted refusal rather than clearing it on success:

```json
{
  "outcome": "executed-without-online-safety",
  "reason": "index-statement",
  "class": "by-design",
  "statement": "DROP INDEX app.orders_created_at_idx",
  "table": "app.orders",
  "safer_idiom": "DROP INDEX CONCURRENTLY",
  "blocking_passthrough": true,
  "lock_timeout": "3s",
  "statement_timeout": "10m"
}
```

`reason`, `class`, and any typed `cause` are the original refusal identity, and `safer_idiom`
keeps its existing contract of naming the idiom rather than rewriting the statement. `detail`
remains human explanation, not an automation key. `blocking_passthrough` is an explicit audit field
even though the outcome also distinguishes the path. Existing consumers that ignore additive
fields remain able to read the object; consumers must recognize the new outcome before
treating it as success.

The dry-run plan report adds `blocking_passthrough_eligible` to every refused statement. It is
`true` exactly when the typed registry entry for the refusal's class, reason, and cause or site
is eligible; it depends neither on whether the execution flag was supplied nor on the catalog
lookup that resolves the accepted table, which runs only on the execution path. The report
retains disposition `refuse`, reason, class, cause, and guidance. Eligibility is permission to
request a later execution path, not a reclassification as online-safe.

Exit code 3 means the statement committed through this marked path. Exit code 0 remains
online-safe success, 1 remains execution failure, and 2 remains refusal with nothing run.
Choosing exit 0 plus a marked outcome lost because shell CI would have to parse JSON or prose
to distinguish accepted blocking execution from the product's online-safe success contract.
A distinct code is intentionally non-zero: generic CI fails closed, while a caller that
deliberately permits this path can allow 3 explicitly.

The full ladder once this ships, with the question each code answers:

| Exit | Meaning | Did the statement run? | Online-safe? |
|------|---------|------------------------|--------------|
| 0 | Executed through an online-safe path, or a dry run found every statement executable | yes (dry run: no) | yes |
| 1 | Failure: a PostgreSQL error or statement-budget cancellation after execution started (attempted, rolled back), or an operational or usage error before it, including a mismatched or unresolvable acknowledgement (nothing ran) | attempted or no | not applicable |
| 2 | Refused: ineligible, no acknowledgement supplied, or the lock budget was exhausted | no | not applicable |
| 3 | Committed through the accepted blocking passthrough | yes | no |

Exit 3 is the only code where the statement committed and the engine does not vouch for online
safety, so a consumer can read it without JSON. The exit code is produced the way exit 2 is
today: `migrate` returns a typed sentinel after printing the verdict, and the entry point maps
that sentinel to the code. Exit 3 needs a second sentinel and constant beside
`ExitCodeRefused` in `pkg/verdict`, because the verdict is a success that must still leave a
non-zero process status; `kong`'s default error path would otherwise print it as an
operational failure and exit 1.

## Failure and interruption semantics

Failure before the statement starts, including an exhausted lock budget, remains a typed
refusal with exit code 2 because nothing ran. A PostgreSQL error or statement-budget
cancellation after execution starts is a typed `failed` verdict with the executor's stable
code and exit code 1. Failure never reports the marked-success exit code.

There is no resume promise. The path creates no checkpoint and has no committed-prefix model
beyond the one submitted statement. A normal statement error or cancellation rolls back its
transaction, but a dropped connection at the commit boundary is ambiguous until the operator
inspects the catalog. Process interruption, server restart, and operator cancellation leave
whatever state PostgreSQL left. A retry starts classification again and is not called resume.

The verdict must not claim “nothing committed” when the engine cannot establish that fact.
Where catalog inspection can resolve a known statement shape, a later implementation may
report the observation; v1's contract is documentation and honest ambiguity, not a synthetic
recovery protocol.

## `--force` interaction

The flags are orthogonal. `--force` chooses the submitted form instead of a safer sequence or
available strategy on routes it already governs. `--accept-blocking` accepts one eligible
policy refusal. Neither widens the other's eligible set, disables classification, or changes
budgets.

Passing both flags is a usage error before execution. A single statement reaches one routing
decision, so both acknowledgements cannot be operative at once; accepting both would make the
audit record ambiguous and encourage callers to supply a universal “make it run” bundle.
The error tells the operator to rerun with the flag matching the printed decision. This is
preferable to silently ignoring one flag or assigning precedence.

## Orchestrator exposure

SchemaBot does not surface `--accept-blocking` in v1. Its pg-sprite integration maps refusals
to `ExecutionModeBlocked`, and its product is the reviewed online-safe check. Letting a PR
author accept blocking execution would move the production availability decision away from
an operator and contradict the existing decision not to enable PostgreSQL direct execution.

The plan adapter may preserve the eligibility field as information, but `Apply` must not turn
it into authority. If an orchestrator exposes the path later, it needs an operator-only action
after planning, engine-keyed consent that states reads and writes can be unavailable, the
resolved table acknowledgement, explicit lock and statement bounds, a fresh apply-time
classification, an audit identity, and distinct handling of exit code 3. Author-controlled
configuration or a plan annotation is insufficient.

## Alternatives considered

### Continue requiring raw `psql`

This keeps the online engine's surface smallest, but abandons the operator at the exact lock
queue hazard pg-sprite already knows how to contain. A typed, narrow path preserves refusal
honesty while guaranteeing engine-owned budgets and audit output.

### Make every refusal eligible

This is arbitrary passthrough under another name. Environmental and invariant refusals cannot
be consented away; owner-routed work is outside the engine; and missing backends do not imply
a safe blocking equivalent. Typed opt-in entries keep the boundary reviewable and closed.

### Exit 0 with a marked JSON outcome

This is conventional for a committed command, but it breaks the existing shell-level promise
that 0 means an online-safe path. Requiring every CI caller to parse JSON would regress the
simplest and strongest gate. Exit 3 keeps the accepted result distinguishable without prose.

### Reuse `--force`

`--force` overrides routing when the engine has a submitted form and a safer sequence or
strategy decision; it does not bypass policy refusals. Reuse would erase that distinction,
expand an existing flag's authority, and make old automation acquire new behavior.

### Permit unbounded execution after bounded acquisition

This maximizes completion for legitimate long work, but once a strong lock is acquired it can
block the fleet indefinitely. Required explicit `statement_timeout` makes the accepted upper
bound reviewable while keeping the lock-acquisition protection separate.

## Compatibility and rollout

The design is additive until an operator supplies the new flag. Existing outcomes and exit
codes do not change. A consumer that treats every non-zero process status as failure remains
fail-closed for the new path. Capability prose is re-tiered only when behavior ships, not when
this design lands.

Sequence implementation as follows:

1. Add a typed eligibility registry at the gate, keyed by refusal class, reason, and typed
   cause or refusal site. The `index-statement` site distinguishes the single-relation forms
   from `DROP INDEX a, b` and `REINDEX SCHEMA`, `DATABASE`, and `SYSTEM`, so the registry can
   admit the former and refuse the latter without a database. Its completeness tests make new
   values ineligible by default and prove that render text is never consulted. *(done)*
2. Add the executor path through engine-owned bounded sessions, requiring explicit non-zero
   `statement_timeout` and non-zero `lock_timeout`. At this step add lock-budget invariants to
   [the invariant registry](invariants.md): every passthrough statement runs in an
   engine-owned session under both bounds; lock-budget exhaustion executes nothing; and no
   ineligible, unclassified, environmental, or invariant-violation refusal reaches execution.
   Also add an RF invariant that refusal analysis and identity survive accepted execution.
   The same step amends two existing entries whose unqualified wording this behavior makes
   false. [RF-5](invariants.md#refusals-and-preflight-rf) names `--force` as *the* route for
   running a risky statement as-submitted; its amended text reads "running as-submitted
   requires a loud, typed, audited acknowledgement: `--force` on the routes it governs, or
   `--accept-blocking` for an eligible policy refusal".
   [RF-6](invariants.md#refusals-and-preflight-rf) says pg-sprite does not substitute a
   blocking parent build for the missing partition-aware flow; its amended text adds "on its
   own initiative — an operator may accept that build only through `--accept-blocking`, under
   engine-owned lock and statement budgets, with the
   refusal identity retained". This design establishes no invariant on its own and amends none
   until the behavior ships; the registry describes shipped behavior only, matching the
   sequencing rule in [refusal-classes.md](refusal-classes.md), whose own rollout record
   checked RF-5 and RF-6 and recorded them unchanged. *(done: the executor-owned AB-1 and AB-2
   invariants ship here; the front-door invariants, refusal-identity invariant, and RF-5/RF-6
   amendments move to step 4 with the flag.)*
3. Add `executed-without-online-safety`, retained reason/class/cause, budget fields, exit code
   3, and dry-run eligibility to the verdict and plan-report contracts. Exit 3 lands as a
   constant and sentinel in `pkg/verdict` beside `ExitCodeRefused` and `ErrRefused`, mapped
   in the entry point the same way. Update [cli-output-examples.md](cli-output-examples.md)
   with generated examples — a new `executed-without-online-safety — exit 3` section and the
   exit-code contract paragraph at its head — and pin the JSON and text renderers. The
   exit-code contract is stated in three more places that today describe a three-code ladder
   and must gain exit 3 in the same change: the exit-codes bullet in
   [execution-model.md](execution-model.md), the dry-run exit-code paragraph in
   [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md#dry-run-diagnostic-codes),
   and the root README's exit-code gate paragraph, which must say that a gate treating every
   non-zero status as failure stays fail-closed for this path.
4. Add `--accept-blocking` to imperative `migrate`, reject its combination with `--force`,
   emit the pre-execution audit record, and add demo assertions for eligibility, success,
   lock-budget refusal, statement-budget failure, mismatched acknowledgement, and exit codes.
   This step adds the two pieces of plumbing the acknowledgement needs: the statement boundary
   exposes the index or table an index-maintenance statement names, and the front door resolves
   an index to its owning table through `pg_index.indrelid` after the eligibility check. It also
   detects an explicitly supplied `--statement-timeout` from the parsed command line, as the
   budgets section requires.
5. Re-tier only the newly shipped path by editing its rows in
   `pkg/capabilities/capabilities.yaml` and regenerating [capabilities.md](capabilities.md)
   with `make gen-capabilities` — the page is a rendered artifact and a hand edit fails its
   generation test. In the same capability-statement change update
   [limitations.md](limitations.md), the root README, and two rows of
   [refusal-classes.md](refusal-classes.md) that state the opposite of this behavior today:
   the `by-design` row's "run the blocking form outside pg-sprite in a maintenance window",
   and the `parent-blocking-index-build` row's "pg-sprite will not substitute a blocking
   parent build". The docs guard for that table checks only that a row exists per cause, not
   what the row says, so nothing fails when those rows go stale. The underlying operation tier
   does not change in this design-only change.

## Non-goals

- Silent bypass, arbitrary SQL execution, string-matched eligibility, or skipped refusal
  analysis.
- Changing existing exit codes or the `executed-natively` outcome for online-safe paths.
- Implementing copy-and-swap, a partition-aware index flow, or any missing backend.
- Changing the tier of any operation as part of this decision record.
- Promising resume, rollback duration, or automatic recovery for an interrupted passthrough.
- Exposing accepted blocking execution through SchemaBot in v1.
