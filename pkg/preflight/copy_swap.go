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

// OwnerRole returns the table owner role.
func (t CopySwapTarget) OwnerRole() string { return t.ownerRole }
