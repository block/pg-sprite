package schemachange

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// ErrShadowNotFound reports that no relation wears the shadow's name in the
// source schema: there is nothing to inspect or drop.
var ErrShadowNotFound = errors.New("shadow table not found")

// InspectShadow re-derives the BuiltShadow proof for a shadow an earlier
// build left behind, from the catalog alone, in one bounded read
// transaction under SET LOCAL ROLE owner. It is the resume path: the caller
// compares the returned fingerprints with its checkpoint before trusting the
// shadow, since the gated statement is not recoverable from the catalog.
// What the catalog can prove, it proves here: the source still has the
// proven shape (ST-6), the shadow is a table owned by the source's owner
// (ST-5), and every source identity column the change kept on the shadow
// still draws its default from the source sequence there (D5) — a shadow
// whose default was stripped would silently insert nulls into its key. A
// missing shadow is ErrShadowNotFound.
func InspectShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) (BuiltShadow, error) {
	if err := opts.validate(); err != nil {
		return BuiltShadow{}, err
	}
	if err := checkProof(target); err != nil {
		return BuiltShadow{}, err
	}
	if err := requireTableLock(lock, target); err != nil {
		return BuiltShadow{}, err
	}
	ctx, stop := lock.Bind(ctx)
	defer stop()
	built, err := inspectShadow(ctx, pool, lock, target, opts)
	if err != nil {
		return BuiltShadow{}, lockLossCause(lock, err)
	}
	return built, nil
}

func inspectShadow(ctx context.Context, pool *pgxpool.Pool, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, opts Options) (BuiltShadow, error) {
	shadow := ShadowName(target.Schema(), target.Table())
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("begin shadow inspection: %w", err)
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
	fidelity, err := readFidelity(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}
	// The live owner, not the proof's: the shadow was created under the
	// proof's owner, so an owner the source acquired since the proof is
	// shape drift and is refused like any other.
	shadowOID, err := resolveShadow(ctx, tx, target.Schema(), shadow, fidelity.Owner)
	if err != nil {
		return BuiltShadow{}, err
	}
	sourceModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), target.Table())
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect source %s.%s: %w", target.Schema(), target.Table(), err)
	}
	targetModel, err := schemadiff.IntrospectTx(ctx, tx, target.Schema(), shadow)
	if err != nil {
		return BuiltShadow{}, fmt.Errorf("introspect shadow %s.%s: %w", target.Schema(), shadow, err)
	}
	identities, err := readIdentityColumns(ctx, tx, oid)
	if err != nil {
		return BuiltShadow{}, err
	}
	// Only the identity columns the change kept on the shadow carry a
	// handoff; one the change dropped has no default to verify, and the
	// fingerprint comparison the caller makes is what tells a dropped
	// column apart from a tampered shadow.
	if err := verifyIdentityDefaults(ctx, tx, shadowOID, handoffIdentities(identities, targetModel)); err != nil {
		return BuiltShadow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BuiltShadow{}, fmt.Errorf("commit shadow inspection: %w", err)
	}
	return newBuiltShadow(target, shadow, oid, shadowOID, sourceModel, targetModel, identities, fidelity)
}

// resolveShadow returns the OID of the relation wearing the shadow's name,
// refusing anything that is not a plain table owned by the source's owner:
// such a relation is not a shadow this engine built, and neither inspection
// nor drop may treat it as one. No relation at all is ErrShadowNotFound.
func resolveShadow(ctx context.Context, tx pgx.Tx, schema, shadow, owner string) (uint32, error) {
	var oid uint32
	var relkind, relOwner string
	err := tx.QueryRow(ctx, `
		SELECT c.oid, c.relkind::text, pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, shadow).Scan(&oid, &relkind, &relOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s.%s", ErrShadowNotFound, schema, shadow)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve shadow %s.%s: %w", schema, shadow, err)
	}
	if relkind != relkindOrdinaryTable {
		// INV: ST-5
		return 0, refuse(CauseForeignRelation, nil, "%s.%s is a relation of kind %q, not a shadow table", schema, shadow, relkind)
	}
	if relOwner != owner {
		// INV: ST-5
		return 0, refuse(CauseForeignRelation, nil, "shadow %s.%s is owned by %s, source by %s", schema, shadow, relOwner, owner)
	}
	return oid, nil
}

// relkindOrdinaryTable is pg_class.relkind for a plain table.
const relkindOrdinaryTable = "r"
