package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

// A failed verdict carries the SQLSTATE and primary message of the server
// error wrapped anywhere in the failure, and never its DETAIL line, which
// quotes the row the server rejected.
func TestWithServerErrorRecordsOnlyThePrimaryMessage(t *testing.T) {
	pgErr := &pgconn.PgError{
		Code:    "23514",
		Message: `new row for relation "orders" violates check constraint "orders_qty_ck"`,
		Detail:  "Failing row contains (7, -3, secret-value).",
	}
	err := fmt.Errorf("execute step 2 of 3: %w", pgErr)

	v := WithServerError(verdict.Verdict{Table: "public.orders", Outcome: verdict.OutcomeFailed}, err)
	assert.Equal(t, "23514", v.SQLState)
	assert.Equal(t, pgErr.Message, v.ServerError)

	out, err := json.Marshal(v)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"sqlstate":"23514"`)
	assert.Contains(t, string(out), `"server_error":"new row for relation \"orders\" violates check constraint \"orders_qty_ck\""`)
	assert.NotContains(t, string(out), "Failing row contains")
	assert.NotContains(t, string(out), "secret-value")
	assert.NotContains(t, v.String(), "secret-value")
}

// A verdict that did not fail, or a failure with no server error in its
// chain, is returned unchanged.
func TestWithServerErrorLeavesOtherVerdictsAlone(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	refused := verdict.Verdict{Table: "public.orders", Outcome: verdict.OutcomeRefused}
	assert.Equal(t, refused, WithServerError(refused, pgErr))

	failed := verdict.Verdict{Table: "public.orders", Outcome: verdict.OutcomeFailed, Code: "commit-outcome-unknown"}
	assert.Equal(t, failed, WithServerError(failed, errors.New("connection reset by peer")))
}
