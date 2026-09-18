package schemachange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

var (
	// ErrShadowExists reports that the deterministic shadow name is already
	// taken in the source schema: an earlier run's leftover, which a later
	// resume path inspects rather than this builder overwriting it.
	ErrShadowExists = errors.New("shadow table already exists")
	// ErrInvariantViolation reports a forged or empty proof, a statement
	// that does not target the proven table, or a re-verification the
	// builder performed on its own work that failed. The builder never
	// executes anything after raising it.
	ErrInvariantViolation = errors.New("invariant violation")
)

// Options bounds the shadow build transaction. Zero values take the dbconn
// session defaults, so a caller that passes Options{} still gets bounded
// waits (LK-2).
type Options struct {
	// LockTimeout bounds every lock wait inside the build transaction.
	LockTimeout time.Duration
	// StatementTimeout bounds every statement inside the build transaction.
	StatementTimeout time.Duration
}

func (o Options) lockTimeout() time.Duration {
	if o.LockTimeout == 0 {
		return dbconn.DefaultLockTimeout
	}
	return o.LockTimeout
}

func (o Options) statementTimeout() time.Duration {
	if o.StatementTimeout == 0 {
		return dbconn.DefaultStatementTimeout
	}
	return o.StatementTimeout
}

// BuiltShadow is the proof that the shadow table exists in the source schema
// with the gated schema change applied, introspected, and owner-correct. Its
// constructor is private: the copier and cutover accept only a value this
// builder returned.
type BuiltShadow struct {
	schema, source, shadow string
	sourceFingerprint      string
	targetFingerprint      string
	identities             []IdentityColumn
	fidelity               FidelitySnapshot
	copyColumns            []string
}

// Schema is the schema holding both the source and the shadow.
func (b BuiltShadow) Schema() string { return b.schema }

// SourceTable is the source table name.
func (b BuiltShadow) SourceTable() string { return b.source }

// ShadowTable is the shadow table name.
func (b BuiltShadow) ShadowTable() string { return b.shadow }

// SourceFingerprint is the digest of the source's introspected model at
// build time; a resume compares it to refuse a source that changed shape.
func (b BuiltShadow) SourceFingerprint() string { return b.sourceFingerprint }

// TargetFingerprint is the digest of the shadow's introspected model after
// the schema change; the checkpoint carries it so a resume can prove the
// shadow it finds is the one this build produced.
func (b BuiltShadow) TargetFingerprint() string { return b.targetFingerprint }

// IdentityColumns are the source identity columns cutover hands over (D5).
func (b BuiltShadow) IdentityColumns() []IdentityColumn {
	return append([]IdentityColumn(nil), b.identities...)
}

// Fidelity is the metadata snapshot replicated onto the shadow; the ST-5
// gate re-reads both tables and compares against it before the swap.
func (b BuiltShadow) Fidelity() FidelitySnapshot { return b.fidelity }

// CopyColumns are the columns the copier moves: every non-generated source
// column that still exists on the shadow after the schema change. Generated
// columns are excluded because the server computes them on insert; a
// column the change dropped is excluded because the shadow has nowhere to
// put it.
func (b BuiltShadow) CopyColumns() []string { return append([]string(nil), b.copyColumns...) }

// BuildShadow creates the copy-and-swap shadow for the proven target and
// applies the gated ALTER TABLE to it, all in one bounded transaction under
// SET ROLE owner: CREATE TABLE … LIKE INCLUDING ALL EXCLUDING IDENTITY, the
// identity-sequence defaults (D5), the metadata LIKE does not carry (D2), and
// finally the statement retargeted onto the shadow — after re-proving that
// the retargeted form differs from the gated one only in its target relation
// and that the target is the shadow (ST-7). Both models are introspected
// inside the same transaction, so a failure at any step leaves no shadow
// behind. An existing relation under the shadow's name is ErrShadowExists.
func BuildShadow(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, st statement.Statement, opts Options) (BuiltShadow, error) {
	if err := checkProof(target); err != nil {
		return BuiltShadow{}, err
	}
	shadow := ShadowName(target.Schema(), target.Table())
	if err := CheckIdentifierLengths(shadow, OldName(target.Schema(), target.Table())); err != nil {
		return BuiltShadow{}, err
	}
	retargeted, err := retargetOntoShadow(st, target, shadow)
	if err != nil {
		return BuiltShadow{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("begin shadow build: %w", err)
	}
	defer func() {
		// Redundant safety closer: after a successful Commit this returns
		// the guaranteed ErrTxClosed; on a failure path the server aborts
		// the transaction with its session either way.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := setBuildSession(ctx, tx, target, opts); err != nil {
		return BuiltShadow{}, err
	}

	oid, err := resolveRelation(ctx, tx, target.Schema(), target.Table())
	if err != nil {
		return BuiltShadow{}, err
	}
	if err := refuseExistingShadow(ctx, tx, target.Schema(), shadow); err != nil {
		return BuiltShadow{}, err
	}
	if err := checkDependentNameLengths(ctx, tx, target, oid); err != nil {
		return BuiltShadow{}, err
	}
	sourceModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), target.Table())
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect source %s.%s: %w", target.Schema(), target.Table(), err)
	}
	fidelity, err := readFidelity(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}
	identities, err := readIdentityColumns(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}

	if err := createShadow(ctx, tx, target, shadow, fidelity.Owner); err != nil {
		return BuiltShadow{}, err
	}
	if err := applyIdentityDefaults(ctx, tx, target.Schema(), shadow, identities); err != nil {
		return BuiltShadow{}, err
	}
	if err := applyFidelity(ctx, tx, target.Schema(), shadow, fidelity); err != nil {
		return BuiltShadow{}, err
	}
	if _, err := tx.Exec(ctx, retargeted); err != nil {
		return BuiltShadow{}, fmt.Errorf("apply schema change to shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	targetModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), shadow)
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BuiltShadow{}, fmt.Errorf("commit shadow build: %w", err)
	}

	sourceFingerprint, err := fingerprint(sourceModel)
	if err != nil {
		return BuiltShadow{}, err
	}
	targetFingerprint, err := fingerprint(targetModel)
	if err != nil {
		return BuiltShadow{}, err
	}
	return BuiltShadow{
		schema:            target.Schema(),
		source:            target.Table(),
		shadow:            shadow,
		sourceFingerprint: sourceFingerprint,
		targetFingerprint: targetFingerprint,
		identities:        identities,
		fidelity:          fidelity,
		copyColumns:       copyColumns(sourceModel, targetModel),
	}, nil
}

// checkProof refuses the zero CopySwapTarget: only the copy-and-swap shape
// check mints a populated one.
func checkProof(target preflight.CopySwapTarget) error {
	if target.Schema() == "" || target.Table() == "" || target.OwnerRole() == "" || target.PKColumn() == "" {
		// INV: ST-6
		return fmt.Errorf("%w: ST-6: copy-and-swap proof is empty", ErrInvariantViolation)
	}
	return nil
}

// retargetOntoShadow produces the SQL the builder executes: the gated
// statement with its one relation moved to the shadow. It first checks the
// gated statement is an ALTER TABLE naming the proven table, then re-parses
// the retargeted text and requires it to match the gated statement in every
// operation and to name the shadow — the ST-7 check with the shadow as the
// sole permitted target.
func retargetOntoShadow(st statement.Statement, target preflight.CopySwapTarget, shadow string) (string, error) {
	// INV: ST-7
	if st.Kind() != statement.KindAlterTable {
		return "", fmt.Errorf("%w: ST-7: shadow builder accepts only ALTER TABLE, got %s", ErrInvariantViolation, st.Kind())
	}
	if !namesTarget(st, target.Schema(), target.Table()) {
		return "", fmt.Errorf("%w: ST-7: statement targets %s, proof is for %s.%s", ErrInvariantViolation, st.Table(), target.Schema(), target.Table())
	}
	retargeted, err := statement.RetargetRelation(st.SQL(), target.Schema(), shadow)
	if err != nil {
		return "", fmt.Errorf("retarget statement onto shadow: %w", err)
	}
	if err := statement.SameOpsExceptTarget(st.SQL(), retargeted); err != nil {
		return "", fmt.Errorf("%w: ST-7: %w", ErrInvariantViolation, err)
	}
	reparsed, err := statement.ParseOne(retargeted)
	if err != nil {
		return "", fmt.Errorf("%w: ST-7: retargeted statement does not re-parse: %w", ErrInvariantViolation, err)
	}
	if reparsed.Schema() != target.Schema() || reparsed.Table() != shadow {
		return "", fmt.Errorf("%w: ST-7: retargeted statement names %s.%s, not the shadow %s.%s", ErrInvariantViolation, reparsed.Schema(), reparsed.Table(), target.Schema(), shadow)
	}
	return retargeted, nil
}

// namesTarget reports whether the statement's relation is the proven table:
// an unqualified statement names it through the search_path the build
// session sets to the target schema; a qualified one must name that schema.
func namesTarget(st statement.Statement, schema, table string) bool {
	if st.Table() != table {
		return false
	}
	return st.Schema() == "" || st.Schema() == schema
}

// setBuildSession bounds the transaction and puts it in the owner's shoes.
// SET LOCAL cannot take bind parameters; the timeouts are integer
// milliseconds and the search_path comes from dbconn so it upholds CO-9.
func setBuildSession(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.lockTimeout().Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.statementTimeout().Milliseconds(), 10) +
		"; " + dbconn.LocalSearchPath(target.Schema(), "public")
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set shadow build budgets: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{target.OwnerRole()}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", target.OwnerRole(), err)
	}
	return nil
}

// resolveRelation returns the OID of schema.table, resolved by explicit
// qualification rather than search_path.
func resolveRelation(ctx context.Context, tx pgx.Tx, schema, table string) (uint32, error) {
	var oid uint32
	err := tx.QueryRow(ctx, `
		SELECT c.oid
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table).Scan(&oid)
	if err != nil {
		return 0, fmt.Errorf("resolve source %s.%s: %w", schema, table, err)
	}
	return oid, nil
}

// refuseExistingShadow returns ErrShadowExists when any relation already
// wears the shadow's name in the schema.
func refuseExistingShadow(ctx context.Context, tx pgx.Tx, schema, shadow string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2)`, schema, shadow).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check for existing shadow %s.%s: %w", schema, shadow, err)
	}
	if exists {
		return fmt.Errorf("%w: %s.%s", ErrShadowExists, schema, shadow)
	}
	return nil
}

// checkDependentNameLengths refuses before the first write if any dependent
// of the source — index, extended-statistics object, or identity sequence —
// would need a retained name longer than the server allows at cutover (D8).
// The shadow's own LIKE-derived names need no check: the server shortens
// them as it invents them, and cutover renames them back to the source's.
func checkDependentNameLengths(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, oid uint32) error {
	rows, err := tx.Query(ctx, `
		SELECT i.relname FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid WHERE x.indrelid = $1
		UNION ALL
		SELECT stxname FROM pg_statistic_ext WHERE stxrelid = $1
		UNION ALL
		SELECT s.relname
		FROM pg_depend d
		JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
		WHERE d.refclassid = 'pg_class'::regclass AND d.refobjid = $1
		  AND d.classid = 'pg_class'::regclass AND d.deptype = 'i'`, oid)
	if err != nil {
		return fmt.Errorf("list dependents of %s.%s: %w", target.Schema(), target.Table(), err)
	}
	dependents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list dependents of %s.%s: %w", target.Schema(), target.Table(), err)
	}
	retained := make([]string, len(dependents))
	for i, dependent := range dependents {
		retained[i] = OldDependentName(target.Schema(), target.Table(), dependent)
	}
	return CheckIdentifierLengths(retained...)
}

// createShadow runs the LIKE clone and verifies the new relation is owned by
// the source's owner: the session is under SET ROLE owner, so any other
// answer means the role the proof carried is not the owner the catalog
// reports, and the builder fails closed before shaping the shadow further.
func createShadow(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow, owner string) error {
	source := pgx.Identifier{target.Schema(), target.Table()}.Sanitize()
	table := pgx.Identifier{target.Schema(), shadow}.Sanitize()
	if _, err := tx.Exec(ctx, "CREATE TABLE "+table+" (LIKE "+source+" INCLUDING ALL EXCLUDING IDENTITY)"); err != nil {
		return fmt.Errorf("create shadow %s: %w", table, err)
	}
	var shadowOwner string
	err := tx.QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, target.Schema(), shadow).Scan(&shadowOwner)
	if err != nil {
		return fmt.Errorf("read owner of shadow %s: %w", table, err)
	}
	if shadowOwner != owner {
		// INV: ST-5
		return fmt.Errorf("%w: ST-5: shadow %s is owned by %s, source by %s", ErrInvariantViolation, table, shadowOwner, owner)
	}
	return nil
}

// fingerprint digests a model as canonical JSON. Every name in the shadow's
// model is deterministic (D8), so a resume that rebuilds the digest from the
// catalog gets the same value for the same shape.
func fingerprint(m schemadiff.Model) (string, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("fingerprint model of %s: %w", m.Table, err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// copyColumns lists the non-generated source columns that exist on the
// shadow after the schema change, in source order.
func copyColumns(source, target schemadiff.Model) []string {
	onShadow := make(map[string]bool, len(target.Columns))
	for _, c := range target.Columns {
		onShadow[c.Name] = true
	}
	columns := make([]string, 0, len(source.Columns))
	for _, c := range source.Columns {
		if c.Generated || !onShadow[c.Name] {
			continue
		}
		columns = append(columns, c.Name)
	}
	return columns
}
