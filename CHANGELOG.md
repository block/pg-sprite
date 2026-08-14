# Changelog

Notable changes to pg-sprite, with emphasis on anything that changes what
automation observes: exit codes, verdict fields, and outcome vocabulary.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed — observable outcomes for automation callers

- **Partitioned-parent index builds now refuse with exit 2 and reason
  `unsupported-partitioned-parent`** instead of reaching a mid-change exit-1
  PostgreSQL failure. This includes forced blocking parent builds.
- **Partitioned-parent foreign keys added `NOT VALID` refuse on PostgreSQL
  before 18** with reason `unsupported-partitioned-parent`; PostgreSQL 18 and
  later retain the supported in-place path.
- **Dry-run and declarative diff reports now account for partitioned-parent
  admission**, reporting disposition `refuse` and reason
  `unsupported-partitioned-parent` instead of advertising execution.

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
