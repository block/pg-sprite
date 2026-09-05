# The TCB model: a small trusted core, an untrusted periphery

pg-sprite rewrites production tables in financial systems — a bug is silent data corruption or
an app-wide outage. That puts its core in the **mission-critical** class, and the right structure
for mission-critical software is a **Trusted Computing Base (TCB)**: identify the invariants that
must never be violated, put the code that enforces them behind a small boundary, treat that
boundary as trusted, and treat everything outside as untrusted. Untrusted code may *request*
dangerous operations as many times as it likes — the TCB ensures correctness anyway.

The [invariant registry](invariants.md) is step one of that recipe. This doc is the rest:
**where the boundary sits, how illegal states are made unrepresentable, and which engineering
rules apply inside the boundary** — drawn from codebases that live or die by this model:
[qmail](https://cr.yp.to/qmail.html) (mutually-distrustful partitioning),
[bitcoin-core](https://github.com/bitcoin/bitcoin) (consensus-critical code isolation),
[s2n-tls](https://github.com/aws/s2n-tls/blob/main/docs/DEVELOPMENT-GUIDE.md) (cognitive-load
minimalism, priority ordering), [TigerBeetle's
TIGER_STYLE](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/TIGER_STYLE.md)
(assertion discipline, put a limit on everything), and
[HTMX's Locality of Behavior](https://htmx.org/essays/locality-of-behaviour/).

## Table of contents

- [The boundary](#the-boundary)
- [The trust rule: the TCB never trusts its callers](#the-trust-rule-the-tcb-never-trusts-its-callers)
- [Make illegal states unrepresentable](#make-illegal-states-unrepresentable)
- [Rules inside the TCB](#rules-inside-the-tcb)
- [Dependencies inside the TCB become part of the TCB](#dependencies-inside-the-tcb-become-part-of-the-tcb)
- [The verification ladder](#the-verification-ladder)
- [AI-assisted development policy](#ai-assisted-development-policy)
- [What this changes concretely](#what-this-changes-concretely)

## The boundary

Membership test: *can a bug here violate a [invariants.md](invariants.md) invariant —
corrupt data, lose writes, swap in a wrong table, strand a slot, or take the application down?*
If yes, it is TCB. If a bug here can only produce a wrong message, an ugly plan, a wasted copy,
or a missed optimization, it is periphery.

| Component | TCB? | Invariants it enforces |
| --- | --- | --- |
| checksum engine (incl. continuous checker, repair) | ✅ | CO-1, CO-2, CO-3 |
| copier + applier write paths (chunk SQL, flush scheduling, change buffer) | ✅ | CO-4, CO-5, CO-6, LK-3 |
| decode position accounting (slot LSN, snapshot coordination) | ✅ | ST-4, CO-4 |
| cutover (swap txn, final drain, fidelity gate, ambiguity resolution) | ✅ | LK-2, LK-4, ST-5 |
| checkpoint store (write/read/validate) | ✅ | ST-1, ST-2 |
| slot lifecycle (create, reap, lag ceiling) | ✅ | ST-3 |
| `dbconn` dangerous primitives (advisory lock, terminate-blockers, session timeouts) | ✅ | LK-1, LK-2 |
| preflight verifier | ✅ | ST-6, RF-1..RF-5 |
| planner / classifier / router | ⚠️ outside, see below | — |
| declarative diff + `fmt` | ❌ | (its *output* is re-classified and re-preflighted inside) |
| CLI, flags, help, prompts, `--force` UX | ❌ | (the force *gate decision* is TCB; the prompt rendering is not) |
| status / progress / ETA rendering, advisory text, lint wording | ❌ | — |
| metrics/observability emission | ❌ | — |
| orchestrator adapter ([schemabot-integration.md](schemabot-integration.md), Phase 11) | ❌ | OC-5, OC-6 hold *at* the boundary |

**The planner is deliberately outside.** Its verdicts are *requests*, not permissions — the Notes
.app / cold-storage pattern from the TCB model: transaction *generation* is untrusted because the
signer enforces policy. Concretely: if the classifier wrongly says "native-safe", the native
executor's own `lock_timeout` bound (LK-2) caps the damage; if it wrongly says "copy", the result
will be a wasteful but *correct* migration once copy-and-swap exists (checksum will still gate).
Today the router reports that route unavailable. The router may choose a bad
strategy; it must never be *able* to cause a wrong result. This is also why the copy-and-swap
executor re-runs preflight itself rather than trusting that the planner did.

qmail's lesson applies directly: the components are **mutually distrustful**, and each does one
thing. bitcoin-core's lesson is organizational: consensus-critical code gets a different review
bar, a different rate of change, and explicit isolation (`libbitcoinkernel`) — our analog is the
TCB package list above, enforced in CI (see [below](#what-this-changes-concretely)).

## The trust rule: the TCB never trusts its callers

Every dangerous operation is behind a trusted interface that **re-verifies its own
preconditions**, regardless of what the caller claims:

- `cutover` does not take the planner's word that the shadow is ready — it demands a
  `VerifiedShadow` (below) and re-checks the fidelity gate (ST-5) inside its own transaction.
- The executors re-run preflight (ST-6) even though the CLI ran it for the dry-run display.
- Control verbs (`stop`/`start`/`cutover`/`cancel`) re-validate current state before acting
  (OC-1, OC-4): a `cutover` request against an unverified shadow is refused, not trusted.
- The orchestrator, the CLI, and any future API are all the same thing to the TCB:
  **untrusted request sources.** They can retry forever; they cannot make the engine do the wrong thing.

## Make illegal states unrepresentable

The domain-type rule: *a domain type may carry the same data as the raw input,
but the domain type proves validation has occurred* — and downstream code accepts only the domain
type. In Go we get this with unexported fields + package-private constructors: the **only** way
to obtain the type is through the function that validates it.

| Raw / untrusted | Validating passage | Domain type (proof) | Encodes |
| --- | --- | --- | --- |
| `string` (user SQL) | `statement.ParseOne` / `statement.ParseOps`, then `planner.Classify` | `planner.Plan` / `planner.Decision` | CO-7 — classification consumes parsed operation descriptors |
| table name | preflight | `PreflightedTable` (carries the proven facts: PK, no FKs/views, replica identity, headroom) | ST-6, RF-* |
| table name (create target) | `preflight.CheckTableAbsent` | `AbsentTarget` (carries the resolved creation schema and the verified-free name; time-of-check — minted inside the apply session, never carried across a plan boundary, and re-verified at use the way ST-7 re-verifies `PreflightedTable`) | ST-6 for the create path |
| creating role's access (create target) | `preflight.CheckCreatePrivileges` | `CreationRole` (carries the connected role and the resolved creation schema whose CONNECT / USAGE / CREATE grants were verified; time-of-check and session-scoped, like `AbsentTarget` — a revoked grant after minting fails with the server's own error) | ST-6 for the create path |
| shadow table | full checksum pass (planned) | `VerifiedShadow` — its constructor will be private to `pkg/checksum`; the planned `cutover.Swap` will accept **only** this type | CO-1 in the type system |
| chunker low-watermark | all-checkers-clean pass (planned) | `CleanWatermark` — will be unobtainable in a pass that repaired anything | CO-2 |
| — | planned table-lock acquisition | `TableLock` token, planned as a required parameter of every mutating operation | LK-1 |
| orchestrator proto/request | adapter validation at the edge | engine domain types; proto types never cross into the engine | OC-5, OC-6 |

The intended compile-time effect of the future cutover API: **the cutover cannot be called with
an unverified shadow because no such call type-checks.** An LLM (or a tired human) writing periphery code cannot hand a raw string to
the router or a fresh shadow to the swap — the types refuse. This is the cheapest, most durable
enforcement we have; runtime assertions (below) are the second layer for what Go's types can't
express.

## Rules inside the TCB

Adopted from the named codebases; these apply to TCB packages and are advisory elsewhere.

**Priorities, in order (s2n-tls, adapted):** Correctness → Readability → Ease of use →
Performance. When a trade-off is hard, the higher priority wins. This is
[safety over speed](design-principles.md) made operational for code review.

**Put a limit on everything (TIGER_STYLE).** Every loop bounded, every queue bounded, every
retry counted, every wait deadlined. Existing instances include bounded native attempts, retry
budgets, bounded session timeouts, and — for concurrent index builds — a caller-owned
cancellable context standing in for the server statement timeout; the executor refuses a
context that cannot be cancelled, so the bound is different in kind, not absent. The future copy-and-swap
path will also bound its change buffer, chunk target time, and slot-lag ceiling. The rule makes
limits the *default*: an unbounded anything in a TCB package is a review-blocking defect. Where a
loop is intentionally endless (the applier's consume loop), that must be stated and its exit
conditions asserted.

**Assert the positive and the negative space (TIGER_STYLE).** Assertions detect programmer
errors; operating errors get error handling. In a migration engine, "crash on corrupt logic" is
correct *before* the swap — a failed migration is recoverable (resume/abort), a wrong swap is
not. Concretely:

- Assert preconditions and postconditions of every TCB function on data it did not produce.
- **Pair assertions** across boundaries where data crosses valid/invalid lines: chunk boundaries
  asserted where the copier produces them *and* where the checksum consumes them; the LSN
  asserted where decode records it *and* where the drain claims completion; row counts asserted
  before write and after read-back.
- Invariant violations will map to a distinct error class (`ErrInvariantViolation`) in the
  executor phases, always aborting fail-closed and naming the [invariants](invariants.md) ID —
  never a warning, never retried.

**Simple, explicit control flow (s2n-tls + TIGER_STYLE).** Linear happy path, branch on failure
(Go's early-return idiom is s2n's `GUARD` pattern natively); treat `else` with suspicion; push
`if`s up and `for`s down — the parent function owns control flow and state transitions, leaf
functions stay pure; no recursion in TCB packages; explicit state machines with typed states
rather than booleans that can disagree.

**Minimize state (TIGER_STYLE).** Derive rather than store; re-derive rather
than persist when possible. The planned checkpoint (ST-1) will hold the *minimum* resumable state —
watermark, slot name, LSN, statement fingerprint, engine version — everything else is
reconstructed from the database on resume. Small state is what makes "work out all system state
by hand" possible during an incident.

**Locality of behavior (HTMX).** The enforcement of an invariant is *local and visible where it
matters*: the code that enforces CO-2 carries a `// INV: CO-2` comment at the enforcement point,
so a reviewer (or an agent) greps the ID and sees the entire enforcement in one screen — the
Spirit `runner.go` watermark comment is the exemplar. Don't smear one invariant's enforcement
across three packages; if the protocol spans components (CO-4), each side asserts its half and
names the shared ID.

**Function-size discipline (TIGER_STYLE's 70-line rule, softened to a review heuristic):** if a
TCB function doesn't fit on a screen, look for the hourglass shape — few parameters, meaty pure
middle, simple return.

## Dependencies inside the TCB become part of the TCB

A dependency inside the boundary is code we ship with full trust — so the list is explicit and
short:

| Dependency | Status | Treatment |
| --- | --- | --- |
| `pgx/v5` / `pgconn` | TCB (unavoidable — the wire) | pin, review upgrades like TCB changes, changelog read before bump |
| `pglogrepl` | planned TCB dependency (future decode path; not currently imported) | pin and review upgrades like TCB changes when added |
| `go-pgquery` (Wasm `libpg_query`) | boundary (parses untrusted input into typed operation descriptors) | fuzz at our boundary; parse failure is an error (CO-7), never a fallback; a parser crash is a Wasm trap surfaced as a Go error, not a process crash; verify wasilibs' reproducible-build provenance on every bump (cgo `pg_query_go` supplies AST types only) |
| `kong`, `testcontainers`, testify | periphery / test-only | normal hygiene |

Rule: **no new dependency inside TCB packages without an explicit recorded decision.** CI
enforces the import boundary (below), so a periphery-only dep physically cannot creep into the
core. The decision rubric, in order:

1. **Is it load-bearing expertise?** A real SQL grammar (`go-pgquery`), the wire protocol
   (`pgx`/`pglogrepl`), crypto — take the dependency, pin it, treat it as TCB. Hand-rolling a
   SQL parser to avoid a dependency would be the *opposite* of safety (CO-7 exists because
   string-splitting SQL is how tools corrupt data).
2. **Could ~100 lines of copied code do the job?** Then ["a little copying is better than a
   little dependency"](https://go-proverbs.github.io/) (Go proverbs): copy it, with an
   attributing comment. Retry/backoff, CA-bundle loading, and the keepalive loop are
   hand-written for exactly this reason — already the Phase 0 practice.
3. **Neither?** Then the feature is probably too big for its value — reconsider the feature
   before reconsidering the rule.

One decision worth recording now because it will tempt every phase: **pg-sprite must not import
`block/spirit` as a Go module.** We port Spirit's *ideas* — the invariants in
[invariants](invariants.md) carry file-level citations precisely so the lineage survives without a
code dependency. Importing it would drag the MySQL toolchain (go-mysql, the TiDB parser) into
our module graph, couple our releases to Spirit's cadence, and blur the "separate, purpose-built
tool" stance of [low-level-design](low-level-design.md). Small helpers worth having (`CloseAndLog`-style
cleanup, status/state-machine shapes) are copied and re-owned, not imported. (the orchestrator importing *us* is the
correct direction of that arrow.)

## The verification ladder

Testing is necessary but not sufficient; each rung catches what the previous
can't. The [phase mapping in 17](invariants.md#build-phase-mapping) says *when*; this says
*what kind*:

1. **Unit + integration against real PostgreSQL** — the floor, already policy
   (build-plan, test-first, no mocked-DB core tests).
2. **Property-based tests** (`pgregory.net/rapid` — already in our module graph via pgx) for the
   TCB's algorithmic hearts: the chunker (*property: chunks exactly partition the PK space — no
   gap, no overlap — for random PK distributions*), the change buffer (*after any event sequence,
   at most one entry per PK holding the latest image* — CO-5), and the applier convergence
   protocol (*for random interleavings of chunk-copy and backlog-flush, the shadow converges* —
   CO-4, CO-6 including unique-value moves).
3. **Deterministic-interleaving tests**: the copier/applier race tests (Phase 6) drive
   interleavings through injected scheduling points rather than sleeps, so every race in
   [low-level-design § copy/apply ordering](low-level-design.md#copy-and-apply-ordering-the-core-correctness-subtlety)
   is reproducible, not probabilistic. (The affordable slice of TigerBeetle's simulation-testing
   idea.)
4. **Fuzzing** (Go native fuzzing) at trust boundaries: statement input → parser/classifier
   (must classify, refuse, or error — never panic, never misroute), checksum expression
   generation over adversarial column types/collations (CO-1's determinism).
5. **Model checking (optional, high value): TLA+/PlusCal for the two real state machines** — the
   cutover protocol (LK-2/LK-4: lock, drain-to-LSN, swap, ambiguity resolution, retry) and the
   checkpoint/resume/slot-loss machine (ST-1..ST-4, CO-2's watermark invalidation). These are
   exactly the "distributed handshake" shapes TLA+ pays off on; Kani/CBMC don't apply to Go, and
   this is our equivalent. Scoped as a build-tracker task, not a build-plan gate.

Assertions multiply all of this (TIGER_STYLE): the property tests and fuzzers find bugs by
tripping TCB assertions, not just by comparing final outputs.

## AI-assisted development policy

The AI-assistance posture differs per side of the boundary:

- **Inside the TCB: less AI, more steering.** Detailed spec first (05 + the 17 invariant IDs are
  the spec), test-first with the invariant's named test obligation, small diffs, careful review
  against the rules above. Agents are *excellent* here precisely because the invariants are
  written down — but the human owns the mental model (TIGER_STYLE: assertions are a safety net,
  not a substitute for understanding).
- **Outside the TCB: more AI, less steering.** Status rendering, advisory wording, docs, CLI
  ergonomics — iterate at inference speed; the boundary means a bug here cannot corrupt data.
- **[AGENTS.md](../AGENTS.md) encodes the split**: a short section
  naming the TCB packages and the stricter rules that apply inside — and stays short otherwise
  (don't restate what an agent can infer from the code).
- **Pinned dev environment + one entry point**: hermit-pinned toolchain and `make
  build/test/lint` as the only entry points, so agents never burn effort on environment drift.

## What this changes concretely

1. **Repo layout marks the boundary.** TCB packages are enumerated (the repo-root [`SAFETY.md`](../SAFETY.md), listing them and linking here and to the invariant registry), and CI enforces it: an import-boundary lint (depguard) pins which dependencies
   TCB packages may import, and `CODEOWNERS` routes TCB paths to maintainer review (the
   bitcoin-core discipline).
2. **`cutover`/`swap` API takes domain types only** — the `VerifiedShadow`/`CleanWatermark`/
   `TableLock` types land with their producing packages (Phases 4–7), not retrofitted.
3. **The `// INV: <id>` convention and `ErrInvariantViolation` have landed** with the
   executor phases.
4. **The enforcement backlog:** [SAFETY.md](../SAFETY.md), the depguard import-boundary
   rule (`.golangci.yml`), and CODEOWNERS have landed; still open are the property/fuzz
   suites per rung above and the optional TLA+ models for cutover and resume.
5. **The periphery stays free.** None of this doc applies review friction to status text, CLI
   help, or docs — that's the point of having a boundary.
