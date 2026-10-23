package schemachange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/internal/chunksql"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

var (
	// ErrShadowExists reports that the deterministic shadow name is already
	// taken in the source schema: an earlier run's leftover, which a resume
	// verifies with InspectShadow or removes with DropShadow rather than
	// this builder overwriting it.
	ErrShadowExists = errors.New("shadow table already exists")
	// ErrInvariantViolation is the sentinel every shadow refusal wraps: a
	// forged or empty proof, a statement that does not target the proven
	// table, a lock the operation cannot trust, or a re-verification of the
	// engine's own work that failed. No operation executes anything after
	// raising it. The refusal itself is a *RefusalError, whose Cause says
	// which of those it was.
	ErrInvariantViolation = errors.New("invariant violation")
	// ErrInvalidOptions reports a timeout that cannot encode a positive
	// PostgreSQL setting: PostgreSQL counts timeouts in whole milliseconds,
	// and a nonzero value below one millisecond would round to zero, which
	// the server reads as no timeout at all.
	ErrInvalidOptions = errors.New("invalid shadow build options")
)

// Options bounds the shadow build transaction. Zero values take the dbconn
// session defaults, so a caller that passes Options{} still gets bounded
// waits (LK-2); any other value must be at least one millisecond.
type Options struct {
	// LockTimeout bounds every lock wait inside the build transaction.
	LockTimeout time.Duration
	// StatementTimeout bounds every statement inside the build transaction.
	StatementTimeout time.Duration
}

// validate refuses a timeout the server would read as disabled.
func (o Options) validate() error {
	for _, timeout := range []struct {
		name  string
		value time.Duration
	}{{"lock timeout", o.LockTimeout}, {"statement timeout", o.StatementTimeout}} {
		if timeout.value == 0 {
			continue
		}
		if timeout.value < time.Millisecond {
			// INV: LK-2
			return fmt.Errorf("%w: %s %s is below PostgreSQL's one-millisecond resolution; use zero for the default", ErrInvalidOptions, timeout.name, timeout.value)
		}
	}
	return nil
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
	// target is the copy-and-swap shape proof the build ran under; the
	// swap re-checks it under its own lock.
	target                 preflight.CopySwapTarget
	schema, source, shadow string
	sourceOID, shadowOID   uint32
	sourceFingerprint      string
	targetFingerprint      string
	identities             []IdentityColumn
	fidelity               FidelitySnapshot
	shadowFidelity         FidelitySnapshot
	copyColumns            []string
	conversions            map[string]string
	conversionFingerprint  string
}

// Schema is the schema holding both the source and the shadow.
func (b BuiltShadow) Schema() string { return b.schema }

// SourceTable is the source table name.
func (b BuiltShadow) SourceTable() string { return b.source }

// ShadowTable is the shadow table name.
func (b BuiltShadow) ShadowTable() string { return b.shadow }

// SourceOID is the source relation's OID as resolved inside the build
// transaction; a later stage compares it rather than re-resolving the name.
func (b BuiltShadow) SourceOID() uint32 { return b.sourceOID }

// ShadowOID is the shadow relation's OID as created by this build.
func (b BuiltShadow) ShadowOID() uint32 { return b.shadowOID }

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

// Fidelity is the source's metadata snapshot at build time, the one the
// builder replicated onto the shadow; the ST-5 gate re-reads the source and
// refuses a source that drifted from it.
func (b BuiltShadow) Fidelity() FidelitySnapshot { return b.fidelity }

// ShadowFidelity is the shadow's metadata snapshot as the gated statement
// left it. It is kept apart from Fidelity because the statement may change
// metadata on purpose — a NOT VALID constraint, a storage parameter — so
// the ST-5 gate compares the shadow against this, not against the source.
func (b BuiltShadow) ShadowFidelity() FidelitySnapshot { return b.shadowFidelity }

// CopyColumns are the columns the copier moves: every non-generated source
// column that is still the same column on the shadow after the schema change.
// Generated columns are excluded because the server computes them on insert;
// a column the change dropped is excluded because the shadow has nowhere to
// put it, and so is one the change dropped and added again under the same
// name, which is a new column holding its own default.
func (b BuiltShadow) CopyColumns() []string { return append([]string(nil), b.copyColumns...) }

// Conversions maps each copy column the gated statement retyped with a
// USING clause to that clause's expression, rendered by pkg/statement; the
// copier selects the column as the expression so every value is converted
// the way the statement declared. The catalog keeps no record of a USING
// clause, so the proof carries it from the statement; a column the change
// retyped and then dropped has no entry, since the copy does not carry it.
func (b BuiltShadow) Conversions() map[string]string { return maps.Clone(b.conversions) }

// ConversionFingerprint is the digest of the catalog entries the
// conversion expressions resolve among, read in the build's transaction
// after the gated statement ran: every function, operator, type, and
// collation under a name an expression spells, in every schema on the
// path the copy resolves them in. Every guarded write recomputes it and
// refuses when it differs, so an object created, replaced, or dropped
// under one of those names after the build cannot change what a later
// chunk lands (CO-9). It is empty exactly when Conversions is.
func (b BuiltShadow) ConversionFingerprint() string { return b.conversionFingerprint }

// BuildShadow creates the copy-and-swap shadow for the proven target and
// applies the gated ALTER TABLE to it, all in one bounded transaction under
// SET LOCAL ROLE owner: CREATE TABLE … LIKE INCLUDING ALL EXCLUDING IDENTITY, the
// identity-sequence defaults (D5), the metadata LIKE does not carry (D2), and
// finally the statement retargeted onto the shadow — after re-proving that
// the retargeted form differs from the gated one only in its target relation
// and that the target is the shadow (ST-7). Both models are introspected
// inside the same transaction, so a failure at any step leaves no shadow
// behind. An existing relation under the shadow's name is ErrShadowExists.
//
// The caller holds the per-table lock (LK-1): the build runs under the lock
// session's Bind context, so a lost lock cancels the statement in flight,
// and the transaction confirms from its own connection that the lock
// session's backend holds the table before its first write.
func BuildShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, st statement.Statement, opts Options) (BuiltShadow, error) {
	if err := opts.validate(); err != nil {
		return BuiltShadow{}, err
	}
	if err := checkProof(target); err != nil {
		return BuiltShadow{}, err
	}
	if err := requireTableLock(lock, target.Schema(), target.Table()); err != nil {
		return BuiltShadow{}, err
	}
	shadow := ShadowName(target.Schema(), target.Table())
	// The conversions are read before the retarget: both deparse the
	// statement, and a USING the deparser cannot render faithfully should
	// be refused as the conversion it is, not as a retarget that failed to
	// re-parse.
	conversions, err := columnConversions(st, target)
	if err != nil {
		return BuiltShadow{}, err
	}
	retargeted, err := retargetOntoShadow(st, target, shadow)
	if err != nil {
		return BuiltShadow{}, err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	built, err := buildShadow(ctx, pool, lock, target, shadow, retargeted, conversions, opts)
	if err != nil {
		return BuiltShadow{}, lockLossCause(lock, err)
	}
	return built, nil
}

func buildShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, shadow, retargeted string, conversions map[string]string, opts Options) (BuiltShadow, error) {
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
	if err := confirmTableLock(ctx, tx, lock); err != nil {
		return BuiltShadow{}, err
	}
	// INV: ST-6
	if err := preflight.RecheckCopySwapShape(ctx, tx, target); err != nil {
		return BuiltShadow{}, fmt.Errorf("re-check copy-and-swap shape: %w", err)
	}

	oid, err := resolveRelation(ctx, tx, target.Schema(), target.Table())
	if err != nil {
		return BuiltShadow{}, err
	}
	if err := refuseExistingShadow(ctx, tx, target.Schema(), shadow); err != nil {
		return BuiltShadow{}, err
	}
	sourceModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), target.Table())
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect source %s.%s: %w", target.Schema(), target.Table(), err)
	}
	// The introspection sets its own transaction-local search_path; the
	// build session's is re-asserted so the gated statement resolves
	// against what this builder chose, not what a callee happened to.
	if err := setSearchPath(ctx, tx, target.Schema()); err != nil {
		return BuiltShadow{}, err
	}
	fidelity, err := readFidelity(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}
	identities, err := readIdentityColumns(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}

	shadowOID, err := createShadow(ctx, tx, target, shadow, fidelity.Owner)
	if err != nil {
		return BuiltShadow{}, err
	}
	if err := applyIdentityDefaults(ctx, tx, target.Schema(), shadow, identities); err != nil {
		return BuiltShadow{}, err
	}
	if err := applyFidelity(ctx, tx, target.Schema(), shadow, shadowOID, fidelity); err != nil {
		return BuiltShadow{}, err
	}
	if err := carryExtendedStatisticsTargets(ctx, tx, target.Schema(), oid, shadowOID, fidelity.ExtendedStatisticsTargets); err != nil {
		return BuiltShadow{}, err
	}
	// The probe runs on the shadow the statement is about to run on, under
	// the same search_path and role, so it resolves every name the way the
	// statement will; a refusal rolls the whole build back with the shadow.
	if err := probeConversions(ctx, tx, target, shadow, sourceModel, conversions); err != nil {
		return BuiltShadow{}, err
	}
	if _, err := tx.Exec(ctx, retargeted); err != nil {
		return BuiltShadow{}, fmt.Errorf("apply schema change to shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	targetModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), shadow)
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	sourcePositions, err := readColumnPositions(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}
	shadowPositions, err := readColumnPositions(ctx, tx, shadowOID)
	if err != nil {
		return BuiltShadow{}, err
	}
	// The handoff is proven on the shadow the statement left, not the one
	// the defaults were applied to: the proof records exactly what an
	// inspection of this shadow would accept.
	if err := verifyIdentityDefaults(ctx, tx, shadowOID, handoffIdentities(identities, targetModel)); err != nil {
		return BuiltShadow{}, err
	}
	shadowFidelity, err := readFidelity(ctx, tx, shadowOID)
	if err != nil {
		return BuiltShadow{}, err
	}
	// The fingerprint is read after the statement ran, in the transaction
	// that ran it: the catalog it digests is the one the statement's own
	// resolution of the expressions saw.
	carried := carryConversions(sourceModel, sourcePositions, shadowPositions, conversions)
	resolution, err := chunksql.ResolutionFingerprint(ctx, tx, target.Schema(), carried.conversions)
	if err != nil {
		return BuiltShadow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BuiltShadow{}, fmt.Errorf("commit shadow build: %w", err)
	}
	return newBuiltShadow(target, shadow, oid, shadowOID, sourceModel, targetModel, identities, fidelity, shadowFidelity, carried, resolution)
}

// newBuiltShadow assembles the proof from what a build or an inspection read
// inside its transaction and from the gated statement; the fingerprints,
// copy columns, and identity handoffs are derived from the two models so
// the same catalog state always yields the same proof, and the conversions
// are kept for the copy columns only, so the same statement over the same
// catalog state does too.
func newBuiltShadow(target preflight.CopySwapTarget, shadow string, sourceOID, shadowOID uint32, sourceModel, targetModel schemadiff.Model, identities []IdentityColumn, fidelity, shadowFidelity FidelitySnapshot, carried carriedConversions, resolution string) (BuiltShadow, error) {
	sourceFingerprint, err := fingerprint(sourceModel)
	if err != nil {
		return BuiltShadow{}, err
	}
	targetFingerprint, err := fingerprint(targetModel)
	if err != nil {
		return BuiltShadow{}, err
	}
	return BuiltShadow{
		target:                target,
		schema:                target.Schema(),
		source:                target.Table(),
		shadow:                shadow,
		sourceOID:             sourceOID,
		shadowOID:             shadowOID,
		sourceFingerprint:     sourceFingerprint,
		targetFingerprint:     targetFingerprint,
		identities:            handoffIdentities(identities, targetModel),
		fidelity:              fidelity,
		shadowFidelity:        shadowFidelity,
		copyColumns:           carried.columns,
		conversions:           carried.conversions,
		conversionFingerprint: resolution,
	}, nil
}

// carriedConversions is what the copy moves: the columns the shadow kept
// and, of those, the ones it retypes through a USING expression.
type carriedConversions struct {
	columns     []string
	conversions map[string]string
}

// carryConversions derives, from the two tables' columns and the gated
// statement's conversions, the copy columns and the conversions the copy
// carries. A build and an inspection of the same shadow derive the same
// value, which is what lets the fingerprint over the conversions be read
// inside either transaction and compared across them.
func carryConversions(source schemadiff.Model, sourcePositions, shadowPositions []columnPosition, conversions map[string]string) carriedConversions {
	columns := copyColumns(source, sameColumns(sourcePositions, shadowPositions))
	return carriedConversions{columns: columns, conversions: copyConversions(conversions, columns)}
}

// columnConversions reads the USING expressions out of the gated statement
// after checking it is an ALTER TABLE naming the proven table, the same
// check the retarget makes: an expression taken from a statement about
// another table would convert this one's columns. An expression the copy
// cannot carry as the statement wrote it is refused before any shadow
// exists: one the deparser cannot prove it rendered faithfully, one that
// reads any column but the one it retypes, a column retyped twice, or a
// conversion of the primary key, whose values every chunk range,
// watermark, and resume clear of the copy are expressed in.
func columnConversions(st statement.Statement, target preflight.CopySwapTarget) (map[string]string, error) {
	// INV: ST-7
	if st.Kind() != statement.KindAlterTable {
		return nil, refuse(CauseStatementTarget, nil, "shadow builder accepts only ALTER TABLE, got %s", st.Kind())
	}
	if !namesTarget(st, target.Schema(), target.Table()) {
		return nil, refuse(CauseStatementTarget, nil, "statement targets %s, proof is for %s.%s", st.Table(), target.Schema(), target.Table())
	}
	conversions, err := statement.ColumnConversions(st.SQL())
	if err != nil {
		return nil, refuse(CauseStatementConversion, []error{err}, "the copy cannot carry a USING expression of the statement for %s.%s", target.Schema(), target.Table())
	}
	if _, converted := conversions[target.PKColumn()]; converted {
		return nil, refuse(CauseStatementConversion, nil, "the statement for %s.%s converts the primary key %s with USING", target.Schema(), target.Table(), target.PKColumn())
	}
	return conversions, nil
}

// checkProof refuses the zero CopySwapTarget: only the copy-and-swap shape
// check mints a populated one.
func checkProof(target preflight.CopySwapTarget) error {
	if target.Schema() == "" || target.Table() == "" || target.OwnerRole() == "" || target.PKColumn() == "" {
		// INV: ST-6
		return refuse(CauseProofEmpty, nil, "copy-and-swap proof is empty")
	}
	return nil
}

// retargetOntoShadow produces the SQL the builder executes: the gated
// statement with its one relation moved to the shadow. It first checks the
// gated statement is an ALTER TABLE naming the proven table, then hands the
// retargeted text to proveRetarget.
func retargetOntoShadow(st statement.Statement, target preflight.CopySwapTarget, shadow string) (string, error) {
	// INV: ST-7
	if st.Kind() != statement.KindAlterTable {
		return "", refuse(CauseStatementTarget, nil, "shadow builder accepts only ALTER TABLE, got %s", st.Kind())
	}
	if !namesTarget(st, target.Schema(), target.Table()) {
		return "", refuse(CauseStatementTarget, nil, "statement targets %s, proof is for %s.%s", st.Table(), target.Schema(), target.Table())
	}
	retargeted, err := statement.RetargetRelation(st.SQL(), target.Schema(), shadow)
	if err != nil {
		return "", fmt.Errorf("retarget statement onto shadow: %w", err)
	}
	if err := proveRetarget(st, target.Schema(), shadow, retargeted); err != nil {
		return "", err
	}
	return retargeted, nil
}

// proveRetarget is the ST-7 check with the shadow as the sole permitted
// target: the retargeted text must re-parse, match the gated statement in
// every operation, and name schema.shadow. It takes the retargeted text as
// a value so the gate is provable without trusting the retarget that
// produced it.
func proveRetarget(gated statement.Statement, schema, shadow, retargeted string) error {
	// INV: ST-7
	if err := statement.SameOpsExceptTarget(gated.SQL(), retargeted); err != nil {
		return refuse(CauseStatementTarget, []error{err}, "retargeted statement differs from the gated one")
	}
	reparsed, err := statement.ParseOne(retargeted)
	if err != nil {
		return refuse(CauseStatementTarget, []error{err}, "retargeted statement does not re-parse")
	}
	if reparsed.Schema() != schema || reparsed.Table() != shadow {
		return refuse(CauseStatementTarget, nil, "retargeted statement names %s.%s, not the shadow %s.%s", reparsed.Schema(), reparsed.Table(), schema, shadow)
	}
	return nil
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
	return setSession(ctx, tx, target.Schema(), target.OwnerRole(), opts)
}

// setSession is setBuildSession for any schema and owner role: the build
// takes both from the copy-and-swap proof, the cutover gate from the built
// shadow's snapshot.
func setSession(ctx context.Context, tx pgx.Tx, schema, owner string, opts Options) error {
	if err := setBudgets(ctx, tx, opts); err != nil {
		return err
	}
	if err := setSearchPath(ctx, tx, schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{owner}.Sanitize()); err != nil {
		return fmt.Errorf("set owner role %s: %w", owner, err)
	}
	return nil
}

// setBudgets bounds the transaction's lock and statement waits from opts.
// SET LOCAL cannot take bind parameters; the timeouts are integer
// milliseconds.
func setBudgets(ctx context.Context, tx pgx.Tx, opts Options) error {
	// INV: LK-2
	budgets := "SET LOCAL lock_timeout = " + strconv.FormatInt(opts.lockTimeout().Milliseconds(), 10) +
		"; SET LOCAL statement_timeout = " + strconv.FormatInt(opts.statementTimeout().Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return fmt.Errorf("set shadow build budgets: %w", err)
	}
	return nil
}

// setSearchPath puts the target schema first on the transaction-local
// search_path, catalog-first (CO-9), so the gated statement's unqualified
// names resolve to the proven table's schema. It is the path chunksql
// chooses for a converted copy, so a USING expression resolves its names
// in the copy exactly as it did when the statement ran on the shadow.
func setSearchPath(ctx context.Context, tx pgx.Tx, schema string) error {
	// INV: CO-9
	if _, err := tx.Exec(ctx, chunksql.SearchPath(schema, true)); err != nil {
		return fmt.Errorf("set shadow build search_path: %w", err)
	}
	return nil
}

// resolveRelation returns the OID of schema.table, resolved by explicit
// qualification rather than search_path.
func resolveRelation(ctx context.Context, tx pgx.Tx, schema, table string) (uint32, error) {
	var oid uint32
	// INV: CO-9 — every catalog name is qualified, so a planted overload in a
	// searched schema cannot answer this read.
	err := tx.QueryRow(ctx, `
		SELECT c.oid
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`, schema, table).Scan(&oid)
	if err != nil {
		return 0, fmt.Errorf("resolve source %s.%s: %w", schema, table, err)
	}
	return oid, nil
}

// refuseExistingShadow returns ErrShadowExists when any relation already
// wears the shadow's name in the schema.
func refuseExistingShadow(ctx context.Context, tx pgx.Tx, schema, shadow string) error {
	var exists bool
	// INV: CO-9 — every catalog name is qualified, so a planted overload in a
	// searched schema cannot answer this read.
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2)`, schema, shadow).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check for existing shadow %s.%s: %w", schema, shadow, err)
	}
	if exists {
		return fmt.Errorf("%w: %s.%s", ErrShadowExists, schema, shadow)
	}
	return nil
}

// createShadow runs the LIKE clone and verifies the new relation is owned by
// the source's owner: the session is under SET LOCAL ROLE owner, so any other
// answer means the role the proof carried is not the owner the catalog
// reports, and the builder fails closed before shaping the shadow further.
// It returns the shadow's OID.
func createShadow(ctx context.Context, tx pgx.Tx, target preflight.CopySwapTarget, shadow, owner string) (uint32, error) {
	source := pgx.Identifier{target.Schema(), target.Table()}.Sanitize()
	table := pgx.Identifier{target.Schema(), shadow}.Sanitize()
	if _, err := tx.Exec(ctx, "CREATE TABLE "+table+" (LIKE "+source+" INCLUDING ALL EXCLUDING IDENTITY)"); err != nil {
		return 0, fmt.Errorf("create shadow %s: %w", table, err)
	}
	var oid uint32
	var shadowOwner string
	// INV: CO-9 — every catalog name is qualified, so a planted overload in a
	// searched schema cannot answer this read.
	err := tx.QueryRow(ctx, `
		SELECT c.oid, pg_catalog.pg_get_userbyid(c.relowner)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`, target.Schema(), shadow).Scan(&oid, &shadowOwner)
	if err != nil {
		return 0, fmt.Errorf("read owner of shadow %s: %w", table, err)
	}
	if shadowOwner != owner {
		// INV: ST-5
		return 0, refuse(CauseShadowOwner, nil, "shadow %s is owned by %s, source by %s", table, shadowOwner, owner)
	}
	return oid, nil
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

// copyConversions keeps the conversions of the columns the copy carries: a
// column the change retyped and then dropped has no place on the shadow
// for its converted values. No conversions is a nil map, which is what the
// proof's JSON form decodes to when the field is absent, so a proof built
// from a statement without USING equals its checkpointed copy field for
// field.
func copyConversions(conversions map[string]string, columns []string) map[string]string {
	var kept map[string]string
	for _, column := range columns {
		if expr, ok := conversions[column]; ok {
			if kept == nil {
				kept = make(map[string]string, len(conversions))
			}
			kept[column] = expr
		}
	}
	return kept
}

// copyColumns lists the non-generated source columns that are still the same
// column on the shadow after the schema change, in source order. A column the
// change dropped has nowhere to go, and one it dropped and added again under
// the same name is a new column that takes its own default, not the dropped
// column's values.
func copyColumns(source schemadiff.Model, same map[string]bool) []string {
	columns := make([]string, 0, len(source.Columns))
	for _, c := range source.Columns {
		// INV: ST-11
		if c.IsGenerated() || !same[c.Name] {
			continue
		}
		columns = append(columns, c.Name)
	}
	return columns
}
