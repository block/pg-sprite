package decode_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// Every ForeignStateError, whichever object it is about, matches the
// sentinel a caller branches on, and says which object and why.
func TestForeignStateErrorUnwrapsToTheSentinelAndNamesTheObject(t *testing.T) {
	err := fmt.Errorf("create slot: %w", &decode.ForeignStateError{
		Object: decode.ForeignObjectPublication,
		Name:   "pgsprite_0123abcd",
		Detail: "it is FOR ALL TABLES",
	})
	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	var foreign *decode.ForeignStateError
	require.ErrorAs(t, err, &foreign)
	assert.Equal(t, decode.ForeignObjectPublication, foreign.Object)
	assert.Equal(t, "publication pgsprite_0123abcd is not this route's: it is FOR ALL TABLES", foreign.Error())

	slot := &decode.ForeignStateError{Object: decode.ForeignObjectSlot, Name: "pgsprite_0123abcd", Detail: "it is physical"}
	assert.Equal(t, "slot pgsprite_0123abcd is not this route's: it is physical", slot.Error())
	assert.True(t, errors.Is(slot, decode.ErrForeignDecodingState))
}

// The privilege refusal names the publication, the database, and the
// privilege the role lacks, and keeps the server's SQLSTATE reachable.
func TestPublicationPrivilegeErrorKeepsTheServerErrorReachable(t *testing.T) {
	server := &pgconn.PgError{Severity: "ERROR", Code: "42501", Message: "permission denied for database app"}
	err := &decode.PublicationPrivilegeError{
		Publication: "pgsprite_0123abcd",
		Database:    "app",
		Privilege:   "CREATE",
		Err:         server,
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42501", pgErr.Code)
	assert.Equal(t,
		`create publication pgsprite_0123abcd: ERROR: permission denied for database app (SQLSTATE 42501) (the role needs CREATE on database "app")`,
		err.Error())
}
