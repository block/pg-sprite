package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The privilege vocabulary is what keeps a server this code does not know
// from having new keywords spliced into a GRANT: every keyword a table can
// carry through PostgreSQL 17 is accepted, and anything else is not.
func TestIsTablePrivilegeKnowsTheTableVocabulary(t *testing.T) {
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"} {
		assert.True(t, isTablePrivilege(privilege), privilege)
	}
	for _, privilege := range []string{"USAGE", "EXECUTE", "CONNECT", "CREATE", "select", ""} {
		assert.False(t, isTablePrivilege(privilege), privilege)
	}
}

// Columns carry a strict subset of the table vocabulary.
func TestIsColumnPrivilegeKnowsTheColumnVocabulary(t *testing.T) {
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"} {
		assert.True(t, isColumnPrivilege(privilege), privilege)
	}
	for _, privilege := range []string{"DELETE", "TRUNCATE", "TRIGGER", "MAINTAIN", ""} {
		assert.False(t, isColumnPrivilege(privilege), privilege)
	}
}

// pg_policy.polcmd codes map onto CREATE POLICY's FOR keywords; an unknown
// code is reported rather than rendered.
func TestPolicyCommandMapsEveryCatalogCode(t *testing.T) {
	for code, want := range map[string]string{"*": "ALL", "r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE"} {
		got, known := policyCommand(code)
		assert.True(t, known, code)
		assert.Equal(t, want, got)
	}
	_, known := policyCommand("x")
	assert.False(t, known)
}

// The PUBLIC pseudo-role is rendered as the bare keyword; every real role,
// including one named "PUBLIC", is quoted so it cannot be read as the keyword.
func TestRoleSQLQuotesEveryRealRole(t *testing.T) {
	assert.Equal(t, "PUBLIC", roleSQL("", true))
	assert.Equal(t, `"PUBLIC"`, roleSQL("PUBLIC", false))
	assert.Equal(t, `"app reader"`, roleSQL("app reader", false))
}

// GRANT and REVOKE render the privilege, the optional column, the grantee,
// and the grant option in the server's syntax.
func TestGrantAndRevokeSQL(t *testing.T) {
	reader := Grant{Privilege: "SELECT", Grantee: "reader", Grantable: true}
	public := Grant{Privilege: "INSERT", Public: true}
	assert.Equal(t, `GRANT SELECT ON TABLE "s"."t" TO "reader" WITH GRANT OPTION`, grantSQL(`"s"."t"`, "", reader))
	assert.Equal(t, `GRANT INSERT ("balance") ON TABLE "s"."t" TO PUBLIC`, grantSQL(`"s"."t"`, "balance", public))
	assert.Equal(t, `REVOKE SELECT ON TABLE "s"."t" FROM "reader"`, revokeSQL(`"s"."t"`, "", reader, false))
	assert.Equal(t, `REVOKE GRANT OPTION FOR SELECT ("balance") ON TABLE "s"."t" FROM "reader"`, revokeSQL(`"s"."t"`, "balance", reader, true))
}
