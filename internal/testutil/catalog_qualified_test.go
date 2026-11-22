package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// reports records what AssertCatalogQualified reports instead of failing.
type reports struct {
	got []string
}

func (r *reports) Helper() {}

func (r *reports) Errorf(format string, args ...any) {
	r.got = append(r.got, fmt.Sprintf(format, args...))
}

// A subquery in FROM or JOIN, LATERAL or not, names no relation of its own,
// so a fully qualified read with one passes.
func TestCatalogQualifiedPassesASubqueryInFromOrJoin(t *testing.T) {
	r := &reports{}
	AssertCatalogQualified(r, `
	SELECT pos.write_lsn, extra.rendered
	FROM pg_catalog.pg_replication_slots s
	CROSS JOIN (
	       SELECT pg_catalog.pg_current_wal_lsn() AS write_lsn
	       ) AS pos
	CROSS JOIN LATERAL (SELECT pg_catalog.to_jsonb(s) AS rendered) AS extra
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	assert.Empty(t, r.got)
}

// A parenthesised join is not a subquery: the relation it opens with is a
// relation, reported when bare and passed when qualified.
func TestCatalogQualifiedReportsABareRelationInAParenthesisedJoin(t *testing.T) {
	bare := &reports{}
	AssertCatalogQualified(bare, `
	SELECT 1
	FROM (pg_replication_slots s CROSS JOIN pg_catalog.pg_class c)
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	assert.Len(t, bare.got, 1)
	assert.Contains(t, bare.got[0], `relation "pg_replication_slots" is not qualified`)

	qualified := &reports{}
	AssertCatalogQualified(qualified, `
	SELECT 1
	FROM (pg_catalog.pg_replication_slots s CROSS JOIN pg_catalog.pg_class c)
	WHERE s.slot_name OPERATOR(pg_catalog.=) $1`)
	assert.Empty(t, qualified.got)
}
