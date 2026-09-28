package copier

import (
	"fmt"
	"slices"

	"github.com/block/pg-sprite/pkg/preflight"
)

// Shadow is the proof the copier writes into: the shadow table the shadow
// builder created for a proven source, with the relation OIDs the builder
// observed and the columns the two tables share. The copier names the shape
// rather than the builder's type because pkg/schemachange, which owns the
// builder and the orchestrator that runs the copy, imports every producer
// and no producer imports it; only the builder and the shadow inspection
// mint a value whose OIDs are set. What the shape cannot promise, checkShadow
// verifies before a connection is opened.
type Shadow interface {
	// Schema is the schema holding both the source and the shadow.
	Schema() string
	// SourceTable is the table being copied from.
	SourceTable() string
	// ShadowTable is the table being copied into.
	ShadowTable() string
	// SourceOID is pg_class.oid of the source when the shadow was built.
	SourceOID() uint32
	// ShadowOID is pg_class.oid of the shadow when it was built.
	ShadowOID() uint32
	// CopyColumns are the columns present in both tables, in source order.
	CopyColumns() []string
}

// checkShadow refuses a shadow proof that does not describe a copy into the
// proven target's shadow: a nil or empty value, a shadow built for another
// table, a shadow that is the source itself by name or by relation, a proof
// without the OIDs the per-chunk relation check compares against, or a
// column list that cannot carry the primary key. The source and the shadow
// must be two relations because the copy reads one and the resume clear
// deletes from the other.
func checkShadow(target preflight.CopySwapTarget, shadow Shadow) error {
	// INV: ST-6
	if shadow == nil {
		return fmt.Errorf("%w (ST-6): copy requires a built shadow", ErrInvariantViolation)
	}
	if shadow.ShadowTable() == "" {
		return fmt.Errorf("%w (ST-6): shadow proof is empty", ErrInvariantViolation)
	}
	if shadow.Schema() != target.Schema() || shadow.SourceTable() != target.Table() {
		return fmt.Errorf("%w (ST-6): shadow is for %s.%s, proof is for %s.%s", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), target.Schema(), target.Table())
	}
	if shadow.ShadowTable() == shadow.SourceTable() {
		return fmt.Errorf("%w (ST-6): shadow of %s.%s is the source table itself", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == 0 || shadow.ShadowOID() == 0 {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s carries no relation OIDs", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if shadow.SourceOID() == shadow.ShadowOID() {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s names relation %d as both source and shadow", ErrInvariantViolation, target.Schema(), target.Table(), shadow.SourceOID())
	}
	if !slices.Contains(shadow.CopyColumns(), target.PKColumn()) {
		return fmt.Errorf("%w (ST-6): shadow copy columns for %s.%s do not include the primary key %s", ErrInvariantViolation, target.Schema(), target.Table(), target.PKColumn())
	}
	return nil
}
