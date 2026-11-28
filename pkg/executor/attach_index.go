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
// builds, or that is DEFERRABLE. Nothing is built for it. It is also returned
// for a table to attach that is not a plain table, such as a partitioned one.
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
// order. It first pairs the table's indexes with the parent's as the attach
// will (pairAttachIndexes): a parent index the pairing gives a table index
// has it, whether or not that index backs a constraint, and only the rest
// are built, each without adopting an index the pairing gives another. It
// reads and decides every call, so a rerun resumes: an earlier build's
// invalid index under the derived name is removed under proof and built
// again, and a key constraint resumes as the leaf key step does. A name the
// step needs held by something else, and an index the step cannot give the
// table, are refused, typed, before that index is built; earlier indexes
// stay. After the builds it reads both index lists again and refuses, with
// errAttachIndexIncomplete, unless the attach would pair every parent index
// with the table index reported for it. The table is a standalone node, not
// yet a partition, and every read proves it still is. The caller holds the
// parent's advisory table lock (LK-1), and the pool needs the concurrent
// build's two connections.
func ensureAttachIndexes(ctx context.Context, pool *pgxpool.Pool, parentOID uint32, table partitionNode, b ConcurrentBudget) ([]attachIndexResult, error) {
	if !table.standalone {
		return nil, fmt.Errorf("%w: the attach index step takes a standalone table, got %s", ErrInvariantViolation, table.qualified())
	}
	if table.relkind != "r" {
		return nil, fmt.Errorf("%w: %s is of kind %q, and the step gives indexes to a plain table only",
			errAttachIndexUnsupported, table.qualified(), table.relkind)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	tx := briefTx{pool: pool}
	var first attachIndexState
	err := tx.read(ctx, "read the indexes of "+table.qualified()+" and of its partitioned parent", func(ctx context.Context, q querier) error {
		var err error
		first, err = readAttachIndexState(ctx, q, parentOID, table)
		return err
	})
	if err != nil {
		return nil, err
	}
	paired := pairAttachIndexes(first)
	tableIndexName := map[uint32]string{}
	for _, c := range first.table {
		tableIndexName[c.oid] = c.name
	}
	results := make([]attachIndexResult, 0, len(first.parent))
	for i, pi := range first.parent {
		taken := takenByOthers(first.parent, paired, results, i)
		var res attachIndexResult
		switch {
		case pi.contype == "x":
			err = fmt.Errorf("%w: index %s backs an exclusion constraint", errAttachIndexUnsupported, pi.facts.name)
		case pi.contype != "":
			res, err = ensureAttachKeyIndex(ctx, pool, tx, pi, table, taken, b)
		case paired[i] != 0:
			res = attachIndexResult{parentIndex: pi.facts.name, action: attachIndexFound,
				index: tableIndexName[paired[i]], indexOID: paired[i]}
		default:
			res, err = ensureAttachPlainIndex(ctx, pool, tx, pi, table, taken, b)
		}
		if err != nil {
			return results, fmt.Errorf("give %s the index of %s: %w", table.qualified(), pi.facts.name, err)
		}
		results = append(results, res)
	}
	err = tx.read(ctx, "prove the attach to "+table.qualified()+" builds no index", func(ctx context.Context, q querier) error {
		after, err := readAttachIndexState(ctx, q, parentOID, table)
		if err != nil {
			return err
		}
		return attachIndexesComplete(first.parent, after, results)
	})
	if err != nil {
		return results, err
	}
	return results, nil
}

// takenByOthers maps every table index that goes to a parent index other
// than the i-th to that parent index's name: the one the pairing gives every
// other parent index, and the one the step reported for each earlier one.
func takenByOthers(parent []parentIndex, paired []uint32, results []attachIndexResult, i int) map[uint32]string {
	taken := map[uint32]string{}
	for j, oid := range paired {
		if j != i && oid != 0 {
			taken[oid] = parent[j].facts.name
		}
	}
	for _, r := range results {
		taken[r.indexOID] = r.parentIndex
	}
	return taken
}

// ensureAttachPlainIndex builds, concurrently under the derived name, the
// table's index for a parent index that backs no constraint and that the
// pairing gives no table index, unless an earlier run's build of it is
// already there.
func ensureAttachPlainIndex(ctx context.Context, pool *pgxpool.Pool, tx briefTx, pi parentIndex, table partitionNode,
	taken map[uint32]string, b ConcurrentBudget) (attachIndexResult, error) {
	name := derivedPartitionIndexName(pi.facts.name, table.oid)
	res := attachIndexResult{parentIndex: pi.facts.name, action: attachIndexFound}
	found, debris, err := findAttachIndex(ctx, tx, pi, table, name, taken)
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
	// nothing. The build checks only the table's OID on its own session;
	// the read after every build proves the table still standalone.
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

// findAttachIndex looks under the derived name for an earlier run's build of
// the table's index for a plain parent index: the index there must be the
// one the build would create, and an invalid one is debris. It returns the
// zero value when the build has to run, and whether debris must go first.
func findAttachIndex(ctx context.Context, tx briefTx, pi parentIndex, table partitionNode, name string, taken map[uint32]string) (relationFacts, bool, error) {
	var (
		occupant   relationFacts
		occupied   bool
		quarantine []invalidIndex
	)
	err := tx.read(ctx, "inspect the indexes of "+table.qualified(), func(ctx context.Context, q querier) error {
		var err error
		if occupant, occupied, err = relationByName(ctx, q, table.schema, name); err != nil || occupied {
			return err
		}
		quarantine, err = listDroppableQuarantinedIndexes(ctx, q, indexTarget{tableOID: table.oid, schema: table.schema, table: table.name})
		return err
	})
	if err != nil {
		return relationFacts{}, false, err
	}
	if occupied {
		return judgeAttachIndexOccupant(occupant, pi, table, taken)
	}
	return relationFacts{}, len(quarantine) > 0, nil
}

// judgeAttachIndexOccupant decides whether the relation under the derived
// name is the index the build would create on the table: an index on the
// table, attached to nothing, backing no constraint, of the parent index's
// definition, that taken gives to no other parent index. A valid one is
// taken, an invalid one is debris, and anything else holds a name the step
// will not take from its owner.
func judgeAttachIndexOccupant(occ relationFacts, pi parentIndex, table partitionNode, taken map[uint32]string) (relationFacts, bool, error) {
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
	if owner, ok := taken[occ.oid]; ok {
		return relationFacts{}, false, fmt.Errorf("%w: %s is the index the attach takes for the parent's index %s",
			ErrPartitionedIndexNameTaken, name, owner)
	}
	return occ, false, nil
}

// ensureAttachKeyIndex gives the table the key constraint a parent's PRIMARY
// KEY or UNIQUE constraint adopts at the attach, through the partitioned key
// build's leaf step. That step builds a plain unique btree index on the key
// columns and nothing else, so a parent constraint whose index is anything
// more, or a DEFERRABLE one, is refused here before anything is built. The
// step neither adopts nor reports an index that taken gives another parent
// index.
func ensureAttachKeyIndex(ctx context.Context, pool *pgxpool.Pool, tx briefTx, pi parentIndex, table partitionNode,
	taken map[uint32]string, b ConcurrentBudget) (attachIndexResult, error) {
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
	leaf, err := ensureLeafKeyExcept(ctx, pool, spec, table, taken, b)
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
// with the primary key, unique, or exclusion constraint each backs and that
// constraint's columns by name, in key order. A foreign key also names an
// index in conindid, the referenced table's, so a foreign key of the table to
// itself would match its key's index a second time without the type filter.
// Every relation, operator, and function is pg_catalog-qualified (CO-9).
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
		                                       AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog.text[])
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
