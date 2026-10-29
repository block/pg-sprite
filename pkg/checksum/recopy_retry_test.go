package checksum

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// A collision is the server's own refusal under a unique index or an
// exclusion constraint, found through any wrapping; every other error,
// from the server or not, is not one.
func TestCollidedClassifiesByServerCode(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"unique violation":            {&pgconn.PgError{Code: sqlstateUniqueViolation}, true},
		"exclusion violation":         {&pgconn.PgError{Code: sqlstateExclusionViolation}, true},
		"wrapped unique violation":    {fmt.Errorf("recopy chunk: %w", &pgconn.PgError{Code: sqlstateUniqueViolation}), true},
		"undefined table":             {&pgconn.PgError{Code: sqlstateUndefinedTable}, false},
		"not a server error":          {errors.New("connection reset"), false},
		"lock lost without the cause": {fmt.Errorf("%w (LK-1): table lock was lost", ErrInvariantViolation), false},
		"nil":                         {nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, collided(tc.err))
		})
	}
}
