package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A key's add carries the storage parameters of the index the key owns,
// since the constraint definition the server prints leaves them out. An index
// without parameters leaves the add as it was, and an add of anything other
// than one key is refused.
func TestWithKeyIndexOptions(t *testing.T) {
	got, err := WithKeyIndexOptions(
		`ALTER TABLE "public"."events" ADD CONSTRAINT "events_email_key" UNIQUE (email) DEFERRABLE INITIALLY DEFERRED`,
		"CREATE UNIQUE INDEX events_email_key ON events USING btree (email) WITH (fillfactor='70')")
	require.NoError(t, err)
	assert.Equal(t, `ALTER TABLE public.events ADD CONSTRAINT events_email_key UNIQUE (email) WITH (fillfactor='70') DEFERRABLE INITIALLY DEFERRED`, got)

	plain := `ALTER TABLE "public"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY (id)`
	got, err = WithKeyIndexOptions(plain, "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)")
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	_, err = WithKeyIndexOptions(`ALTER TABLE events ADD CONSTRAINT events_amt_check CHECK (amt > 0)`,
		"CREATE UNIQUE INDEX events_email_key ON events USING btree (email) WITH (fillfactor='70')")
	require.ErrorIs(t, err, ErrDisallowedStatement)

	_, err = WithKeyIndexOptions(plain, "DROP INDEX events_pkey")
	require.ErrorIs(t, err, ErrNotCreateIndex)
}
