package preflight

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// The server refusing the probe table is told apart by SQLSTATE: a
// server that accepts no writes, and a role without TEMPORARY, are each
// ErrAddColumnProbeUnavailable naming its own fix, and keep the server's
// error. Any other failure to create the table is not.
func TestProbeTableErrorNamesTheFix(t *testing.T) {
	cases := []struct {
		name        string
		code        string
		unavailable bool
		fix         string
	}{
		{"read-only server", sqlstateReadOnlySQLTransaction, true, "plan the change against the primary"},
		{"role without TEMPORARY", sqlstateInsufficientPrivilege, true, "grant TEMPORARY on the database"},
		{"disk full", "53100", false, "create the probe table"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := &pgconn.PgError{Code: tc.code, Message: "refused"}
			err := probeTableError(server)
			assert.Equal(t, tc.unavailable, errors.Is(err, ErrAddColumnProbeUnavailable))
			assert.ErrorIs(t, err, error(server))
			assert.Contains(t, err.Error(), tc.fix)
		})
	}
}
