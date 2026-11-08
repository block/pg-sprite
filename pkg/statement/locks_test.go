package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// LOCK TABLE names the tables it locks in a mode that blocks writes, from
// SHARE up and ACCESS EXCLUSIVE when no mode is given, and TRUNCATE names
// every table it empties. A weaker LOCK TABLE mode and every other
// statement name none.
func TestTablesLockedAgainstWrites(t *testing.T) {
	for sql, want := range map[string][]statement.TableName{
		"LOCK TABLE t":                                {{Name: "t"}},
		"LOCK TABLE app.t, u IN SHARE MODE":           {{Schema: "app", Name: "t"}, {Name: "u"}},
		"LOCK t IN SHARE ROW EXCLUSIVE MODE":          {{Name: "t"}},
		"LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE": nil,
		"LOCK TABLE t IN ROW EXCLUSIVE MODE":          nil,
		"LOCK TABLE t IN ACCESS SHARE MODE":           nil,
		`TRUNCATE app."Orders", t`:                    {{Schema: "app", Name: "Orders"}, {Name: "t"}},
		"TRUNCATE ONLY t":                             {{Name: "t"}},
		"ALTER TABLE t ADD COLUMN x int":              nil,
		"SELECT 1":                                    nil,
	} {
		got, err := statement.TablesLockedAgainstWrites(sql)
		require.NoError(t, err, sql)
		if want == nil {
			assert.Empty(t, got, sql)
			continue
		}
		assert.Equal(t, want, got, sql)
	}
}
