package checkpoint

import "github.com/jackc/pgx/v5/pgtype"

// statementColumn maps the statement onto the nullable text column: NULL
// when the checkpoint carries none, so a Save without one leaves the row's
// in place rather than clearing it.
func statementColumn(sql string) pgtype.Text {
	return pgtype.Text{String: sql, Valid: sql != ""}
}

// replacesStatement reports whether a Save of cp would put another
// statement's text on a row that already holds one — the one thing the
// statement column refuses after it is first written. A cp without a
// statement, or a row without one, replaces nothing.
func replacesStatement(stored row, cp Checkpoint) bool {
	// INV: ST-2
	if cp.Statement == "" || !stored.statement.Valid {
		return false
	}
	return stored.statement.String != cp.Statement
}
