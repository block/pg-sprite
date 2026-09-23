package executor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestRowSecurityDesiredOperationalErrorsRemainRetryable(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		&pgconn.PgError{Code: "57014"}, // query cancelled
		&pgconn.PgError{Code: "55P03"}, // lock timeout
		&pgconn.PgError{Code: "40001"}, // serialization failure
	} {
		actual := classifyRowSecurityDesiredError(err)
		assert.Equal(t, err, actual)
		assert.False(t, OutcomeCode(actual).Permanent())
	}
}
