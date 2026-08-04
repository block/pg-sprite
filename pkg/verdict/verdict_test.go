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
	}
	s, err := v.JSON()
	require.NoError(t, err)

	var got Verdict
	require.NoError(t, json.Unmarshal([]byte(s), &got))
	assert.Equal(t, v, got)
}

func TestJSONOmitsEmptyOptionalFields(t *testing.T) {
	s, err := Verdict{Outcome: OutcomeExecuted, Statement: "ALTER TABLE t ADD COLUMN x int"}.JSON()
	require.NoError(t, err)
	assert.NotContains(t, s, "reason")
	assert.NotContains(t, s, "table")
	assert.NotContains(t, s, "safer_idiom")
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
