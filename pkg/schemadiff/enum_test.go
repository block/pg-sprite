package schemadiff

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func desiredEnum(t *testing.T, sql string) statement.DesiredEnumType {
	t.Helper()
	file, err := statement.ParseDesiredFile(sql)
	require.NoError(t, err)
	enumType, ok := file.EnumType()
	require.True(t, ok)
	return enumType
}

// A label the type lacks is added in its declared position: after every
// live label it is appended, and anywhere else it goes BEFORE the next
// live label, so labels added in one gap keep their declared order.
func TestDiffEnumTypeAddsEachMissingLabelInPlace(t *testing.T) {
	live := EnumType{Schema: "app", Name: "run_status", Labels: []string{"pending", "running", "failed"}}
	cases := []struct {
		name    string
		desired string
		want    []string
	}{
		{
			name:    "matching",
			desired: "CREATE TYPE run_status AS ENUM ('pending', 'running', 'failed')",
		},
		{
			name:    "appended",
			desired: "CREATE TYPE run_status AS ENUM ('pending', 'running', 'failed', 'cancelled', 'timed_out')",
			want: []string{
				`ALTER TYPE app.run_status ADD VALUE 'cancelled'`,
				`ALTER TYPE app.run_status ADD VALUE 'timed_out'`,
			},
		},
		{
			name:    "inserted in the middle",
			desired: "CREATE TYPE run_status AS ENUM ('pending', 'running', 'waiting', 'blocked', 'failed')",
			want: []string{
				`ALTER TYPE app.run_status ADD VALUE 'waiting' BEFORE 'failed'`,
				`ALTER TYPE app.run_status ADD VALUE 'blocked' BEFORE 'failed'`,
			},
		},
		{
			name:    "inserted first, in the middle, and last",
			desired: "CREATE TYPE run_status AS ENUM ('queued', 'pending', 'running', 'it''s waiting', 'failed', 'done')",
			want: []string{
				`ALTER TYPE app.run_status ADD VALUE 'queued' BEFORE 'pending'`,
				`ALTER TYPE app.run_status ADD VALUE 'it''s waiting' BEFORE 'failed'`,
				`ALTER TYPE app.run_status ADD VALUE 'done'`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := DiffEnumType(live, desiredEnum(t, tc.desired))
			require.NoError(t, err)
			var got []string
			for _, ch := range changes {
				assert.Equal(t, ChangeAddEnumValue, ch.Kind)
				got = append(got, ch.SQL)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// A type with no labels takes every desired label appended in order.
func TestDiffEnumTypeFillsAnEmptyType(t *testing.T) {
	live := EnumType{Schema: "public", Name: "status"}
	changes, err := DiffEnumType(live, desiredEnum(t, "CREATE TYPE status AS ENUM ('a', 'b')"))
	require.NoError(t, err)
	require.Len(t, changes, 2)
	assert.Equal(t, `ALTER TYPE public.status ADD VALUE 'a'`, changes[0].SQL)
	assert.Equal(t, `ALTER TYPE public.status ADD VALUE 'b'`, changes[1].SQL)
}

// PostgreSQL cannot drop or move an enum label, so a file that drops a
// live label (removed or renamed) or lists the live labels in another
// order is refused with its cause, and nothing is planned.
func TestDiffEnumTypeRefusesWhatPostgreSQLCannotDo(t *testing.T) {
	live := EnumType{Schema: "app", Name: "member_role", Labels: []string{"owner", "admin", "member"}}
	cases := []struct {
		name    string
		desired string
		cause   UnsupportedCause
		message string
	}{
		{
			name:    "removed",
			desired: "CREATE TYPE member_role AS ENUM ('owner', 'member')",
			cause:   CauseEnumValueRemoved,
			message: "enum type app.member_role carries ('admin'), which the file no longer lists",
		},
		{
			name:    "renamed",
			desired: "CREATE TYPE member_role AS ENUM ('owner', 'administrator', 'member')",
			cause:   CauseEnumValueRemoved,
			message: "rename it with ALTER TYPE ... RENAME VALUE",
		},
		{
			name:    "reordered",
			desired: "CREATE TYPE member_role AS ENUM ('owner', 'member', 'guest', 'admin')",
			cause:   CauseEnumValueReordered,
			message: "sorts its labels as ('owner', 'admin', 'member'), and the file lists them as ('owner', 'member', 'admin')",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := DiffEnumType(live, desiredEnum(t, tc.desired))
			require.ErrorIs(t, err, ErrUnsupportedChange)
			assert.Empty(t, changes)
			refused, ok := errors.AsType[*UnsupportedChangeError](err)
			require.True(t, ok)
			assert.Equal(t, tc.cause, refused.Cause)
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}

// Diffing one type against another is a caller bug, refused rather than
// diffed.
func TestDiffEnumTypeRefusesAnotherType(t *testing.T) {
	_, err := DiffEnumType(EnumType{Schema: "app", Name: "a"}, desiredEnum(t, "CREATE TYPE b AS ENUM ('x')"))
	require.ErrorIs(t, err, ErrDifferentTables)
}
