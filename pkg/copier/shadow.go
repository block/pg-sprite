package copier

import (
	"fmt"
	"slices"

	"github.com/block/pg-sprite/pkg/preflight"
)

// Shadow is the proof the copier writes into: the shadow table the shadow
// builder created for a proven source, with the relation OIDs the builder
// observed and the columns the two tables share. The copier names the shape
// rather than the builder's type because the builder imports the copier, not
// the other way round; only the builder mints a value whose OIDs are set.
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
// table, a proof without the OIDs the per-chunk relation check compares
// against, or a column list that cannot carry the primary key.
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
	if shadow.SourceOID() == 0 || shadow.ShadowOID() == 0 {
		return fmt.Errorf("%w (ST-6): shadow proof for %s.%s carries no relation OIDs", ErrInvariantViolation, target.Schema(), target.Table())
	}
	if !slices.Contains(shadow.CopyColumns(), target.PKColumn()) {
		return fmt.Errorf("%w (ST-6): shadow copy columns for %s.%s do not include the primary key %s", ErrInvariantViolation, target.Schema(), target.Table(), target.PKColumn())
	}
	return nil
}
