package verdict

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRefusalEnforcesRF7(t *testing.T) {
	r, err := NewRefusal(ClassNoOnlineSafetyProblem, ReasonUnsupportedStatement, OwnerDataChangeRunner)
	require.NoError(t, err)
	assert.Equal(t, ClassNoOnlineSafetyProblem, r.Class())
	assert.Equal(t, ReasonUnsupportedStatement, r.Reason())
	assert.Equal(t, OwnerDataChangeRunner, r.Owner())
	assert.False(t, r.IsZero())
	assert.True(t, Refusal{}.IsZero())
	for _, tc := range []struct {
		name   string
		class  Class
		reason Reason
		owner  Owner
	}{
		{"unknown reason", ClassCapabilityBoundary, "unknown", ""},
		{"zero reason", ClassCapabilityBoundary, ReasonNone, ""},
		{"unknown class", "unknown", ReasonUnsupportedStatement, ""},
		{"zero class", "", ReasonUnsupportedStatement, ""},
		{"unknown owner", ClassNoOnlineSafetyProblem, ReasonUnsupportedStatement, "nobody"},
		{"missing owner", ClassNoOnlineSafetyProblem, ReasonUnsupportedStatement, ""},
		{"unexpected owner", ClassByDesign, ReasonUnsupportedStatement, OwnerDirectOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRefusal(tc.class, tc.reason, tc.owner)
			assert.Error(t, err)
		})
	}
}

func TestAcceptedBlockingVerdictContract(t *testing.T) {
	r := ByDesign(ReasonIndexStatement).WithSite(RefusalSiteIndexSingleRelation)
	v, err := (Verdict{
		Statement:  "DROP INDEX app.orders_created_at_idx",
		Table:      "app.orders",
		SaferIdiom: "DROP INDEX CONCURRENTLY",
	}).WithAcceptedBlocking(r, 3*time.Second, 10*time.Minute)
	require.NoError(t, err)

	assert.Equal(t, OutcomeExecutedWithoutOnlineSafety, v.Outcome)
	gotRefusal, err := v.Refusal()
	require.NoError(t, err)
	assert.Equal(t, r.Class(), gotRefusal.Class())
	assert.Equal(t, r.Reason(), gotRefusal.Reason())
	js, err := v.JSON()
	require.NoError(t, err)
	assert.Equal(t, `{
  "outcome": "executed-without-online-safety",
  "reason": "index-statement",
  "class": "by-design",
  "statement": "DROP INDEX app.orders_created_at_idx",
  "table": "app.orders",
  "safer_idiom": "DROP INDEX CONCURRENTLY",
  "blocking_passthrough": true,
  "lock_timeout": "3s",
  "statement_timeout": "10m"
}`, js)
	assert.Equal(t, `executed without online safety (accepted blocking refusal)
  table:     app.orders
  refusal:   by-design / index-statement
  statement: DROP INDEX app.orders_created_at_idx
  safer:     DROP INDEX CONCURRENTLY
  budgets:   lock 3s, statement 10m`, v.String())
}

func TestAcceptedBlockingVerdictRejectsIllegalStates(t *testing.T) {
	r := ByDesign(ReasonIndexStatement).WithSite(RefusalSiteIndexSingleRelation)
	for _, tc := range []struct {
		name      string
		refusal   Refusal
		lock      time.Duration
		statement time.Duration
	}{
		{"missing refusal", Refusal{}, time.Second, time.Second},
		{"ineligible refusal", CapabilityBoundary(ReasonRewriteRequired), time.Second, time.Second},
		{"zero lock budget", r, 0, time.Second},
		{"negative lock budget", r, -time.Second, time.Second},
		{"zero statement budget", r, time.Second, 0},
		{"negative statement budget", r, time.Second, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (Verdict{}).WithAcceptedBlocking(tc.refusal, tc.lock, tc.statement)
			assert.ErrorIs(t, err, ErrInvalidAcceptedBlocking)
		})
	}
}

func TestExistingVerdictJSONIsUnchanged(t *testing.T) {
	js, err := (Verdict{Statement: "DROP INDEX app.i", SaferIdiom: "DROP INDEX CONCURRENTLY"}).
		WithRefusal(ByDesign(ReasonIndexStatement)).JSON()
	require.NoError(t, err)
	assert.Equal(t, `{
  "outcome": "refused",
  "reason": "index-statement",
  "class": "by-design",
  "statement": "DROP INDEX app.i",
  "safer_idiom": "DROP INDEX CONCURRENTLY"
}`, js)
	assert.False(t, errors.Is(ErrRefused, ErrAcceptedBlocking))
}

// The per-class constructors agree with NewRefusal: what they build, it
// accepts; and each one's class is the one its name says.
func TestClassConstructorsAgreeWithNewRefusal(t *testing.T) {
	for _, r := range []Refusal{
		CapabilityBoundary(ReasonRewriteRequired),
		NoOnlineSafetyProblem(ReasonUnsupportedStatement, OwnerProvisioning),
		ByDesign(ReasonDestructiveChange),
		Environmental(ReasonTableTooLarge),
		InvariantViolation(ReasonUnsupportedStatement),
	} {
		got, err := NewRefusal(r.Class(), r.Reason(), r.Owner())
		require.NoError(t, err, r.Class())
		assert.Equal(t, r, got)
	}
	assert.Equal(t, ClassCapabilityBoundary, CapabilityBoundary(ReasonRewriteRequired).Class())
	assert.Equal(t, ClassNoOnlineSafetyProblem, NoOnlineSafetyProblem(ReasonUnsupportedStatement, OwnerProvisioning).Class())
	assert.Equal(t, ClassByDesign, ByDesign(ReasonDestructiveChange).Class())
	assert.Equal(t, ClassEnvironmental, Environmental(ReasonTableTooLarge).Class())
	assert.Equal(t, ClassInvariantViolation, InvariantViolation(ReasonUnsupportedStatement).Class())
}

func TestWithRefusalStampsOutcomeReasonClassOwner(t *testing.T) {
	v := Verdict{Statement: "GRANT SELECT ON t TO r", Detail: "x"}.
		WithRefusal(NoOnlineSafetyProblem(ReasonUnsupportedStatement, OwnerProvisioning))
	assert.Equal(t, OutcomeRefused, v.Outcome)
	assert.Equal(t, ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, ClassNoOnlineSafetyProblem, v.Class)
	assert.Equal(t, OwnerProvisioning, v.Owner)
	assert.Equal(t, "GRANT SELECT ON t TO r", v.Statement)
	assert.Equal(t, "x", v.Detail)

	// A refusal with no owner leaves the field empty, so it is omitted from JSON.
	v = Verdict{}.WithRefusal(ByDesign(ReasonIndexStatement))
	assert.Empty(t, v.Owner)
	js, err := v.JSON()
	require.NoError(t, err)
	assert.Contains(t, js, `"class": "by-design"`)
	assert.NotContains(t, js, `"owner"`)
}

// A refusal's typed cause is part of its identity, so stamping the refusal
// onto a verdict carries the cause into the JSON; a refusal without a cause
// emits no cause key.
func TestWithRefusalCarriesTypedCause(t *testing.T) {
	withCause := Verdict{}.WithRefusal(
		CapabilityBoundary(ReasonUnsupportedPartitionedParent).WithCause(CauseParentBlockingIndexBuild))
	assert.Equal(t, CauseParentBlockingIndexBuild, withCause.Cause)
	js, err := withCause.JSON()
	require.NoError(t, err)
	assert.Contains(t, js, `"cause": "parent-blocking-index-build"`)

	js, err = Verdict{}.WithRefusal(ByDesign(ReasonIndexStatement)).JSON()
	require.NoError(t, err)
	assert.NotContains(t, js, `"cause"`)
}

func TestRefusalRoundTripsThroughVerdict(t *testing.T) {
	want := NoOnlineSafetyProblem(ReasonUnsupportedStatement, OwnerProvisioning)
	got, err := Verdict{Statement: "GRANT SELECT ON t TO r"}.WithRefusal(want).Refusal()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	caused := CapabilityBoundary(ReasonUnsupportedPartitionedParent).WithCause(CauseParentBlockingIndexBuild)
	got, err = Verdict{}.WithRefusal(caused).Refusal()
	require.NoError(t, err)
	assert.Equal(t, CauseParentBlockingIndexBuild, got.Cause(), "the cause survives the round trip")

	_, err = Verdict{Outcome: OutcomeExecuted}.Refusal()
	require.Error(t, err, "an executed verdict carries no refusal")

	_, err = Verdict{Outcome: OutcomeRefused, Reason: ReasonTableTooLarge}.Refusal()
	require.Error(t, err, "a refused verdict without a class is not a classified refusal")

	_, err = Verdict{Outcome: OutcomeRefused, Reason: ReasonTableTooLarge, Class: ClassEnvironmental, Owner: OwnerProvisioning}.Refusal()
	require.Error(t, err, "an owner outside no-online-safety-problem violates RF-7")
}

func TestJSONRoundTrip(t *testing.T) {
	v := Verdict{
		Outcome:    OutcomeRefused,
		Reason:     ReasonBudgetExceeded,
		Statement:  "ALTER TABLE t ALTER COLUMN id TYPE bigint",
		Table:      "t",
		Detail:     "the optimistic attempt exceeded its statement budget",
		SaferIdiom: "ADD CONSTRAINT ... NOT VALID; VALIDATE CONSTRAINT",
		Attempts:   3,
	}
	s, err := v.JSON()
	require.NoError(t, err)

	var got Verdict
	require.NoError(t, json.Unmarshal([]byte(s), &got))
	assert.Equal(t, v, got)
}

func TestJSONRoundTripFailed(t *testing.T) {
	v := Verdict{
		Outcome:       OutcomeFailed,
		Code:          "budget-statement-exceeded",
		Statement:     "ALTER TABLE t ALTER COLUMN v SET NOT NULL",
		Table:         "t",
		FailedStep:    2,
		FailedStepSQL: "ALTER TABLE t VALIDATE CONSTRAINT c",
		ExecutedSQL:   []string{"ALTER TABLE t ADD CONSTRAINT c CHECK (v IS NOT NULL) NOT VALID"},
		Detail:        "step 2 of 4 failed; the committed step's state remains",
	}
	s, err := v.JSON()
	require.NoError(t, err)

	var got Verdict
	require.NoError(t, json.Unmarshal([]byte(s), &got))
	assert.Equal(t, v, got)
}

// The failed verdict's JSON keys are the machine contract automation reads;
// renaming a Go field must not silently rename a key.
func TestFailedJSONKeysArePinned(t *testing.T) {
	s, err := Verdict{
		Outcome:       OutcomeFailed,
		Code:          "execution-failed",
		Statement:     "ALTER TABLE t ALTER COLUMN v SET NOT NULL",
		FailedStep:    2,
		FailedStepSQL: "ALTER TABLE t VALIDATE CONSTRAINT c",
		ExecutedSQL:   []string{"ALTER TABLE t ADD CONSTRAINT c CHECK (v IS NOT NULL) NOT VALID"},
	}.JSON()
	require.NoError(t, err)
	for _, key := range []string{
		`"outcome": "failed"`, `"code"`, `"failed_step"`, `"failed_step_sql"`, `"executed_sql"`,
	} {
		assert.Contains(t, s, key)
	}
}

func TestJSONOmitsEmptyOptionalFields(t *testing.T) {
	s, err := Verdict{Outcome: OutcomeExecuted, Statement: "ALTER TABLE t ADD COLUMN x int"}.JSON()
	require.NoError(t, err)
	assert.NotContains(t, s, "reason")
	assert.NotContains(t, s, "table")
	assert.NotContains(t, s, "safer_idiom")
	assert.NotContains(t, s, "attempts")
	assert.NotContains(t, s, "code")
	assert.NotContains(t, s, "failed_step")
}

// Reason and Cause values are the machine contract automation switches on:
// flat kebab-case tokens, no spaces or colons — prose belongs in Detail.
func TestReasonAndCauseTokensAreFlat(t *testing.T) {
	toks := []string{
		string(CauseLockBudget), string(CauseStatementBudget),
		string(CauseParentBlockingIndexBuild), string(CauseParentConcurrentIndexBuild),
		string(CauseParentIndexAdoption), string(CauseParentNotValidForeignKey),
	}
	for _, r := range Reasons() {
		toks = append(toks, string(r))
	}
	for _, tok := range toks {
		assert.Regexp(t, `^[a-z0-9]+(-[a-z0-9]+)*$`, tok)
	}
}

// The wire tokens themselves are the contract, not just their shape: a
// renamed token ships a breaking change to every consumer switching on it,
// so the exact strings are pinned here.
func TestReasonsPinsWireTokens(t *testing.T) {
	var got []string
	for _, r := range Reasons() {
		got = append(got, string(r))
	}
	assert.Equal(t, []string{
		"unsupported-statement",
		"index-statement",
		"not-native-safe-table-too-large",
		"insufficient-privileges",
		"unsupported-partitioned-parent",
		"not-native-safe-budget-exceeded",
		"not-native-safe-rewrite-required",
		"backend-unavailable",
		"destructive-change",
		"plan-fingerprint-mismatch",
		"create-collision",
	}, got)
}

func TestStringExecuted(t *testing.T) {
	s := Verdict{
		Outcome:   OutcomeExecuted,
		Statement: "ALTER TABLE t ADD COLUMN x int",
		Table:     "t",
		Detail:    "committed within budget",
	}.String()
	assert.Contains(t, s, "executed natively")
	assert.Contains(t, s, "table:     t")
	assert.Contains(t, s, "ALTER TABLE t ADD COLUMN x int")
}

func TestStringRefusedIncludesReasonAndIdiom(t *testing.T) {
	s := Verdict{
		Outcome:    OutcomeRefused,
		Reason:     ReasonIndexStatement,
		Statement:  "CREATE INDEX i ON t (c)",
		SaferIdiom: "CREATE INDEX CONCURRENTLY i ON t (c)",
	}.String()
	assert.Contains(t, s, "refused (index-statement)")
	assert.Contains(t, s, "CREATE INDEX CONCURRENTLY")
}

func TestStringFailedIncludesCodeStepAndCommittedPrefix(t *testing.T) {
	s := Verdict{
		Outcome:       OutcomeFailed,
		Code:          "execution-failed",
		Statement:     "ALTER TABLE t ALTER COLUMN v SET NOT NULL",
		Table:         "t",
		FailedStep:    2,
		FailedStepSQL: "ALTER TABLE t VALIDATE CONSTRAINT c",
		ExecutedSQL:   []string{"ALTER TABLE t ADD CONSTRAINT c CHECK (v IS NOT NULL) NOT VALID"},
	}.String()
	assert.Contains(t, s, "failed (execution-failed)")
	assert.Contains(t, s, "failed at: step 2: ALTER TABLE t VALIDATE CONSTRAINT c")
	assert.Contains(t, s, "committed before the failure")
	assert.NotContains(t, s, "executed as:", "a committed prefix is not a completed substitution")
}

func TestStringIncludesAttemptsWhenSet(t *testing.T) {
	v := Verdict{
		Outcome:   OutcomeRefused,
		Reason:    ReasonBudgetExceeded,
		Statement: "ALTER TABLE t ADD COLUMN x int",
		Attempts:  3,
	}
	assert.Contains(t, v.String(), "attempts:  3")

	v.Attempts = 0
	assert.NotContains(t, v.String(), "attempts")
}
