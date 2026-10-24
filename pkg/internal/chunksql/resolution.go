package chunksql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// querier runs one query in the transaction whose catalog view the
// fingerprint should read.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ResolutionFingerprint pins what the conversion expressions resolve to.
// Insert carries the expressions as the user wrote them, and every chunk
// resolves their unqualified names afresh on the path SearchPath sets —
// pg_catalog, then schema, then public — so a function, operator, type, or
// collation created, replaced, or dropped under one of those names after
// the shadow build can change what a later chunk runs without any chunk
// failing. The fingerprint is a digest of every catalog entry under a name
// an expression spells, in every schema the path would look in: the
// signature the server resolves a call by and the definition it then runs,
// which for a function is its body in either form (prosrc or a SQL-standard
// prosqlbody), its strictness, its own SET settings, and its argument
// defaults, since each of them changes what a call returns.
// The shadow build records it in the proof, and each guarded transaction
// recomputes it and refuses to write when it differs (CO-9).
//
// The digest is over renderings that depend on nothing but the catalog:
// names are the spelled text, every reference to another object is its
// OID, and nothing goes through a deparser that would render differently
// under another search_path. It is deliberately wider than resolution: an
// overload added under a spelled name that the server would never pick
// changes it too, and a changed function body under a name an expression
// calls changes it although resolution did not, since either is a catalog
// the build did not see. It is also narrower than everything a resolution
// touches: an implicit cast the server picks from pg_cast, the function
// behind a cast the expression performs, the functions a called function
// calls in turn, the labels of an enum type, and the constraints of a
// domain are not under a name the expression spells and are not digested.
//
// The result is empty when conversions is empty, so a proof for a copy
// that evaluates no expression carries no fingerprint; a copy that does
// always carries one, even when its expressions spell no name at all.
func ResolutionFingerprint(ctx context.Context, q querier, schema string, conversions map[string]string) (string, error) {
	if len(conversions) == 0 {
		return "", nil
	}
	names, err := spelledNames(conversions)
	if err != nil {
		return "", err
	}
	path := []string{"pg_catalog", schema, "public"}
	functions := candidates(names.Functions, path)
	operators := candidates(names.Operators, path)
	types := candidates(names.Types, path)
	collations := candidates(names.Collations, path)
	rows, err := q.Query(ctx, resolutionSQL,
		functions.schemas, functions.names,
		operators.schemas, operators.names,
		types.schemas, types.names,
		collations.schemas, collations.names)
	if err != nil {
		return "", fmt.Errorf("read the catalog entries the conversion expressions resolve among: %w", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("read the catalog entries the conversion expressions resolve among: %w", err)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// spelledNames is the union of the names every conversion expression
// spells, in the order of the columns so two proofs of the same
// conversions read the same names.
func spelledNames(conversions map[string]string) (statement.ConversionNames, error) {
	var all statement.ConversionNames
	for _, column := range slices.Sorted(maps.Keys(conversions)) {
		names, err := statement.ConversionNamesOf(conversions[column])
		if err != nil {
			return statement.ConversionNames{}, fmt.Errorf("conversion of column %s: %w", column, err)
		}
		all.Functions = append(all.Functions, names.Functions...)
		all.Operators = append(all.Operators, names.Operators...)
		all.Types = append(all.Types, names.Types...)
		all.Collations = append(all.Collations, names.Collations...)
	}
	return all, nil
}

// candidateSet is the (schema, name) pairs one arm of resolutionSQL looks
// up, as two parallel arrays for unnest.
type candidateSet struct {
	schemas []string
	names   []string
}

// candidates expands names into the schemas the server would look them up
// in: a qualified name in the schema it names, an unqualified one in every
// schema on path. Pairs are unique, so a schema the path lists twice —
// public as the source schema — contributes one lookup.
func candidates(names []statement.QualifiedName, path []string) candidateSet {
	seen := map[statement.QualifiedName]bool{}
	set := candidateSet{schemas: []string{}, names: []string{}}
	add := func(n statement.QualifiedName) {
		if seen[n] {
			return
		}
		seen[n] = true
		set.schemas = append(set.schemas, n.Schema)
		set.names = append(set.names, n.Name)
	}
	for _, n := range names {
		if n.Schema != "" {
			add(n)
			continue
		}
		for _, schema := range path {
			add(statement.QualifiedName{Schema: schema, Name: n.Name})
		}
	}
	return set
}

// resolutionSQL renders one text line per catalog entry under a candidate
// name: functions from ($1, $2), operators from ($3, $4), types from ($5,
// $6), and collations from ($7, $8), each pair of arrays the schemas and
// names of one kind, zipped by ROWS FROM, which is how the grammar spells
// a multi-array unnest. Every catalog name is qualified and every comparison
// spells its operator, so the query reads the same under any search_path
// (CO-9); every column is one whose text rendering the path cannot change,
// which is why references to other objects are rendered as OIDs rather
// than through regproc, regtype, or a deparsing function. Nullable columns
// are coalesced so a NULL in one and a value in another never collapse to
// the same line.
const resolutionSQL = `
	SELECT pg_catalog.concat_ws(E'\t', 'function', c.nspname, c.name,
	       p.proargtypes::pg_catalog.text,
	       COALESCE(p.proallargtypes::pg_catalog.text, ''),
	       COALESCE(p.proargmodes::pg_catalog.text, ''),
	       p.pronargdefaults, p.provariadic, p.prokind, p.proretset, p.prorettype,
	       p.provolatile, p.prolang, p.proisstrict,
	       COALESCE(p.proconfig::pg_catalog.text, ''),
	       COALESCE(p.proargdefaults::pg_catalog.text, ''),
	       COALESCE(p.prosqlbody::pg_catalog.text, ''),
	       COALESCE(p.prosrc, ''), COALESCE(p.probin, ''))
	  FROM ROWS FROM (pg_catalog.unnest($1::pg_catalog.text[]), pg_catalog.unnest($2::pg_catalog.text[])) AS c(nspname, name)
	  JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) c.nspname
	  JOIN pg_catalog.pg_proc p ON p.pronamespace OPERATOR(pg_catalog.=) n.oid
	                           AND p.proname OPERATOR(pg_catalog.=) c.name
	UNION ALL
	SELECT pg_catalog.concat_ws(E'\t', 'operator', c.nspname, c.name,
	       o.oprkind, o.oprleft, o.oprright, o.oprresult, o.oprcode::pg_catalog.oid,
	       COALESCE(f.proargtypes::pg_catalog.text, ''),
	       COALESCE(f.prorettype::pg_catalog.text, ''), COALESCE(f.provolatile::pg_catalog.text, ''),
	       COALESCE(f.prolang::pg_catalog.text, ''), COALESCE(f.proisstrict::pg_catalog.text, ''),
	       COALESCE(f.proconfig::pg_catalog.text, ''), COALESCE(f.prosqlbody::pg_catalog.text, ''),
	       COALESCE(f.prosrc, ''), COALESCE(f.probin, ''))
	  FROM ROWS FROM (pg_catalog.unnest($3::pg_catalog.text[]), pg_catalog.unnest($4::pg_catalog.text[])) AS c(nspname, name)
	  JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) c.nspname
	  JOIN pg_catalog.pg_operator o ON o.oprnamespace OPERATOR(pg_catalog.=) n.oid
	                               AND o.oprname OPERATOR(pg_catalog.=) c.name
	  LEFT JOIN pg_catalog.pg_proc f ON f.oid OPERATOR(pg_catalog.=) o.oprcode::pg_catalog.oid
	UNION ALL
	SELECT pg_catalog.concat_ws(E'\t', 'type', c.nspname, c.name,
	       t.typtype, t.typcategory, t.typlen, t.typbyval, t.typisdefined,
	       t.typinput::pg_catalog.oid, t.typoutput::pg_catalog.oid,
	       t.typbasetype, t.typtypmod, t.typelem, t.typarray, t.typcollation, t.typnotnull)
	  FROM ROWS FROM (pg_catalog.unnest($5::pg_catalog.text[]), pg_catalog.unnest($6::pg_catalog.text[])) AS c(nspname, name)
	  JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) c.nspname
	  JOIN pg_catalog.pg_type t ON t.typnamespace OPERATOR(pg_catalog.=) n.oid
	                           AND t.typname OPERATOR(pg_catalog.=) c.name
	UNION ALL
	SELECT pg_catalog.concat_ws(E'\t', 'collation', c.nspname, c.name,
	       l.collprovider, l.collisdeterministic, l.collencoding,
	       COALESCE(l.collcollate, ''), COALESCE(l.collctype, ''),
	       COALESCE(l.collversion, ''))
	  FROM ROWS FROM (pg_catalog.unnest($7::pg_catalog.text[]), pg_catalog.unnest($8::pg_catalog.text[])) AS c(nspname, name)
	  JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) c.nspname
	  JOIN pg_catalog.pg_collation l ON l.collnamespace OPERATOR(pg_catalog.=) n.oid
	                                AND l.collname OPERATOR(pg_catalog.=) c.name`
