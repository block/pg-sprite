package preflight

// PKType identifies a supported single-column integer primary-key type.
type PKType string

const (
	// PKSmallint is PostgreSQL smallint.
	PKSmallint PKType = "smallint"
	// PKInteger is PostgreSQL integer.
	PKInteger PKType = "integer"
	// PKBigint is PostgreSQL bigint.
	PKBigint PKType = "bigint"
)

// copySwapShape holds the table facts the copy-and-swap route is proven
// against. Both proofs below embed it unexported, so its accessors are
// theirs and neither proof can be assembled outside this package.
type copySwapShape struct {
	database, schema, table, pkColumn string
	pkType                            PKType
	ownerRole                         string
	oid                               uint32
}

// CopySwapShape proves that a table has the shape the copy-and-swap route
// supports and that the connected role holds the copy-and-swap tier against
// it. It is minted only by CheckCopySwapShape and is the sole admission to
// CheckCopySwapEnvironment, which mints the CopySwapTarget every writing
// step requires: the three checks cannot be reordered or skipped by type.
// The role proof's replication bit lives here, not on the shared facts, so
// the target answers "may this run decode WAL" through DecodesWAL alone.
type CopySwapShape struct {
	copySwapShape
	logicalDecoding bool
}

// LogicalDecoding reports whether the privilege proof this shape was minted
// from verified replication access, so a run that decodes WAL can be refused
// as a proof mismatch when it was not.
func (s CopySwapShape) LogicalDecoding() bool { return s.logicalDecoding }

// CopySwapTarget proves ST-6 prerequisites for a copy-and-swap target: the
// shape a CopySwapShape proves and, on top of it, that the cluster and the
// volume were verified for the run's mode. It is minted only by
// CheckCopySwapEnvironment (or CheckCopySwap, which folds the three
// checks). Its zero value is forgeable; consumers must reject it when Table
// is empty.
type CopySwapTarget struct {
	copySwapShape
	decodesWAL bool
}

// DecodesWAL reports whether the environment was verified for a run that
// captures changes through a replication slot. A quiesced run's target
// reports false: no slot was proven available, so none may be created.
func (t CopySwapTarget) DecodesWAL() bool { return t.decodesWAL }

// Database returns the catalog-resolved database the target lives in.
func (s copySwapShape) Database() string { return s.database }

// Schema returns the target schema.
func (s copySwapShape) Schema() string { return s.schema }

// DecodingName returns the name of the replication slot and publication the
// route creates for this target, derived from its database, schema, and
// table (see CopySwapDecodingName).
func (s copySwapShape) DecodingName() string {
	return CopySwapDecodingName(s.database, s.schema, s.table)
}

// Table returns the target table.
func (s copySwapShape) Table() string { return s.table }

// PKColumn returns the primary-key column.
func (s copySwapShape) PKColumn() string { return s.pkColumn }

// PKType returns the primary-key type.
func (s copySwapShape) PKType() PKType { return s.pkType }

// OwnerRole returns the catalog-resolved owner of the target table: the role
// the shadow builder runs SET LOCAL ROLE to so that the shadow table and its
// dependents are created owner-correct, and whose SET-usable membership the
// Tier 3 privilege check proved for the connected role.
func (s copySwapShape) OwnerRole() string { return s.ownerRole }

// zero reports whether the shape was never minted by CheckCopySwapShape.
func (s copySwapShape) zero() bool { return s.table == "" || s.oid == 0 }
