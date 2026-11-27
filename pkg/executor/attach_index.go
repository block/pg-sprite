package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/statement"
)

// The index step of an online ATTACH PARTITION. An ATTACH gives the attached
// table an index for every index of the partitioned parent: it attaches a
// matching index the table already has, and builds any it lacks while it
// holds ACCESS EXCLUSIVE on the table and SHARE UPDATE EXCLUSIVE on the
// parent, so the attach lasts as long as the builds. A PRIMARY KEY or UNIQUE
// constraint of the parent needs a matching constraint on the table, not
// just an index. This file gives the table every one of them first, while
// writes go on: a concurrent build under a derived name for a plain index,
// and for a key constraint the partitioned key build's leaf step
// (ensureLeafKey), a concurrent unique build then ADD CONSTRAINT ... USING
// INDEX. The attach that follows then attaches them and builds nothing.

// errAttachIndexUnsupported is returned for an index of the parent the step
// cannot give the table: an EXCLUDE constraint's, or a key constraint whose
// index is not the plain unique btree index on its columns the leaf key step
// builds, or that is DEFERRABLE. Nothing is built for it.
var errAttachIndexUnsupported = errors.New("the step cannot give the table this index of the partitioned parent")

// attachIndexAction says what the step did for one index of the parent.
type attachIndexAction string

const (
	// attachIndexFound: the table already had a matching index, or key
	// constraint, that the attach will take.
	attachIndexFound attachIndexAction = "found"
	// attachIndexBuilt: the step built the index concurrently.
	attachIndexBuilt attachIndexAction = "built"
	// attachIndexKeyed: the leaf key step gave the table the key
	// constraint, building its index when the table had none.
	attachIndexKeyed attachIndexAction = "keyed"
)

// attachIndexResult is the table's index for one index of the parent.
type attachIndexResult struct {
	parentIndex string
	action      attachIndexAction
	index       string
	indexOID    uint32
}

// parentIndex is one index of the partitioned parent, with the constraint it
// backs, if any.
type parentIndex struct {
	facts relationFacts
	key   string
	// contype is the backed constraint's pg_constraint.contype, or empty.
	contype    string
	constraint string
	deferrable bool
	// columns are the key constraint's columns by name, in key order.
	columns []string
}

// ensureAttachIndexes gives the table every index of the parent the attach
// will need, and returns what it did for each, in the parent indexes' OID
// order. It reads and decides every call, so a rerun resumes: an index the
// table has is found, an earlier build's invalid index under the derived
// name is removed under proof and built again, and a key constraint resumes
// as the leaf key step does. A table index counts for one parent index only,
// as the attach takes it. A name the step needs held by something else, and
// an index the step cannot give the table, are refused, typed, before that
// index is built; earlier indexes stay. The table is a standalone node, not
// yet a partition, and every build proves it still is. The caller holds the
// parent's advisory table lock (LK-1), and the pool needs the concurrent
// build's two connections.
func ensureAttachIndexes(ctx context.Context, pool *pgxpool.Pool, parentOID uint32, table partitionNode, b ConcurrentBudget) ([]attachIndexResult, error) {
	if !table.standalone || table.relkind != "r" {
		return nil, fmt.Errorf("%w: the attach index step takes a standalone plain table, got %s of kind %q",
			ErrInvariantViolation, table.qualified(), table.relkind)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	tx := briefTx{pool: pool}
	var indexes []parentIndex
	err := tx.read(ctx, "list the indexes of the partitioned parent of "+table.qualified(), func(ctx context.Context, q querier) error {
		var err error
		indexes, err = readParentIndexes(ctx, q, parentOID)
		return err
	})
	if err != nil {
		return nil, err
	}
	claimed := map[uint32]bool{}
	results := make([]attachIndexResult, 0, len(indexes))
	for _, pi := range indexes {
		var res attachIndexResult
		switch pi.contype {
		case "":
			res, err = ensureAttachPlainIndex(ctx, pool, tx, pi, table, claimed, b)
		case "p", "u":
			res, err = ensureAttachKeyIndex(ctx, pool, tx, pi, table, b)
		default:
			err = fmt.Errorf("%w: index %s backs a constraint of type %q", errAttachIndexUnsupported, pi.facts.name, pi.contype)
		}
		if err != nil {
			return results, fmt.Errorf("give %s the index of %s: %w", table.qualified(), pi.facts.name, err)
		}
		claimed[res.indexOID] = true
		results = append(results, res)
	}
	return results, nil
}

// ensureAttachPlainIndex gives the table an index the attach takes for a
// parent index that backs no constraint: one it already has, of the parent
// index's definition and claimed by no earlier parent index, or one built
// concurrently under the derived name.
func ensureAttachPlainIndex(ctx context.Context, pool *pgxpool.Pool, tx briefTx, pi parentIndex, table partitionNode,
	claimed map[uint32]bool, b ConcurrentBudget) (attachIndexResult, error) {
	name := derivedPartitionIndexName(pi.facts.name, table.oid)
	res := attachIndexResult{parentIndex: pi.facts.name, action: attachIndexFound}
	found, debris, err := findAttachIndex(ctx, tx, pi, table, name, claimed)
	if err != nil {
		return attachIndexResult{}, err
	}
	if found.oid != 0 {
		res.index, res.indexOID = found.name, found.oid
		return res, nil
	}
	buildSQL, err := statement.PartitionIndex(pi.facts.def, table.schema, table.name, name, statement.PartitionIndexConcurrently)
	if err != nil {
		return attachIndexResult{}, fmt.Errorf("make the index statement for %s: %w", table.qualified(), err)
	}
	// INV: ST-7 — the table is still the one resolved, a partition of
	// nothing; the build below asserts the same table on its own session.
	if err := tx.read(ctx, "prove "+table.qualified(), func(ctx context.Context, q querier) error {
		return proveNodeIn(ctx, q, table)
	}); err != nil {
		return attachIndexResult{}, err
	}
	if debris {
		// An invalid index under the derived name, or one an earlier
		// recovery quarantined, is an earlier run's failed build. The
		// recovery proves it abandoned, quarantines it by OID, and drops
		// it; a backend still building it refuses the run.
		if _, err := recoverAbandonedIndex(ctx, pool, buildSQL, b, recoveryPool{
			minConns: dropRecoveryMinConns,
			sessions: "recovery session and drop session",
		}, nil); err != nil {
			return attachIndexResult{}, fmt.Errorf("recover the invalid index %s on %s: %w",
				qualifiedName(table.schema, name), table.qualified(), err)
		}
	}
	built, err := buildIndexConcurrentlyOn(ctx, pool, buildSQL, b, nil, table.oid)
	if err != nil {
		return attachIndexResult{}, fmt.Errorf("build the index of %s: %w", table.qualified(), err)
	}
	res.action, res.index, res.indexOID = attachIndexBuilt, name, built.IndexOID
	return res, nil
}

// findAttachIndex looks for the table's index for a plain parent index. The
// derived name decides when it is held: the index there must be the one the
// build would create, and an invalid one is debris. Otherwise any valid
// index of the definition, backing no constraint, attached to nothing, and
// claimed by no earlier parent index, is taken. It returns the zero value
// when the build has to run, and whether debris must go first.
func findAttachIndex(ctx context.Context, tx briefTx, pi parentIndex, table partitionNode, name string, claimed map[uint32]bool) (relationFacts, bool, error) {
	var (
		occupant   relationFacts
		occupied   bool
		reusable   []relationFacts
		quarantine []invalidIndex
	)
	err := tx.read(ctx, "inspect the indexes of "+table.qualified(), func(ctx context.Context, q querier) error {
		var err error
		if occupant, occupied, err = relationByName(ctx, q, table.schema, name); err != nil || occupied {
			return err
		}
		if reusable, err = reusableIndexes(ctx, q, table.oid); err != nil {
			return err
		}
		quarantine, err = listDroppableQuarantinedIndexes(ctx, q, indexTarget{tableOID: table.oid, schema: table.schema, table: table.name})
		return err
	})
	if err != nil {
		return relationFacts{}, false, err
	}
	if occupied {
		return judgeAttachIndexOccupant(occupant, pi, table)
	}
	for _, candidate := range reusable {
		if claimed[candidate.oid] {
			continue
		}
		key, err := candidate.definitionKey()
		if err != nil {
			return relationFacts{}, false, err
		}
		if key == pi.key {
			return candidate, false, nil
		}
	}
	return relationFacts{}, len(quarantine) > 0, nil
}

// judgeAttachIndexOccupant decides whether the relation under the derived
// name is the index the build would create on the table: an index on the
// table, attached to nothing, backing no constraint, of the parent index's
// definition. A valid one is taken, an invalid one is debris, and anything
// else holds a name the step will not take from its owner.
func judgeAttachIndexOccupant(occ relationFacts, pi parentIndex, table partitionNode) (relationFacts, bool, error) {
	name := qualifiedName(occ.schema, occ.name)
	if occ.relkind != "i" || occ.tableOID != table.oid {
		return relationFacts{}, false, fmt.Errorf("%w: %s is %s, not the index %s needs",
			ErrPartitionedIndexNameTaken, name, describeRelation(occ), table.qualified())
	}
	if occ.isPartition || occ.backsConstraint {
		return relationFacts{}, false, fmt.Errorf("%w: %s is attached to a partitioned index or backs a constraint",
			ErrPartitionedIndexNameTaken, name)
	}
	key, err := occ.definitionKey()
	if err != nil {
		return relationFacts{}, false, err
	}
	if key != pi.key {
		return relationFacts{}, false, fmt.Errorf("%w: %s does not have the definition of the parent's index %s",
			ErrPartitionedIndexNameTaken, name, pi.facts.name)
	}
	if !occ.valid {
		return relationFacts{}, true, nil
	}
	return occ, false, nil
}

// ensureAttachKeyIndex gives the table the key constraint a parent's PRIMARY
// KEY or UNIQUE constraint adopts at the attach, through the partitioned key
// build's leaf step. That step builds a plain unique btree index on the key
// columns and nothing else, so a parent constraint whose index is anything
// more, or a DEFERRABLE one, is refused here before anything is built.
func ensureAttachKeyIndex(ctx context.Context, pool *pgxpool.Pool, tx briefTx, pi parentIndex, table partitionNode, b ConcurrentBudget) (attachIndexResult, error) {
	if pi.deferrable {
		return attachIndexResult{}, fmt.Errorf("%w: constraint %s is DEFERRABLE", errAttachIndexUnsupported, pi.constraint)
	}
	spec := partitionedKeySpec{kind: partitionedUniqueKey, name: pi.constraint, columns: pi.columns}
	if pi.contype == "p" {
		spec.kind = partitionedPrimaryKey
	}
	var want string
	err := tx.read(ctx, "read the key columns of "+table.qualified(), func(ctx context.Context, q querier) error {
		columns, err := readLeafKeyColumns(ctx, q, table, spec.columns)
		if err != nil {
			return err
		}
		want = leafKeyDefinitionKey(columns)
		return nil
	})
	if err != nil {
		return attachIndexResult{}, err
	}
	if pi.key != want {
		return attachIndexResult{}, fmt.Errorf("%w: the index of constraint %s is not a plain unique btree index on its columns",
			errAttachIndexUnsupported, pi.constraint)
	}
	leaf, err := ensureLeafKey(ctx, pool, spec, table, b)
	if err != nil {
		return attachIndexResult{}, err
	}
	res := attachIndexResult{parentIndex: pi.facts.name, action: attachIndexKeyed, index: leaf.index, indexOID: leaf.indexOID}
	if leaf.action == leafKeyFound {
		res.action = attachIndexFound
	}
	return res, nil
}

// parentIndexesSQL lists the partitioned indexes of the table, in OID order,
// with the constraint each backs and that constraint's columns by name, in
// key order. Every relation, operator, and function is pg_catalog-qualified
// (CO-9).
const parentIndexesSQL = `SELECT c.oid,
		        COALESCE(k.contype::pg_catalog.text, ''),
		        COALESCE(k.conname::pg_catalog.text, ''),
		        COALESCE(k.condeferrable, false),
		        COALESCE(ARRAY(
		          SELECT a.attname::pg_catalog.text
		            FROM pg_catalog.unnest(k.conkey) WITH ORDINALITY AS u(attnum, ord)
		            JOIN pg_catalog.pg_attribute a
		              ON a.attrelid OPERATOR(pg_catalog.=) k.conrelid
		             AND a.attnum OPERATOR(pg_catalog.=) u.attnum
		           ORDER BY u.ord), '{}')
		   FROM pg_catalog.pg_index i
		   JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.indexrelid
		   LEFT JOIN pg_catalog.pg_constraint k ON k.conindid OPERATOR(pg_catalog.=) c.oid
		                                       AND k.conrelid OPERATOR(pg_catalog.=) i.indrelid
		  WHERE i.indrelid OPERATOR(pg_catalog.=) $1
		    AND c.relkind OPERATOR(pg_catalog.=) 'I'
		  ORDER BY c.oid`

// readParentIndexes reads every partitioned index of the parent with its
// definition key and the constraint it backs.
func readParentIndexes(ctx context.Context, q querier, parentOID uint32) ([]parentIndex, error) {
	rows, err := q.Query(ctx, parentIndexesSQL, parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the indexes of table %d: %w", parentOID, err)
	}
	var (
		found []parentIndex
		oids  []uint32
	)
	for rows.Next() {
		var (
			pi  parentIndex
			oid uint32
		)
		if err := rows.Scan(&oid, &pi.contype, &pi.constraint, &pi.deferrable, &pi.columns); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan the indexes of table %d: %w", parentOID, err)
		}
		found = append(found, pi)
		oids = append(oids, oid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the indexes of table %d: %w", parentOID, err)
	}
	for i := range found {
		facts, ok, err := relationByOID(ctx, q, oids[i])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: index %d of table %d left the catalog during the read", ErrInvariantViolation, oids[i], parentOID)
		}
		if found[i].key, err = facts.definitionKey(); err != nil {
			return nil, err
		}
		found[i].facts = facts
	}
	return found, nil
}
