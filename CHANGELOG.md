# Changelog

Notable changes to pg-sprite, with emphasis on anything that changes what
automation observes: exit codes, verdict fields, and outcome vocabulary.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.1.1] - 2026-08-30

### Changed — observable outcomes for automation callers

- **Desired-state execution now creates a table that does not exist yet**
  instead of refusing the plan. `migrate.RunDesired` on a greenfield plan
  verifies the target name is free and the role holds `CREATE` on the
  schema, then runs the `CREATE TABLE` and the index builds as brief
  bounded steps; a rerun converges to an empty plan. An occupied name is a
  new typed refusal reason, **`create-collision`** (added to
  `verdict.Reasons()`); `PARTITION OF` and `IF NOT EXISTS` shapes refuse
  with `unsupported-statement` before anything runs. A caller that relied
  on the previous greenfield `unsupported-statement` refusal now sees the
  create execute. Desired-file statements are additionally ordered for
  execution at parse — the `CREATE TABLE` first, indexes keeping their
  input order after it — everywhere the file replays: the greenfield plan,
  the create path's steps, and the scratch-schema introspection that
  derives a diff once the table exists. The plan states execution order, a
  greenfield plan's fingerprint changes when the desired file listed an
  index before its table, and an index-first file converges on rerun.
- **Alter attempts now run with `search_path` pinned to the target
  schema** (then `public`) whenever the statement is schema-qualified —
  the same resolution the create path and introspection use. A statement's
  unqualified secondary names — a column's type, an expression's
  function — resolve in the target schema, where previously they resolved
  via the session's ambient `search_path` and could silently bind a
  same-named object in `public`. A caller that relied on ambient
  resolution for secondary names must qualify them.
- **`ALTER COLUMN ... DROP NOT NULL` now classifies as destructive** in plan
  reports (`destructive: true`): dropping `NOT NULL` discards the same
  guarantee as dropping the equivalent constraint. The full destructive set
  is a dropped column, constraint, index, or `NOT NULL`; `DROP DEFAULT` is
  deliberately not destructive — a default guarantees nothing about existing
  rows and is recreated by a metadata-only statement. A consumer gating on
  `.statements[].destructive` now sees `DROP NOT NULL` flagged, and
  desired-state execution refuses it like any other drop.

### Added

- **Library-level desired-state execution: `migrate.RunDesired`** converges
  one live table onto its parsed desired schema — derive the convergence
  plan, admit it as a whole (table existence, destructive guard, routed
  dispositions, optional `ExpectedFingerprint` pin), then run each planned
  statement back through the same `migrate.Run` pipeline with fresh
  introspection and classification, stopping at the first refusal or
  failure. The result carries the plan, per-statement verdicts, and an
  aggregate outcome with committed-prefix detail
  ([docs/execution-model.md](docs/execution-model.md)). Two new refusal
  reasons enter the vocabulary: `destructive-change` (the plan discards
  live structure — a dropped column, constraint, index, or `NOT NULL`;
  desired-state execution never runs it) and
  `plan-fingerprint-mismatch` (the plan derived at execution time is not
  the pinned reviewed plan). Library-only for now — the `migrate --desired`
  CLI flag follows separately.

## [0.1.0] - 2026-08-19

First module release: native-path CLI and adapter surface. See the
[v0.1.0 release notes](https://github.com/block/pg-sprite/releases/tag/v0.1.0).

### Changed — observable outcomes for automation callers

- **`diff` now exits 2 when the derived plan contains a statement execution
  would refuse**, in all three output modes (default report, `--sql`,
  `--json`) — the same CI-gate contract as `migrate --dry-run`. Previously
  `diff` always exited 0, so CI gating on the exit code saw refusals as
  green. The report is still complete and valid on stdout; a caller must
  read the plan before branching on the exit code.
- **`diff` prints the diagnostic report by default; the executable SQL
  script moved behind `--sql`** (`--sql --json` is rejected at parse time).
  A caller consuming default `diff` stdout as SQL must ask for the script
  explicitly.
- **`lint` and `suggest` text output renders in the dry-run's diagnostic
  grammar.** Display only — the JSON reports and exit codes are unchanged —
  but the one-line `file:line:column: severity: …` shape is gone, so
  errorformat-style CI annotators should consume `--json` and supply the
  file name they passed in.
- **Human diagnostic reports color their labels when stdout is a terminal**
  (`--color=auto`, the default, on `migrate`, `diff`, `lint`, and
  `suggest`). A caller that allocates a pty — `docker run -t`, `script`, an
  expect harness, a CI runner with tty allocation — now receives ANSI
  escape sequences in the human reports where it previously received plain
  text; `--color=never`, a non-empty `NO_COLOR`, or `TERM=dumb` forces
  plain output, and `--color=always` keeps color on a pipe. The machine
  contracts are never colored regardless of mode: the `--json` reports and
  `diff --sql` stay escape-free, and exit codes are identical across color
  modes.
- **The plan report is format version 2**: rewrite-required statements now
  carry a `guidance` field naming the typed manual path, drawn from the
  suggest report's Guidance vocabulary. The fingerprint definition is
  unchanged.
- **The suggest report is format version 2**: the Guidance vocabulary gains
  `name-constraint-then-validate`, emitted for an unnamed `ADD CHECK` /
  `ADD FOREIGN KEY`, and `unique-index-then-constraint`, covering an
  `ADD PRIMARY KEY` / `ADD UNIQUE` whose `USING INDEX` rewrite could not be
  constructed. Previously those statements made `suggest` (and any surface
  deriving guidance) fail with an internal error instead of producing
  advice.
- **`migrate --dry-run` text output reserves `help:` for steps the user runs**:
  a rewrite-required refusal's manual path renders as `help[<guidance>]:` after
  the findings that explain it, while a safer sequence pg-sprite runs itself is
  now a `note:`. Each guidance code's `docs:` line links its own anchor in
  docs/suggest-report.md. Display only — the JSON report is unchanged.
- **`migrate --dry-run` now exits 2 when any statement would be refused**
  (disposition `rewrite-required`, `unavailable`, or `refuse`), matching the
  refusal exit code an apply of the same statement ends in. Previously a dry
  run always exited 0, so CI gating on the exit code saw refusals as green.
- **Statement kinds `migrate` does not support are now gated in `--dry-run`
  too**: `DROP INDEX`, `REINDEX`, `CREATE TABLE`, and other non-`ALTER TABLE`
  / `CREATE INDEX` statements emit the same refusal verdict (exit 2) the
  apply would, instead of dry-running to an executable plan.
- **`--force` combined with `--dry-run` is now rejected at parse time.**
  A dry run reports the unforced plan, so the acknowledgement has nothing to
  apply to; previously the flag was silently ignored.
- **`planner.Reasons()` now includes `app-breaking-rename`.** The value was
  always emitted in reports and documented in the plan-report vocabulary,
  but the programmatic enumeration omitted it, so a consumer enumerating
  the closed set would wrongly treat a routable rename as unknown.

- **Index builds on partitioned parents now refuse with exit 2 and reason
  `unsupported-partitioned-parent`** instead of reaching a mid-change exit-1
  PostgreSQL failure. Concurrent and blocking builds carry distinct typed
  causes; blocking builds are refused by policy even though PostgreSQL supports
  them. Leaf partitions retain the blocking-to-`CONCURRENTLY` substitution
  described below.
- **Partitioned-parent `ADD CONSTRAINT ... USING INDEX` now refuses before
  execution** on every supported PostgreSQL version, with typed cause
  `parent-index-adoption`.
- **Partitioned-parent foreign keys added `NOT VALID` refuse on PostgreSQL
  before 18** with reason `unsupported-partitioned-parent`; PostgreSQL 18 and
  later retain the supported in-place path.
- **Dry-run and declarative diff reports now account for partitioned-parent
  admission**, reporting disposition `refuse` and reason
  `unsupported-partitioned-parent` instead of advertising execution. Refused
  statements now carry the reason directly and omit impossible safer-SQL advice.

- **A plain (blocking) `CREATE INDEX` now succeeds instead of refusing.**
  The engine substitutes `CREATE INDEX CONCURRENTLY` and drives it to a
  verified valid index: exit 0, with the substitution disclosed in the
  verdict's `executed_sql`. Previously this refused with exit 2 and reason
  `index-statement`. Anything branching on the old refusal will now see
  success for the same input.
- **Rewrite-requiring changes (e.g. `ALTER COLUMN ... TYPE`) refuse up
  front with reason `backend-unavailable`** instead of running a blind
  bounded attempt that budget-cancels. No lock acquisition is attempted;
  the refusal is decided from the classification alone. Previously the same
  input ended in reason `not-native-safe-budget-exceeded` after a cancelled
  attempt.
- **Blocking `ALTER TABLE` forms with a safer native sequence (e.g.
  `SET NOT NULL`) run as that sequence by default**, disclosed in
  `executed_sql`. The submitted form can still be forced with
  `--force <schema.table>`, which is audited and recorded in the verdict's
  `forced` field.

### Added

- **A third verdict outcome, `failed`,** for execution failures (still exit
  1 — refusals remain exit 2). The verdict carries the executor's stable
  outcome code in `code`, and for a mid-sequence failure the 1-based
  `failed_step`, its `failed_step_sql`, and the committed prefix in
  `executed_sql` — an empty prefix means nothing committed, so automation
  can distinguish "nothing happened" from "partial state left behind"
  without parsing stderr.
- `--force <schema.table>`: an audited override that runs the submitted
  form as one bounded attempt, unlocking only the substitution-override,
  rewrite-required, and backend-unavailable paths. Planner refusals stay
  unforceable.
