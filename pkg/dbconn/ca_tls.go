package dbconn

import (
	"crypto/tls"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrCACertWithSSLModeDisable reports a connection string that selects
// sslmode=disable alongside a CA bundle: the bundle asks for a verified
// encrypted session and the mode asks for none, and neither is safe to guess.
var ErrCACertWithSSLModeDisable = errors.New("a CA bundle (--ca-cert, PGSPRITE_CA_CERT, or Config.CACertPath) requires TLS but the connection string selects sslmode=disable; remove sslmode=disable or the CA bundle")

// hostPort identifies one dial target of a possibly multi-host connection.
type hostPort struct {
	host string
	port uint16
}

// pinToCABundle makes every network connection attempt of pc verify-full
// against the CA bundle at caCertPath, each against its own host name.
//
// pgx expresses sslmode=prefer (the default) and sslmode=allow as extra
// fallback attempts per host: an unencrypted one, or an encrypted one that
// skips verification. Keeping those would let a server that fails the
// verified handshake — a certificate from another CA, or a man in the middle
// — push the pool onto a connection that is unencrypted or unauthenticated.
// Each distinct host and port keeps exactly one attempt, so multi-host
// failover stays while every attempt is verified. Each attempt keeps the
// client certificate and ALPN protocols pgx parsed for it (sslcert / sslkey,
// sslnegotiation=direct), since they authenticate the client, not the server.
//
// A unix-domain socket attempt keeps pgx's nil TLS config: PostgreSQL does
// not speak TLS over a socket, libpq ignores sslmode for one, and the socket
// never leaves the host, so there is no server to verify.
func pinToCABundle(pc *pgxpool.Config, cfg Config) error {
	if dsnSSLMode(cfg.URL) == "disable" {
		return ErrCACertWithSSLModeDisable
	}
	cc := pc.ConnConfig
	verified, err := caTLSConfig(cfg.CACertPath, cc.Host)
	if err != nil {
		return err
	}

	// The client-side settings pgx parsed for each target, from the first
	// attempt at it that carries TLS: sslmode=allow tries plaintext first.
	parsed := map[hostPort]*tls.Config{}
	note := func(target hostPort, c *tls.Config) {
		if c != nil && parsed[target] == nil {
			parsed[target] = c
		}
	}
	note(hostPort{cc.Host, cc.Port}, cc.TLSConfig)
	for _, fb := range cc.Fallbacks {
		note(hostPort{fb.Host, fb.Port}, fb.TLSConfig)
	}
	attempt := func(target hostPort) *tls.Config {
		if network, _ := pgconn.NetworkAddress(target.host, target.port); network == "unix" {
			return nil
		}
		c := verified.Clone()
		c.ServerName = target.host
		if p := parsed[target]; p != nil {
			c.Certificates = p.Certificates
			c.GetClientCertificate = p.GetClientCertificate
			c.NextProtos = p.NextProtos
		}
		return c
	}

	primary := hostPort{cc.Host, cc.Port}
	cc.TLSConfig = attempt(primary)
	seen := map[hostPort]bool{primary: true}
	fallbacks := make([]*pgconn.FallbackConfig, 0, len(cc.Fallbacks))
	for _, fb := range cc.Fallbacks {
		target := hostPort{fb.Host, fb.Port}
		if seen[target] {
			continue
		}
		seen[target] = true
		fallbacks = append(fallbacks, &pgconn.FallbackConfig{Host: fb.Host, Port: fb.Port, TLSConfig: attempt(target)})
	}
	cc.Fallbacks = fallbacks
	return nil
}
