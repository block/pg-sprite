package migrate

import (
	"fmt"
	"log/slog"

	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// checkQuiescedAck confirms that the quiesced acknowledgement names the
// resolved target table, exactly as the force and restart acknowledgements
// must. A quiesced run refuses every write to the table for its whole
// length, so a typed table name is what stops the declaration from being
// left on a script and taking the next table offline by accident.
func checkQuiescedAck(st statement.Statement, ack string) error {
	if ack == qualified(st) {
		return nil
	}
	return fmt.Errorf("the quiesced acknowledgement must name the resolved target table %q, got %q; nothing was executed",
		qualified(st), ack)
}

// auditQuiesced records, before the fence goes on, that the operator took
// the table offline for writes. The record is warn-level and
// unconditional, as the force and restart audits are: an audit trail must
// not depend on diagnostics being enabled.
func auditQuiesced(audit *slog.Logger, st statement.Statement) {
	audit.Warn("quiesced run acknowledged; every write to the table will be refused until the swap",
		"table", qualified(st),
		"sqlstate", schemachange.WriteFenceSQLState)
}
