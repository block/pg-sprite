package statement

import "fmt"

// UniqueIndexKey returns the uniqueness rule a CREATE UNIQUE INDEX enforces,
// as a statement two indexes share exactly when they reject duplicates over
// the same key: the statement with its name, predicate, INCLUDE columns,
// CONCURRENTLY, and IF NOT EXISTS cleared. An index whose predicate narrows
// that key's rule to some rows has the same key as one without it. A CREATE
// INDEX that is not UNIQUE has no such rule and returns ok false.
func UniqueIndexKey(sql string) (key string, ok bool, err error) {
	node, err := parseSingle(sql)
	if err != nil {
		return "", false, err
	}
	ix := node.GetIndexStmt()
	if ix == nil {
		return "", false, fmt.Errorf("%w: %q", ErrNotCreateIndex, sql)
	}
	if !ix.GetUnique() {
		return "", false, nil
	}
	ix.Idxname = ""
	ix.WhereClause = nil
	ix.IndexIncludingParams = nil
	ix.Concurrent = false
	ix.IfNotExists = false
	key, err = deparseOne(node)
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}
