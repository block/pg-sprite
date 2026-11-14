package dbconn

import (
	"crypto/tls"
	"crypto/x509"

	"github.com/jackc/pgx/v5/pgconn"
)

// hostPort identifies one dial target of a possibly multi-host connection.
type hostPort struct {
	host string
	port uint16
}

// isUnixSocket reports whether target is a unix-domain socket, decided the
// way pgx decides it when it dials.
func isUnixSocket(target hostPort) bool {
	network, _ := pgconn.NetworkAddress(target.host, target.port)
	return network == "unix"
}

// pinEveryAttempt makes every network connection attempt of cc verify-full
// against roots, each against its own host name.
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
func pinEveryAttempt(cc *pgconn.Config, roots *x509.CertPool) {
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
		if isUnixSocket(target) {
			return nil
		}
		c := &tls.Config{
			RootCAs:    roots,
			ServerName: target.host,
			MinVersion: tls.VersionTLS12,
		}
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
	var fallbacks []*pgconn.FallbackConfig
	for _, fb := range cc.Fallbacks {
		target := hostPort{fb.Host, fb.Port}
		if seen[target] {
			continue
		}
		seen[target] = true
		fallbacks = append(fallbacks, &pgconn.FallbackConfig{Host: fb.Host, Port: fb.Port, TLSConfig: attempt(target)})
	}
	cc.Fallbacks = fallbacks
}

// networkHosts lists the host of every network attempt of cc in dial order,
// each once; unix-domain sockets are left out.
func networkHosts(cc *pgconn.Config) []string {
	var hosts []string
	seen := map[string]bool{}
	add := func(target hostPort) {
		if isUnixSocket(target) || seen[target.host] {
			return
		}
		seen[target.host] = true
		hosts = append(hosts, target.host)
	}
	add(hostPort{cc.Host, cc.Port})
	for _, fb := range cc.Fallbacks {
		add(hostPort{fb.Host, fb.Port})
	}
	return hosts
}
