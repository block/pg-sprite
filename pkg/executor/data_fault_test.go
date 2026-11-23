package executor_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The index build's data-fault rule is a copy of schemachange.IsDataFault,
// since that package imports this one. The two must agree on every
// SQLSTATE, or one failure would get one code on the copy-and-swap path
// and another on the index-build path.
func TestDataFaultRuleMatchesSchemachange(t *testing.T) {
	for _, code := range []string{"22001", "22012", "22P02", "23502", "23503", "23505", "23514", "23P01",
		"40001", "40P01", "42P07", "55006", "57014", "53100", "08006", "XX000", "2", ""} {
		build := &pgconn.PgError{Code: code}
		own := &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Build: build, Cleanup: executor.ErrBuildLeftInvalidIndex}
		assert.Equal(t, schemachange.IsDataFault(build), executor.OutcomeCode(own) == executor.CodeDataFault, "SQLSTATE %q", code)
	}
}
