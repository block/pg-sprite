package schemachange

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// syncGrants makes the shadow's effective table ACL equal to want, the
// source's. The shadow is born with the owner's default privileges (ALTER
// DEFAULT PRIVILEGES), which may grant roles the source never did, so the
// entries the source lacks are revoked and the ones it has are granted;
// the result is re-read and must match exactly, or the builder fails closed
// before a row is ever copied into a table with the wrong readers.
func syncGrants(ctx context.Context, tx pgx.Tx, table string, shadowOID uint32, want []Grant) error {
	have, err := readGrants(ctx, tx, shadowOID)
	if err != nil {
		return err
	}
	if err := reconcileACL(ctx, tx, table, "", have, want); err != nil {
		return err
	}
	got, err := readGrants(ctx, tx, shadowOID)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		// INV: ST-5
		return refuse(CauseGrantsDiffer, nil, "shadow %s grants differ from the source after synchronisation", table)
	}
	return nil
}

// syncColumnGrants makes the shadow's effective column ACLs equal to want,
// column by column, and re-reads them to prove it.
func syncColumnGrants(ctx context.Context, tx pgx.Tx, table string, shadowOID uint32, want []ColumnGrant) error {
	have, err := readColumnGrants(ctx, tx, shadowOID)
	if err != nil {
		return err
	}
	for _, column := range columnsOf(have, want) {
		if err := reconcileACL(ctx, tx, table, column, grantsOnColumn(have, column), grantsOnColumn(want, column)); err != nil {
			return err
		}
	}
	got, err := readColumnGrants(ctx, tx, shadowOID)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		// INV: ST-5
		return refuse(CauseGrantsDiffer, nil, "shadow %s column grants differ from the source after synchronisation", table)
	}
	return nil
}

// grantee identifies who holds a privilege; two entries with the same
// grantee and privilege differ at most in their grant option.
type grantee struct {
	privilege string
	role      string
	public    bool
}

func granteeOf(g Grant) grantee {
	return grantee{privilege: g.Privilege, role: g.Grantee, public: g.Public}
}

// reconcileACL issues the REVOKE and GRANT statements that move one ACL —
// the table's when column is empty, otherwise that column's — from have to
// want. A grant option the source lacks is revoked on its own, so the
// privilege beneath it survives; a grant option the source has is added by
// re-granting with it.
func reconcileACL(ctx context.Context, tx pgx.Tx, table, column string, have, want []Grant) error {
	wanted := make(map[grantee]Grant, len(want))
	for _, w := range want {
		wanted[granteeOf(w)] = w
	}
	held := make(map[grantee]Grant, len(have))
	for _, h := range have {
		held[granteeOf(h)] = h
		w, isWanted := wanted[granteeOf(h)]
		if !isWanted {
			if _, err := tx.Exec(ctx, revokeSQL(table, column, h, false)); err != nil {
				return fmt.Errorf("revoke %s on shadow from %s: %w", privilegeLabel(h, column), granteeLabel(h), err)
			}
			continue
		}
		if h.Grantable && !w.Grantable {
			if _, err := tx.Exec(ctx, revokeSQL(table, column, h, true)); err != nil {
				return fmt.Errorf("revoke grant option for %s on shadow from %s: %w", privilegeLabel(h, column), granteeLabel(h), err)
			}
		}
	}
	for _, w := range want {
		h, isHeld := held[granteeOf(w)]
		if isHeld && h.Grantable == w.Grantable {
			continue
		}
		if isHeld && h.Grantable {
			// The surplus grant option was revoked above; the privilege
			// itself is already held.
			continue
		}
		if _, err := tx.Exec(ctx, grantSQL(table, column, w)); err != nil {
			return fmt.Errorf("grant %s on shadow to %s: %w", privilegeLabel(w, column), granteeLabel(w), err)
		}
	}
	return nil
}

// columnsOf lists, in first-seen order, every column that carries a grant on
// either side.
func columnsOf(have, want []ColumnGrant) []string {
	var columns []string
	for _, g := range slices.Concat(have, want) {
		if !slices.Contains(columns, g.Column) {
			columns = append(columns, g.Column)
		}
	}
	return columns
}

func grantsOnColumn(grants []ColumnGrant, column string) []Grant {
	var on []Grant
	for _, g := range grants {
		if g.Column == column {
			on = append(on, g.Grant)
		}
	}
	return on
}

// privilegeSQL renders the privilege clause of GRANT and REVOKE: the keyword
// alone for a table privilege, with the quoted column for a column one.
func privilegeSQL(privilege, column string) string {
	if column == "" {
		return privilege
	}
	return privilege + " (" + pgx.Identifier{column}.Sanitize() + ")"
}

// privilegeLabel names a privilege for error messages.
func privilegeLabel(g Grant, column string) string {
	if column == "" {
		return g.Privilege
	}
	return g.Privilege + " on column " + column
}

func grantSQL(table, column string, g Grant) string {
	sql := "GRANT " + privilegeSQL(g.Privilege, column) + " ON TABLE " + table + " TO " + roleSQL(g.Grantee, g.Public)
	if g.Grantable {
		sql += " WITH GRANT OPTION"
	}
	return sql
}

func revokeSQL(table, column string, g Grant, grantOptionOnly bool) string {
	sql := "REVOKE "
	if grantOptionOnly {
		sql += "GRANT OPTION FOR "
	}
	return sql + privilegeSQL(g.Privilege, column) + " ON TABLE " + table + " FROM " + roleSQL(g.Grantee, g.Public)
}
