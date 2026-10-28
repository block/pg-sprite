package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// IsDataFault answers for the two SQLSTATE classes the data itself raises
// when it is shaped into the shadow, however deep the server's error is
// wrapped, and for nothing else: a server fault outside those classes, a
// cancellation, and a plain error are stops a rerun clears.
func TestIsDataFaultMatchesTheTwoDataClassesOnly(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"22001 string_data_right_truncation": {
			err:  &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(2)"},
			want: true,
		},
		"22003 numeric_value_out_of_range, wrapped twice": {
			err:  fmt.Errorf("copy chunk: %w", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "22003"})),
			want: true,
		},
		"23502 not_null_violation, joined beside a cancellation": {
			err:  errors.Join(context.Canceled, &pgconn.PgError{Code: "23502"}),
			want: true,
		},
		"23505 unique_violation": {
			err:  &pgconn.PgError{Code: "23505"},
			want: true,
		},
		"57P01 admin_shutdown is the server going away, not the data": {
			err:  &pgconn.PgError{Code: "57P01"},
			want: false,
		},
		"55006 object_in_use is the write fence refusing, not the data": {
			err:  &pgconn.PgError{Code: schemachange.WriteFenceSQLState},
			want: false,
		},
		"a cancelled context": {
			err:  context.Canceled,
			want: false,
		},
		"a code too short to carry a class": {
			err:  &pgconn.PgError{Code: "2"},
			want: false,
		},
		"a plain error": {
			err:  errors.New("connection reset"),
			want: false,
		},
		"nil": {
			err:  nil,
			want: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, schemachange.IsDataFault(tc.err))
		})
	}
}
