package executor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Scratch SQLSTATE class 42 means an invalid definition or insufficient access,
// including unresolved roles, columns, and helper functions. Retrying the same
// declaration cannot fix it. Operational failures retain their original class.
func classifyRowSecurityDesiredError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "42") {
		return fmt.Errorf("%w: %w", ErrRowSecurityRefused, err)
	}
	return err
}
