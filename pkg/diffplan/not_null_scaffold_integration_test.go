package diffplan_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A column the desired file makes NOT NULL plans as the online sequence: a
// helper CHECK added NOT VALID, its validation, SET NOT NULL, and the
// helper's drop. The plan records the helper and the definition its add
// leaves as the constraint the sequence validates, so the validation step
// checks it is still that helper, and a caller reading the plan sees the
// validation is the plan's own. It also records the helper as it stands once
// validated as the object the last step drops, so a caller that runs each
// step on its own checks the drop. Both definitions match what the catalog
// prints, including for a column whose name needs quoting.
func TestPlanSetNotNullRecordsTheHelperItValidates(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tests := []struct {
		name       string
		live       string
		desired    string
		constraint string
		def        string
	}{
		{
			name: "a plain column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					email text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					email text NOT NULL
				);`,
			constraint: "orders_email_not_null_check",
			def:        "CHECK ((email IS NOT NULL)) NOT VALID",
		},
		{
			name: "a mixed-case column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					"Email" text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					"Email" text NOT NULL
				);`,
			constraint: "orders_Email_not_null_check",
			def:        `CHECK (("Email" IS NOT NULL)) NOT VALID`,
		},
		{
			name: "a keyword column",
			live: `
				CREATE TABLE %s.orders (
					id bigint PRIMARY KEY,
					"order" text
				)`,
			desired: `
				CREATE TABLE orders (
					id bigint PRIMARY KEY,
					"order" text NOT NULL
				);`,
			constraint: "orders_order_not_null_check",
			def:        `CHECK (("order" IS NOT NULL)) NOT VALID`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tt.live, schema))
			require.NoError(t, err)

			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, tt.desired)})
			require.NoError(t, err)

			require.Len(t, report.Statements, 1)
			st := report.Statements[0]
			assert.Equal(t, schemadiff.ChangeSetNotNull, st.Kind)
			require.Len(t, st.ExecSQL, 4)
			assert.Contains(t, st.ExecSQL[1], "VALIDATE CONSTRAINT")
			assert.Equal(t, &schemadiff.ConstraintValidation{Constraint: tt.constraint, Def: tt.def}, st.Validates)

			// The recorded definition is the one the catalog prints once the
			// helper is added, which the validation step compares it to.
			_, err = pool.Exec(t.Context(), st.ExecSQL[0])
			require.NoError(t, err)
			var def string
			require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_catalog.pg_get_constraintdef(oid)
				FROM pg_catalog.pg_constraint
				WHERE conrelid = $1::regclass AND conname = $2`, schema+".orders", tt.constraint).Scan(&def))
			assert.Equal(t, tt.def, def)

			// The recorded drop is the helper as the executor reads it once
			// validated, which the drop step compares it to.
			assert.Equal(t, &schemadiff.DroppedObject{
				Kind:       schemadiff.DroppedConstraint,
				Name:       tt.constraint,
				Definition: strings.TrimSuffix(tt.def, " NOT VALID"),
			}, st.DropsHelper)
			_, err = pool.Exec(t.Context(), st.ExecSQL[1])
			require.NoError(t, err)
			live, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "orders", schemadiff.DroppedConstraint, tt.constraint)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, *st.DropsHelper, live)
			assert.False(t, st.Destructive, "the helper is the plan's own, not live structure")
		})
	}
}
