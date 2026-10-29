package dbconn

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// Only protocol_violation on the probe's BEGIN is read as a statement-mode
// pooler's refusal. Any other connection-class SQLSTATE, and a connection
// that drops without one, is a real failure to reach the server and must stay
// untyped, so an operator is not told to change a pooler setting for a
// network fault.
func TestIsTransactionBlockRefusal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "statement-mode pooler refuses the transaction block",
			err: fmt.Errorf("begin: %w", &pgconn.PgError{
				Severity: "FATAL",
				Code:     "08P01",
				Message:  "transaction blocks not allowed in statement pooling mode",
			}),
			want: true,
		},
		{
			name: "connection failure with another connection-class SQLSTATE",
			err:  &pgconn.PgError{Severity: "FATAL", Code: "08006"},
			want: false,
		},
		{
			name: "admin shutdown",
			err:  &pgconn.PgError{Severity: "FATAL", Code: "57P01"},
			want: false,
		},
		{
			name: "connection dropped without a SQLSTATE",
			err:  fmt.Errorf("begin: %w", io.ErrUnexpectedEOF),
			want: false,
		},
		{
			name: "no error",
			err:  nil,
			want: false,
		},
		{
			name: "plain error",
			err:  errors.New("something else"),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransactionBlockRefusal(tc.err))
		})
	}
}
