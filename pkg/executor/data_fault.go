package executor

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE classes of the faults the table's rows themselves raise: a value
// an expression or type will not take (22, data exception) and a constraint
// the rows break (23, integrity constraint violation — a unique build over
// duplicate rows is 23505). The class is the first two characters of the
// five-character code.
const (
	sqlstateClassDataException       = "22"
	sqlstateClassIntegrityConstraint = "23"
)

// catalogSchema is the schema of the server's own catalogs, which a
// violation names when the write it refused was to a catalog row.
const catalogSchema = "pg_catalog"

// isDataFault reports whether err is the server rejecting the table's rows,
// a fault every rerun of the same statement meets again until the data is
// corrected. It is the same rule as pkg/schemachange's IsDataFault, copied
// because that package imports this one.
func isDataFault(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case sqlstateClassDataException, sqlstateClassIntegrityConstraint:
		return true
	default:
		return false
	}
}
