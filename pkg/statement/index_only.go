package statement

import "errors"

// ErrNotCreateIndex is returned when an index definition is expected and
// sql is anything other than a single CREATE INDEX.
var ErrNotCreateIndex = errors.New("statement is not CREATE INDEX")

// IndexOnWholeTable returns a CREATE INDEX definition with ONLY removed
// from its ON clause, reprinted through the PostgreSQL deparser.
// pg_get_indexdef prints every index on a partitioned parent as
// CREATE INDEX ... ON ONLY, whether or not its partitions carry the index;
// a desired file declares the parent's index for the whole table, which is
// what PostgreSQL builds for ON without ONLY. On a table with no partitions
// — the scratch schema a desired file is built in, and a table the file
// creates — the two forms build the same index, so a file rendered with
// this form introspects back to the definition it was rendered from. Only
// the target RangeVar's inheritance flag changes.
func IndexOnWholeTable(sql string) (string, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", err
	}
	index := node.GetIndexStmt()
	if index == nil || index.GetRelation() == nil {
		return "", ErrNotCreateIndex
	}
	index.Relation.Inh = true
	return deparseOne(node)
}
