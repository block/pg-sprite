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

// CopySwapTarget proves ST-6 prerequisites for a copy-and-swap target. Its
// zero value is forgeable; consumers must reject it when Table is empty.
type CopySwapTarget struct {
	schema, table, pkColumn string
	pkType                  PKType
	ownerRole               string
}

// Schema returns the target schema.
func (t CopySwapTarget) Schema() string { return t.schema }

// Table returns the target table.
func (t CopySwapTarget) Table() string { return t.table }

// PKColumn returns the primary-key column.
func (t CopySwapTarget) PKColumn() string { return t.pkColumn }

// PKType returns the primary-key type.
func (t CopySwapTarget) PKType() PKType { return t.pkType }

// OwnerRole returns the catalog-resolved owner of the target table: the role
// the shadow builder runs SET ROLE to so that the shadow table and its
// dependents are created owner-correct, and whose SET-usable membership the
// Tier 3 privilege check proved for the connected role.
func (t CopySwapTarget) OwnerRole() string { return t.ownerRole }
