package checkpoint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Unfinished is one checkpoint row in a non-terminal phase, as a status
// reader shows it: a run in progress, or a stopped run whose leftovers
// stand until the same statement resumes it or a discard drops them.
type Unfinished struct {
	Schema string
	Table  string
	// Phase is the stored phase name. It is the row's own text, not a
	// Phase, since a row a newer engine left can carry a phase this engine
	// does not know, and such a row is unfinished too.
	Phase string
	// SlotName is the replication slot the row names, empty for a run that
	// decodes no WAL.
	SlotName string
	// Statement is the SQL text the run is carrying out, empty in a row an
	// engine that did not record it wrote.
	Statement string
	// UpdatedAt is when the row was last saved.
	UpdatedAt time.Time
}

// unfinishedSQL lists every row in a non-terminal phase, judged as the
// reaper judges a slot's row (nonTerminalPhaseSQL), in table order. The
// statement column is NULL in a row an engine that did not record it
// wrote.
const unfinishedSQL = `
SELECT schema_name, table_name, phase, slot_name, statement, updated_at
FROM ` + tableIdent + `
WHERE ` + nonTerminalPhaseSQL + `
ORDER BY schema_name, table_name`

// Unfinished lists every checkpoint row of the store's database in a
// non-terminal phase, whatever its row format, without the table lock: a
// snapshot for a reader to show, not a decision any write may rest on. A
// database where Ensure has never run is ErrTableMissing, and a table an
// older engine shaped is ErrTableBehind; a role that may not read the
// table gets the server's error.
func (s *Store) Unfinished(ctx context.Context) ([]Unfinished, error) {
	rows, err := s.pool.Query(ctx, unfinishedSQL, terminalPhaseNames()...)
	if err != nil {
		return nil, fmt.Errorf("list unfinished checkpoint rows: %w", tableUnusable(err))
	}
	unfinished, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Unfinished, error) {
		var u Unfinished
		var statement pgtype.Text
		err := row.Scan(&u.Schema, &u.Table, &u.Phase, &u.SlotName, &statement, &u.UpdatedAt)
		u.Statement = statement.String
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("list unfinished checkpoint rows: %w", tableUnusable(err))
	}
	return unfinished, nil
}
