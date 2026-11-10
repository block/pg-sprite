package checksum

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
	"github.com/block/pg-sprite/pkg/preflight"
)

// CompareCarried reads every key a live pass left for the swap
// (VerifiedShadow.Carried) on the source and on the shadow, inside the
// swap transaction the caller owns, and returns the ones whose rows
// differ, in ascending order. The pass could conclude nothing about these
// keys because they changed on every reread; the caller holds ACCESS
// EXCLUSIVE on both relations and has applied the whole stream to the
// shadow in tx, so here nothing can change them and the two reads compare
// the rows the rename would publish (CO-1). An empty result with a nil
// error is every carried key equal: present on both sides with the same
// row, or absent from both.
//
// Each side is read with the row hash the pass's digests use: the copy
// columns cast to the shadow's types and rendered as a record, the source
// side through the schema change's USING expressions. The rows are read
// by key, so the swap's lock covers a read of at most MaxCarryKeys rows
// on each side, each found through the primary key.
//
// The compare trusts nothing it was not handed proof of. It refuses, as an
// ErrInvariantViolation, a shadow proof that does not describe target's
// shadow or that verified was not minted over, a transaction that does not
// hold ACCESS EXCLUSIVE on both relations — without it the two reads are
// two snapshots a writer could commit between — relations that are no
// longer the ones the proofs name (ST-6), and, for a copy that evaluates
// conversion expressions, a catalog they no longer resolve in as the
// build saw, checked before the reads and again after them (CO-9). The
// second check sees a change committed after the first only when tx is
// READ COMMITTED, as the swap's is.
//
// The caller's transaction already carries its budgets and role, so none
// are set here. The compare pins the output settings the digests pin,
// chunksql.Rendering, so a conversion renders a value as the copy did,
// and sets the search_path the pass's reads run under. It restores the
// caller's extra_float_digits and bytea_output before it returns and
// leaves the search_path for the caller to set back as its own statements
// need. A proof that carries no key touches nothing, not even the session
// settings.
//
// Any error leaves the caller to roll tx back: the keys were not compared,
// and the proof says nothing about them.
func CompareCarried(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow copier.Shadow, verified VerifiedShadow) ([]int64, error) {
	// INV: ST-6
	if target.Table() == "" {
		return nil, fmt.Errorf("%w (ST-6): copy-and-swap target proof is empty", ErrInvariantViolation)
	}
	if err := checkShadow(target, shadow); err != nil {
		return nil, err
	}
	if err := checkVerifiedOver(shadow, verified); err != nil {
		return nil, err
	}
	keys := verified.carried
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > MaxCarryKeys {
		// INV: CO-1
		return nil, fmt.Errorf("%w (CO-1): verified shadow proof for %s.%s leaves %d keys for the swap, more than the %d a pass may leave", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), len(keys), MaxCarryKeys)
	}
	digits, byteaOutput, err := pinCompareSettings(ctx, tx, chunksql.SearchPath(shadow.Schema(), len(shadow.Conversions()) > 0))
	if err != nil {
		return nil, err
	}
	if err := confirmStilled(ctx, tx, shadow); err != nil {
		return nil, err
	}
	if err := confirmProvenRelations(ctx, tx, shadow); err != nil {
		return nil, err
	}
	if err := confirmProvenResolution(ctx, tx, shadow); err != nil {
		return nil, err
	}
	differing, err := compareKeys(ctx, tx, target, shadow, keys)
	if err != nil {
		return nil, err
	}
	// A catalog change committed between the first check and the reads is
	// seen by this one, and the caller rolls the swap back (CO-9).
	if err := confirmProvenResolution(ctx, tx, shadow); err != nil {
		return nil, err
	}
	// One statement puts back both of the caller's output settings, since it
	// runs under the swap's lock; set_config takes the values as bind
	// parameters, which SET LOCAL cannot, and sets them transaction-local
	// as SET LOCAL would.
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('extra_float_digits', $1, true), pg_catalog.set_config('bytea_output', $2, true)`, digits, byteaOutput); err != nil {
		return nil, fmt.Errorf("restore extra_float_digits and bytea_output after comparing carried keys: %w", err)
	}
	return differing, nil
}

// checkVerifiedOver refuses a verified-shadow proof minted over relations
// other than the ones shadow describes: its carried keys are keys of that
// other pair, and comparing them here would vouch for rows no pass named.
func checkVerifiedOver(shadow copier.Shadow, verified VerifiedShadow) error {
	// INV: ST-6
	if verified.Table() == "" {
		return fmt.Errorf("%w (ST-6): verified shadow proof is empty", ErrInvariantViolation)
	}
	if verified.Schema() != shadow.Schema() || verified.Table() != shadow.SourceTable() || verified.Shadow() != shadow.ShadowTable() {
		return fmt.Errorf("%w (ST-6): verified shadow proof is for %s.%s into %s, shadow proof is for %s.%s into %s", ErrInvariantViolation,
			verified.Schema(), verified.Table(), verified.Shadow(), shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable())
	}
	if verified.SourceOID() != shadow.SourceOID() || verified.ShadowOID() != shadow.ShadowOID() {
		return fmt.Errorf("%w (ST-6): verified shadow proof compared relations %d and %d, shadow proof is for relations %d and %d", ErrInvariantViolation,
			verified.SourceOID(), verified.ShadowOID(), shadow.SourceOID(), shadow.ShadowOID())
	}
	return nil
}

// pinCompareSettings sets the session settings the compare's reads depend
// on inside the caller's transaction — the output settings of
// chunksql.Rendering and searchPath, the path the pass's reads run under —
// and returns the extra_float_digits and bytea_output it found, so the
// caller's own values can be put back once the keys are read.
func pinCompareSettings(ctx context.Context, tx pgx.Tx, searchPath string) (string, string, error) {
	var digits, byteaOutput string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('extra_float_digits'), pg_catalog.current_setting('bytea_output')`).Scan(&digits, &byteaOutput); err != nil {
		return "", "", fmt.Errorf("read extra_float_digits and bytea_output before comparing carried keys: %w", err)
	}
	// INV: CO-9
	if _, err := tx.Exec(ctx, chunksql.Rendering+"; "+searchPath); err != nil {
		return "", "", fmt.Errorf("set settings for comparing carried keys: %w", err)
	}
	return digits, byteaOutput, nil
}

// stilledSQL counts the relations, of the source ($1) and the shadow ($2),
// this backend holds ACCESS EXCLUSIVE on in its own database. Every
// catalog name and operator is qualified, so a planted overload in a
// searched schema cannot answer this read (CO-9).
const stilledSQL = `
	SELECT pg_catalog.count(DISTINCT l.relation)
	FROM pg_catalog.pg_locks l
	WHERE l.locktype OPERATOR(pg_catalog.=) 'relation'
	  AND l.granted
	  AND l.database OPERATOR(pg_catalog.=) (
	      SELECT d.oid
	      FROM pg_catalog.pg_database d
	      WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database())
	  AND (l.relation OPERATOR(pg_catalog.=) $1::pg_catalog.oid
	       OR l.relation OPERATOR(pg_catalog.=) $2::pg_catalog.oid)
	  AND l.mode OPERATOR(pg_catalog.=) 'AccessExclusiveLock'
	  AND l.pid OPERATOR(pg_catalog.=) pg_catalog.pg_backend_pid()`

// confirmStilled refuses to compare unless the transaction itself holds
// ACCESS EXCLUSIVE on the source and on the shadow. The compare reads each
// side in a statement of its own, so only a lock that excludes every other
// writer and reader makes the two reads one moment; a difference read
// without it could be a write that landed between them.
func confirmStilled(ctx context.Context, tx pgx.Tx, shadow copier.Shadow) error {
	var held int
	if err := tx.QueryRow(ctx, stilledSQL, shadow.SourceOID(), shadow.ShadowOID()).Scan(&held); err != nil {
		return fmt.Errorf("read the locks held on %s.%s and its shadow %s: %w", shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), err)
	}
	if held != 2 {
		// INV: CO-1
		return fmt.Errorf("%w (CO-1): carried keys of %s.%s are compared only under ACCESS EXCLUSIVE on it and on its shadow %s; the transaction holds it on %d of the two", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), held)
	}
	return nil
}

// confirmProvenRelations refuses to compare when the source or the shadow
// is no longer the relation its proof was minted for, as the pass's guard
// does: the reads name the relations, and a table dropped and recreated
// under the same name holds no row the pass judged.
func confirmProvenRelations(ctx context.Context, tx pgx.Tx, shadow copier.Shadow) error {
	var sourceOID, shadowOID *uint32
	err := tx.QueryRow(ctx, relationOIDsSQL, shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable()).Scan(&sourceOID, &shadowOID)
	if err != nil {
		return fmt.Errorf("resolve %s.%s and its shadow %s: %w", shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), err)
	}
	// INV: ST-6
	if err := confirmRelation("source", shadow.Schema(), shadow.SourceTable(), shadow.SourceOID(), sourceOID); err != nil {
		return err
	}
	return confirmRelation("shadow", shadow.Schema(), shadow.ShadowTable(), shadow.ShadowOID(), shadowOID)
}

// confirmProvenResolution refuses to compare when the objects the
// conversion expressions resolve among are no longer the ones the shadow
// build digested: the source side evaluates the expressions afresh, so the
// compare would hold the shadow against a rendering the gated statement
// never produced. A copy without conversions has nothing to confirm.
func confirmProvenResolution(ctx context.Context, tx pgx.Tx, shadow copier.Shadow) error {
	err := chunksql.ConfirmResolution(ctx, tx, shadow.Schema(), shadow.SourceTable(), shadow.ShadowTable(), shadow.Conversions(), shadow.ConversionFingerprint())
	// INV: CO-9
	if errors.Is(err, ErrConversionDrift) {
		return fmt.Errorf("%w (CO-9): %w", ErrInvariantViolation, err)
	}
	return err
}

// compareKeys reads keys on both sides and returns the ones whose rows
// differ. keys is ascending, as the proof holds it, so the result is too.
func compareKeys(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow copier.Shadow, keys []int64) ([]int64, error) {
	types, err := shadowColumnTypes(ctx, tx, shadow)
	if err != nil {
		return nil, err
	}
	schema := shadow.Schema()
	source, err := readRows(ctx, tx, keyedRowsSQL(target, schema, shadow.SourceTable(), types, shadow.Conversions()), keys)
	if err != nil {
		return nil, fmt.Errorf("read %d carried keys of %s.%s: %w", len(keys), schema, shadow.SourceTable(), err)
	}
	// The shadow holds the converted values, so its side carries no
	// conversion, as the pass's digest of it carries none.
	held, err := readRows(ctx, tx, keyedRowsSQL(target, schema, shadow.ShadowTable(), types, nil), keys)
	if err != nil {
		return nil, fmt.Errorf("read %d carried keys of shadow %s.%s: %w", len(keys), schema, shadow.ShadowTable(), err)
	}
	return differingKeys(keys, source, held), nil
}

// differingKeys returns the keys whose row is not the same on both sides,
// in the order keys lists them. A key neither side holds is the same on
// both: the source deleted the row and the stream carried the delete.
func differingKeys(keys []int64, source, shadow rowSet) []int64 {
	var differing []int64
	for _, key := range keys {
		// INV: CO-1
		if source.at(key) != shadow.at(key) {
			differing = append(differing, key)
		}
	}
	return differing
}

// keyedRowsSQL is the rows statement over a list of keys instead of a
// range: the same per-row SHA-256, for the rows whose key is in $1, in key
// order. The keys are declared bigint whatever the key's integer type, as
// the range's bounds are, so the primary-key index serves each lookup, and
// the comparison names the catalog's equality so no schema on the
// search_path can answer it (CO-9).
func keyedRowsSQL(target preflight.CopySwapTarget, schema, table string, types []columnType, conversions map[string]string) string {
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	return "SELECT " + key + ", " + rowHashSQL(types, conversions) +
		" FROM " + pgx.Identifier{schema, table}.Sanitize() +
		" WHERE " + key + " OPERATOR(pg_catalog.=) ANY($1::bigint[])" +
		" ORDER BY " + key
}
