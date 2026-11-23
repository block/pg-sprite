package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The partition tree under a partitioned table and the proofs that a listed
// partition, or a relation resolved by name, still has the identity it was
// read with. Every catalog read here runs under catalogReadPath, and every
// relation and operator is pg_catalog-qualified (CO-9).

// partitionNode is one partition in the tree under the target, as the catalog
// lists it. relkind is 'r' for a leaf, 'p' for a sub-partitioned partition, and
// 'f' for a foreign table.
type partitionNode struct {
	oid       uint32
	parentOID uint32
	schema    string
	name      string
	relkind   string
}

func (n partitionNode) subPartitioned() bool { return n.relkind == "p" }
func (n partitionNode) foreign() bool        { return n.relkind == "f" }
func (n partitionNode) qualified() string    { return qualifiedName(n.schema, n.name) }

// listPartitionTree returns every partition under the table with the given
// OID, at every depth, parents before their children.
func listPartitionTree(ctx context.Context, q querier, tableOID uint32) ([]partitionNode, error) {
	rows, err := q.Query(ctx, listPartitionTreeSQL, tableOID)
	if err != nil {
		return nil, fmt.Errorf("list partitions of table %d: %w", tableOID, err)
	}
	defer rows.Close()
	var nodes []partitionNode
	for rows.Next() {
		var n partitionNode
		if err := rows.Scan(&n.oid, &n.parentOID, &n.schema, &n.name, &n.relkind); err != nil {
			return nil, fmt.Errorf("scan partitions of table %d: %w", tableOID, err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list partitions of table %d: %w", tableOID, err)
	}
	return nodes, nil
}

const listPartitionTreeSQL = `WITH RECURSIVE tree AS (
		        SELECT h.inhrelid AS oid, h.inhparent AS parent, 1 AS depth
		          FROM pg_catalog.pg_inherits h
		         WHERE h.inhparent OPERATOR(pg_catalog.=) $1
		        UNION ALL
		        SELECT h.inhrelid, h.inhparent, tree.depth OPERATOR(pg_catalog.+) 1
		          FROM pg_catalog.pg_inherits h
		          JOIN tree ON h.inhparent OPERATOR(pg_catalog.=) tree.oid)
		 SELECT tree.oid, tree.parent, n.nspname, c.relname, c.relkind::pg_catalog.text
		   FROM tree
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tree.oid
		   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  ORDER BY tree.depth, tree.oid`

// childrenOf returns the direct partitions of parentOID from a tree listing.
func childrenOf(tree []partitionNode, parentOID uint32) []partitionNode {
	var children []partitionNode
	for _, n := range tree {
		if n.parentOID == parentOID {
			children = append(children, n)
		}
	}
	return children
}

// foreignPartitions names every foreign table in the tree.
func foreignPartitions(tree []partitionNode) []string {
	var names []string
	for _, n := range tree {
		if n.foreign() {
			names = append(names, n.qualified())
		}
	}
	return names
}

const proveNodeSQL = `SELECT EXISTS (
		   SELECT 1
		     FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		     JOIN pg_catalog.pg_inherits h ON h.inhrelid OPERATOR(pg_catalog.=) c.oid
		    WHERE c.oid OPERATOR(pg_catalog.=) $1
		      AND n.nspname OPERATOR(pg_catalog.=) $2
		      AND c.relname OPERATOR(pg_catalog.=) $3
		      AND h.inhparent OPERATOR(pg_catalog.=) $4)`

func proveNodeIn(ctx context.Context, q querier, node partitionNode) error {
	present, err := nodeInTree(ctx, q, node)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: ST-7: partition %s (table %d) is no longer a partition under that name; the partition tree changed since it was listed",
			ErrInvariantViolation, node.qualified(), node.oid)
	}
	return nil
}

// nodeInTree reports whether the partition is still the relation, under the
// schema and name, and the direct partition of the parent, that it was
// listed as.
func nodeInTree(ctx context.Context, q querier, node partitionNode) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, proveNodeSQL, node.oid, node.schema, node.name, node.parentOID).Scan(&present); err != nil {
		return false, fmt.Errorf("prove partition %s: %w", node.qualified(), err)
	}
	return present, nil
}

// lockAndProveNode takes a bounded lock on a partition and proves, under it,
// that the partition is still in the tree. A detach needs an ACCESS EXCLUSIVE
// lock on the partition, which this lock excludes until the transaction ends,
// so a partition proved here cannot leave the tree before the statement that
// follows. The lock is the weakest one that excludes a detach, and it does not
// block the partition's writers. It is taken under the transaction's own
// lock_timeout (LK-2).
// INV: ST-7 — the statement runs against the partition that was proven.
func lockAndProveNode(ctx context.Context, tx pgx.Tx, node partitionNode) error {
	_, err := tx.Exec(ctx, "LOCK TABLE ONLY "+pgx.Identifier{node.schema, node.name}.Sanitize()+" IN SHARE UPDATE EXCLUSIVE MODE")
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateUndefinedTable {
			return fmt.Errorf("%w: ST-7: partition %s no longer exists under that name; it was renamed or dropped since the build listed it",
				ErrInvariantViolation, node.qualified())
		}
		return err
	}
	return proveNodeIn(ctx, tx, node)
}

// proveNameBinding checks that the name the build resolved still names the
// same relation.
func proveNameBinding(ctx context.Context, q querier, want relationFacts) error {
	got, found, err := relationByName(ctx, q, want.schema, want.name)
	if err != nil {
		return err
	}
	if !found || got.oid != want.oid {
		return fmt.Errorf("%w: ST-7: after the attach, the name %s no longer resolves to index %d; a name resolved to another index",
			ErrInvariantViolation, qualifiedName(want.schema, want.name), want.oid)
	}
	return nil
}

// proveIdentity checks that the relation the build resolved under a name
// still has that identity: the same OID under the same name and schema, of
// the same kind, on the same table.
func proveIdentity(ctx context.Context, q querier, want relationFacts, tableOID uint32) error {
	got, found, err := relationByOID(ctx, q, want.oid)
	if err != nil {
		return err
	}
	if !found || got.schema != want.schema || got.name != want.name || got.relkind != want.relkind || got.tableOID != tableOID {
		return fmt.Errorf("%w: ST-7: index %s no longer has the identity the build resolved",
			ErrInvariantViolation, qualifiedName(want.schema, want.name))
	}
	return nil
}
