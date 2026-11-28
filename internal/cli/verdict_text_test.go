package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

// A verdict with no statement, such as a row security run with nothing to
// change, prints no statement line. One with a statement names it.
func TestVerdictTextPrintsAStatementLineOnlyForAStatement(t *testing.T) {
	var text strings.Builder
	require.NoError(t, writeVerdictText(&text, palette{}, verdict.Verdict{Outcome: verdict.OutcomeExecuted, Table: "public.documents"}))
	assert.Equal(t, "executed natively\n  table:     public.documents\n", text.String())

	text.Reset()
	require.NoError(t, writeVerdictText(&text, palette{}, verdict.Verdict{Outcome: verdict.OutcomeExecuted, Table: "public.documents", Statement: "ALTER TABLE documents ENABLE ROW LEVEL SECURITY"}))
	assert.Equal(t, "executed natively\n  table:     public.documents\n  statement: ALTER TABLE documents ENABLE ROW LEVEL SECURITY\n", text.String())
}
