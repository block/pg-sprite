package dbconn

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCACertWithSSLModeDisable reports a connection string that selects
// sslmode=disable alongside a CA bundle: the bundle asks for a verified
// encrypted session and the mode asks for none, and neither is safe to guess.
var ErrCACertWithSSLModeDisable = errors.New("CACertPath requires TLS but the connection string selects sslmode=disable")

// hostPort identifies one dial target of a possibly multi-host connection.
type hostPort struct {
	host string
	port uint16
}

// pinToCABundle makes every connection attempt of pc verify-full against the
// CA bundle at caCertPath, each against its own host name.
//
// pgx expresses sslmode=prefer (the default) and sslmode=allow as extra
// fallback attempts per host: an unencrypted one, or an encrypted one that
// skips verification. Keeping those would let a server that fails the
// verified handshake — a certificate from another CA, or a man in the middle
// — push the pool onto a connection that is unencrypted or unauthenticated.
// Each distinct host keeps exactly one attempt, so multi-host failover stays
// while every attempt is verified.
func pinToCABundle(pc *pgxpool.Config, cfg Config) error {
	if dsnSSLMode(cfg.URL) == "disable" {
		return ErrCACertWithSSLModeDisable
	}
	cc := pc.ConnConfig
	verified, err := caTLSConfig(cfg.CACertPath, cc.Host)
	if err != nil {
		return err
	}
	cc.TLSConfig = verified

	seen := map[hostPort]bool{{cc.Host, cc.Port}: true}
	fallbacks := make([]*pgconn.FallbackConfig, 0, len(cc.Fallbacks))
	for _, fb := range cc.Fallbacks {
		target := hostPort{fb.Host, fb.Port}
		if seen[target] {
			continue
		}
		seen[target] = true
		tlsCfg := verified.Clone()
		tlsCfg.ServerName = fb.Host
		fallbacks = append(fallbacks, &pgconn.FallbackConfig{Host: fb.Host, Port: fb.Port, TLSConfig: tlsCfg})
	}
	cc.Fallbacks = fallbacks
	return nil
}
