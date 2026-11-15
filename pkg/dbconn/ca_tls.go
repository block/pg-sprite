package dbconn

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCACertWithSSLModeDisable reports a connection string that selects
// sslmode=disable alongside a CA bundle: the bundle asks for a verified
// encrypted session and the mode asks for none, and neither is safe to guess.
var ErrCACertWithSSLModeDisable = errors.New("a CA bundle (--ca-cert, PGSPRITE_CA_CERT, or Config.CACertPath) requires TLS but the connection string selects sslmode=disable; remove sslmode=disable or the CA bundle")

// pinToCABundle makes every network connection attempt of pc verify-full
// against the CA bundle at cfg.CACertPath, each against its own host name, with
// no unencrypted or unverified fallback (see pinEveryAttempt).
func pinToCABundle(pc *pgxpool.Config, cfg Config) error {
	if dsnSSLMode(cfg.URL) == "disable" {
		return ErrCACertWithSSLModeDisable
	}
	roots, err := loadCABundle(cfg.CACertPath)
	if err != nil {
		return err
	}
	pinEveryAttempt(&pc.ConnConfig.Config, roots)
	return nil
}
