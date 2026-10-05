package schemachange

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// carrySequenceGrants makes the recreated identity sequence's ACL equal to
// the source sequence's, which by now wears its _old name and goes with
// the old table. ADD GENERATED … AS IDENTITY gives the new sequence the
// owner's default privileges and nothing else, so a role the source let
// call nextval or currval on the sequence would be refused after the swap
// unless its grants are carried over. The result is re-read and must match
// exactly, or the swap fails closed (ST-5), as the build does for the
// table's grants.
func carrySequenceGrants(ctx context.Context, tx pgx.Tx, from, to sequenceRef) error {
	want, err := readSequenceGrantsOf(ctx, tx, from)
	if err != nil {
		return err
	}
	have, err := readSequenceGrantsOf(ctx, tx, to)
	if err != nil {
		return err
	}
	if err := reconcileACL(ctx, tx, aclSequence, to.sql(), "", have, want); err != nil {
		return err
	}
	got, err := readSequenceGrantsOf(ctx, tx, to)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		// INV: ST-5
		return refuse(CauseGrantsDiffer, nil, "identity sequence %s grants differ from the source sequence's after the handoff", to.sql())
	}
	return nil
}

// sequenceRef names one sequence.
type sequenceRef struct {
	schema, name string
}

// sql is the quoted, schema-qualified name.
func (r sequenceRef) sql() string { return pgx.Identifier{r.schema, r.name}.Sanitize() }

// readSequenceGrantsOf resolves the sequence by name and reads its grants.
func readSequenceGrantsOf(ctx context.Context, tx pgx.Tx, ref sequenceRef) ([]Grant, error) {
	oid, err := resolveRelation(ctx, tx, ref.schema, ref.name)
	if err != nil {
		return nil, err
	}
	grants, err := readSequenceGrants(ctx, tx, oid)
	if err != nil {
		return nil, fmt.Errorf("sequence %s: %w", ref.sql(), err)
	}
	return grants, nil
}

// readSequenceGrants reads a sequence's effective ACL, folded per privilege
// and grantee like readGrants; the default ACL of a sequence with none set
// is the owner's USAGE, SELECT, and UPDATE.
func readSequenceGrants(ctx context.Context, tx pgx.Tx, oid uint32) ([]Grant, error) {
	grants, err := readACL(ctx, tx, oid, aclSequence)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if !isSequencePrivilege(g.Privilege) {
			return nil, fmt.Errorf("sequence grant to %s carries unknown privilege %q", granteeLabel(g), g.Privilege)
		}
	}
	return grants, nil
}

// isSequencePrivilege reports whether the server-reported privilege keyword
// is one this code knows a sequence can carry.
func isSequencePrivilege(privilege string) bool {
	switch privilege {
	case "USAGE", "SELECT", "UPDATE":
		return true
	default:
		return false
	}
}
