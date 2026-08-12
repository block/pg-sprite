//go:build ministack

package testutil

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuroraControlPlaneURLEscaping proves connection URLs survive
// RDS-generated passwords containing URL-reserved characters: the harness
// does not choose the master password, so every character the generator
// may emit must round-trip through URL construction and pgx parsing back
// to the identical password. Needs no Docker — it exercises only the URL
// seam — but lives in the ministack tier with the code it proves.
func TestAuroraControlPlaneURLEscaping(t *testing.T) {
	cluster := &AuroraCluster{addr: "db.internal.example:5432"}
	for _, password := range []string{
		"pass%zzword",      // invalid percent-escape when interpolated raw
		"pass%40word",      // decodes to a different password when unescaped
		"pass#word",        // fragment truncation
		"pass?sslmode=off", // query truncation
		"pass:word",        // userinfo separator
		"pass&word=1",      // query separator
		"pass word",        // space
		"pass@word/end",    // authority and path separators
	} {
		cfg, err := pgx.ParseConfig(cluster.urlWithPassword(password))
		require.NoErrorf(t, err, "URL with password %q must parse", password)
		assert.Equalf(t, password, cfg.Password, "password %q must round-trip", password)
		assert.Equal(t, fixtureUser, cfg.User)
		assert.Equal(t, "db.internal.example", cfg.Host)
		assert.Equal(t, uint16(5432), cfg.Port)
		assert.Equal(t, fixtureDatabase, cfg.Database)
	}
}
