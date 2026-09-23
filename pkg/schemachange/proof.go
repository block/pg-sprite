package schemachange

import "encoding/json"

// Proof is the plain view of a BuiltShadow: the same facts as its accessors,
// as exported fields a checkpoint can encode and decode. A resume decodes
// the checkpoint it kept into a Proof and compares it with the Proof of what
// InspectShadow returned; the two are equal exactly when the shadow the
// inspection found is the one the build produced, since every field is
// derived from the catalog the same way on both paths. Proof is a view only:
// nothing turns it back into a BuiltShadow, so the copier and cutover still
// accept only a value the builder or the inspection returned.
type Proof struct {
	// Schema is the schema holding both the source and the shadow.
	Schema string `json:"schema"`
	// SourceTable is the source table name.
	SourceTable string `json:"source_table"`
	// ShadowTable is the shadow table name.
	ShadowTable string `json:"shadow_table"`
	// SourceOID is the source relation's OID as resolved inside the build.
	SourceOID uint32 `json:"source_oid"`
	// ShadowOID is the shadow relation's OID as created by the build.
	ShadowOID uint32 `json:"shadow_oid"`
	// SourceFingerprint is the digest of the source's introspected model.
	SourceFingerprint string `json:"source_fingerprint"`
	// TargetFingerprint is the digest of the shadow's introspected model.
	TargetFingerprint string `json:"target_fingerprint"`
	// IdentityColumns are the source identity columns cutover hands over.
	IdentityColumns []IdentityColumn `json:"identity_columns"`
	// Fidelity is the metadata snapshot replicated onto the shadow.
	Fidelity FidelitySnapshot `json:"fidelity"`
	// CopyColumns are the columns the copier moves.
	CopyColumns []string `json:"copy_columns"`
}

// Proof returns the plain view of the built shadow: each field is what the
// accessor of the same name returns.
func (b BuiltShadow) Proof() Proof {
	return Proof{
		Schema:            b.schema,
		SourceTable:       b.source,
		ShadowTable:       b.shadow,
		SourceOID:         b.sourceOID,
		ShadowOID:         b.shadowOID,
		SourceFingerprint: b.sourceFingerprint,
		TargetFingerprint: b.targetFingerprint,
		IdentityColumns:   b.IdentityColumns(),
		Fidelity:          b.fidelity,
		CopyColumns:       b.CopyColumns(),
	}
}

// MarshalJSON encodes the built shadow as its Proof, so a checkpoint can
// store a BuiltShadow directly. BuiltShadow has no UnmarshalJSON on purpose:
// decode into a Proof instead.
func (b BuiltShadow) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.Proof())
}
