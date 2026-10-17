package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func parseStatement(t *testing.T, sql string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	return st
}

// DETACH PARTITION is substituted to its concurrent form, which the sequence
// executor does not drive; the dry run learns the refusal from the routed
// step and names the table the way the apply does.
func TestAdmitRoutedStepsRefusesConcurrentDetach(t *testing.T) {
	st := parseStatement(t, "ALTER TABLE billing.invoices DETACH PARTITION billing.invoices_2024")

	v, refused, err := AdmitRoutedSteps(st, []string{
		"ALTER TABLE billing.invoices DETACH PARTITION billing.invoices_2024 CONCURRENTLY",
	})

	require.NoError(t, err)
	require.True(t, refused)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, "billing.invoices", v.Table)
}

// An unnamed index build is refused whether or not the table is qualified:
// the name check precedes the qualification check in the build admission.
func TestAdmitRoutedStepsRefusesUnnamedIndexBuild(t *testing.T) {
	st := parseStatement(t, "CREATE INDEX ON invoices (amount)")

	v, refused, err := AdmitRoutedSteps(st, []string{"CREATE INDEX CONCURRENTLY ON invoices (amount)"})

	require.NoError(t, err)
	require.True(t, refused)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, "public.invoices", v.Table, "an unqualified target is reported in the schema the dry run plans against")
}

// IF NOT EXISTS is a permanent design refusal, so it keeps its by-design class.
func TestAdmitRoutedStepsKeepsTheIfNotExistsClass(t *testing.T) {
	st := parseStatement(t, "CREATE INDEX IF NOT EXISTS invoices_amount_idx ON billing.invoices (amount)")

	v, refused, err := AdmitRoutedSteps(st, []string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS invoices_amount_idx ON billing.invoices (amount)",
	})

	require.NoError(t, err)
	require.True(t, refused)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
}

// The apply resolves an unqualified table before its executor admits the
// steps, so a named index build on one must not be refused for lacking a
// schema qualification.
func TestAdmitRoutedStepsAdmitsNamedIndexBuildOnUnqualifiedTable(t *testing.T) {
	st := parseStatement(t, "CREATE INDEX invoices_amount_idx ON invoices (amount)")

	_, refused, err := AdmitRoutedSteps(st, []string{
		"CREATE INDEX CONCURRENTLY invoices_amount_idx ON invoices (amount)",
	})

	require.NoError(t, err)
	assert.False(t, refused)
}

func TestAdmitRoutedStepsAdmitsAnOrdinaryAlter(t *testing.T) {
	st := parseStatement(t, "ALTER TABLE billing.invoices ADD COLUMN note text")

	_, refused, err := AdmitRoutedSteps(st, []string{"ALTER TABLE billing.invoices ADD COLUMN note text"})

	require.NoError(t, err)
	assert.False(t, refused)
}

// A failure outside the refusal contract is an operational error, exactly as
// it is where the apply runs admission: nothing is reported as a refusal.
func TestAdmitRoutedStepsReturnsNonRefusalFailuresAsErrors(t *testing.T) {
	st := parseStatement(t, "ALTER TABLE billing.invoices ADD COLUMN note text")

	_, refused, err := AdmitRoutedSteps(st, nil)

	require.Error(t, err)
	assert.False(t, refused)
}
