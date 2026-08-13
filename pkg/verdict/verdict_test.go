package verdict

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	for _, tok := range []string{
		string(ReasonUnsupportedStatement),
		string(ReasonIndexStatement),
		string(ReasonTableTooLarge),
		string(ReasonBudgetExceeded),
		string(ReasonInsufficientPrivileges),
		string(CauseLockBudget),
		string(CauseStatementBudget),
	} {
		assert.Regexp(t, `^[a-z0-9]+(-[a-z0-9]+)*$`, tok)
	}
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
