package copier

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only the server refusing a row on a unique index or an exclusion
// constraint is a collision the copier waits out; every other failure of a
// chunk is the first attempt's error.
func TestCollidedClassifiesByServerCode(t *testing.T) {
	cases := map[string]struct {
		err      error
		collided bool
	}{
		"unique violation":              {&pgconn.PgError{Code: "23505"}, true},
		"exclusion violation":           {&pgconn.PgError{Code: "23P01"}, true},
		"unique violation when wrapped": {fmt.Errorf("copy chunk: %w", &pgconn.PgError{Code: "23505"}), true},
		"check violation":               {&pgconn.PgError{Code: "23514"}, false},
		"not-null violation":            {&pgconn.PgError{Code: "23502"}, false},
		"lock timeout":                  {&pgconn.PgError{Code: "55P03"}, false},
		"no error":                      {nil, false},
		"not a server error":            {errors.New("connection reset"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.collided, collided(tc.err))
		})
	}
}

func TestCopierCollisionOptionsDefault(t *testing.T) {
	opts := Options{}.withDefaults()
	assert.Equal(t, DefaultCollisionAttempts, opts.CollisionAttempts)
	assert.Equal(t, DefaultCollisionBackoff, opts.CollisionBackoff)
	require.NoError(t, opts.validate())

	given := Options{CollisionAttempts: 3, CollisionBackoff: 20 * time.Millisecond}.withDefaults()
	assert.Equal(t, 3, given.CollisionAttempts)
	assert.Equal(t, 20*time.Millisecond, given.CollisionBackoff)
	require.NoError(t, given.validate())
}

// An attempt count below one would never run the chunk, and a negative
// backoff is not a wait; both are refused rather than defaulted.
func TestCopierCollisionOptionsRefuseUnboundedValues(t *testing.T) {
	assert.ErrorIs(t, Options{CollisionAttempts: -1}.withDefaults().validate(), ErrInvalidOptions)
	assert.ErrorIs(t, Options{CollisionBackoff: -time.Millisecond}.withDefaults().validate(), ErrInvalidOptions)
}
