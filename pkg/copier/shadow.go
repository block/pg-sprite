package copier

import (
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
	// Conversions are the USING expressions the gated statement retyped
	// copy columns with, keyed by column, each the SQL text the copy
	// selects in the column's place; a column absent from the map is
	// copied bare. The map is empty when the statement carried no USING.
	Conversions() map[string]string
}

// checkShadow refuses a shadow proof that does not describe a copy into the
// proven target's shadow: a nil or empty value, a shadow built for another
// table, a shadow that is the source itself by name or by relation, a proof
// without the OIDs the per-chunk relation check compares against, or a
// column list that cannot carry the primary key, or a conversion for a
// column the copy does not carry or with no expression to select. The
// source and the shadow must be two relations because the copy reads one
// and the resume clear deletes from the other.
func checkShadow(target preflight.CopySwapTarget, shadow Shadow) error {
	// INV: ST-6
	if shadow == nil {
		return refuse(CauseShadowMismatch, nil, "copy requires a built shadow")
	}
	if shadow.ShadowTable() == "" {
		return refuse(CauseShadowMismatch, nil, "shadow proof is empty")
	}
	if shadow.Schema() != target.Schema() || shadow.SourceTable() != target.Table() {
		return refuse(CauseShadowMismatch, nil, "shadow is for %s.%s, proof is for %s.%s", shadow.Schema(), shadow.SourceTable(), target.Schema(), target.Table())
	}
	if shadow.ShadowTable() == shadow.SourceTable() {
		return refuse(CauseShadowMismatch, nil, "shadow of %s.%s is the source table itself", target.Schema(), target.Table())
	}
	if shadow.SourceOID() == 0 || shadow.ShadowOID() == 0 {
		return refuse(CauseShadowMismatch, nil, "shadow proof for %s.%s carries no relation OIDs", target.Schema(), target.Table())
	}
	if shadow.SourceOID() == shadow.ShadowOID() {
		return refuse(CauseShadowMismatch, nil, "shadow proof for %s.%s names relation %d as both source and shadow", target.Schema(), target.Table(), shadow.SourceOID())
	}
	if !slices.Contains(shadow.CopyColumns(), target.PKColumn()) {
		return refuse(CauseShadowMismatch, nil, "shadow copy columns for %s.%s do not include the primary key %s", target.Schema(), target.Table(), target.PKColumn())
	}
	// The chunk ranges, the watermark, the resume clear, and the conflict
	// target all speak the source's key values, so a converted key would
	// land rows under keys those ranges do not describe.
	if _, converted := shadow.Conversions()[target.PKColumn()]; converted {
		return refuse(CauseShadowMismatch, nil, "shadow proof for %s.%s converts the primary key %s", target.Schema(), target.Table(), target.PKColumn())
	}
	for column, expr := range shadow.Conversions() {
		if !slices.Contains(shadow.CopyColumns(), column) {
			return refuse(CauseShadowMismatch, nil, "shadow proof for %s.%s converts column %s, which the copy does not carry", target.Schema(), target.Table(), column)
		}
		if expr == "" {
			return refuse(CauseShadowMismatch, nil, "shadow proof for %s.%s converts column %s with an empty expression", target.Schema(), target.Table(), column)
		}
	}
	return nil
}
