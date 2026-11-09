package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrPolicyRecursion refuses policies that read a table whose own policies,
// directly or through other tables and views, read the target table back.
// PostgreSQL accepts such policies, then fails every query on the table that
// they apply to with "infinite recursion detected in policy".
var ErrPolicyRecursion = errors.New("a policy cannot read a table whose policies read this table back")

// policyRecursionQuery returns the 1-based position in $1 of the first read
// relation whose policies or view definitions lead back to the relation $2,
// following the relation dependencies PostgreSQL records for every policy
// expression and view rule. Policies count whether or not their table has
// row security enabled, so enabling it later cannot close a cycle this
// check let through. A read relation that does not exist leads nowhere.
const policyRecursionQuery = `
WITH RECURSIVE edge(src, dst) AS (
    SELECT p.polrelid, d.refobjid
    FROM pg_catalog.pg_policy p
    JOIN pg_catalog.pg_depend d
      ON d.classid = 'pg_catalog.pg_policy'::pg_catalog.regclass AND d.objid = p.oid
    WHERE d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
    UNION ALL
    SELECT r.ev_class, d.refobjid
    FROM pg_catalog.pg_rewrite r
    JOIN pg_catalog.pg_depend d
      ON d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND d.objid = r.oid
    WHERE d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
),
target(oid) AS (
    SELECT pg_catalog.to_regclass($2)::pg_catalog.oid
),
reach(origin, rel) AS (
    SELECT read.position, pg_catalog.to_regclass(read.name)::pg_catalog.oid
    FROM pg_catalog.unnest($1::pg_catalog.text[]) WITH ORDINALITY AS read(name, position)
    UNION
    SELECT reach.origin, edge.dst
    FROM reach
    JOIN edge ON edge.src = reach.rel
    WHERE reach.rel <> (SELECT oid FROM target)
)
SELECT origin FROM reach WHERE rel = (SELECT oid FROM target) ORDER BY origin LIMIT 1`

// refusePolicyRecursion refuses desired policies on schema.table that read a
// relation whose policies read schema.table back. It runs while the caller
// holds ACCESS EXCLUSIVE on the target: a new policy or view that reads the
// target needs a lock on it, so none can be committed between this check and
// the caller's commit.
func refusePolicyRecursion(ctx context.Context, tx pgx.Tx, schema string, desired statement.DesiredWithRowSecurity) error {
	reads := desired.PolicyReads()
	if len(reads) == 0 {
		return nil
	}
	names := make([]string, len(reads))
	for i, read := range reads {
		names[i] = pgx.Identifier{read.Schema, read.Name}.Sanitize()
	}
	target := pgx.Identifier{schema, desired.Table()}.Sanitize()
	var origin int64
	err := tx.QueryRow(ctx, policyRecursionQuery, names, target).Scan(&origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check policies on %s for recursion: %w", target, err)
	}
	if origin < 1 || int(origin) > len(reads) {
		return fmt.Errorf("check policies on %s for recursion: read position %d is outside the %d tables the policies read", target, origin, len(reads))
	}
	return fmt.Errorf("%w: %w: the policies on %s read %s, whose policies read %s back",
		ErrRowSecurityRefused, ErrPolicyRecursion, target, reads[origin-1], target)
}
