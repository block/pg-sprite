package statement_test

import (
	"testing"

	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecurityStatementsQualifiesOnlyRLS(t *testing.T) {
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 CREATE INDEX idx_id ON documents (id);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (id = 7);
 COMMENT ON POLICY readers ON documents IS 'readers';`)
	require.NoError(t, err)
	sql, err := desired.SecurityStatements(`odd"schema`)
	require.NoError(t, err)
	assert.Equal(t, []string{
		`ALTER TABLE "odd""schema".documents ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY readers ON "odd""schema".documents FOR SELECT TO public USING (id = 7)`,
		`COMMENT ON POLICY readers ON "odd""schema".documents IS 'readers'`,
	}, sql)
}
