package schemachange

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE classes of the faults the data itself raises when it is shaped
// into the shadow: a value the new type will not hold (22, data exception:
// a string too long, a number out of range, a cast that fails) and a
// constraint the rows break (23, integrity constraint violation: NOT NULL,
// a check, a unique key). The class is the first two characters of the
// five-character code.
const (
	sqlstateClassDataException       = "22"
	sqlstateClassIntegrityConstraint = "23"
)

// IsDataFault reports whether err is the server rejecting the data under
// the statement's new shape — a fault a rerun meets again on the same rows,
// as opposed to a lost connection, a cancelled context, or a lock the run
// could take next time. It is the predicate Runner.Run abandons a stopped
// run on, and the one a verdict's remedy reads.
func IsDataFault(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case sqlstateClassDataException, sqlstateClassIntegrityConstraint:
		return true
	default:
		return false
	}
}
