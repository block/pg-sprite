package dbconn

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrMixedRDSAndOtherHosts reports a connection string that names no sslmode
// and lists an RDS/Aurora endpoint alongside a host that is not one. The
// embedded RDS bundle verifies only the endpoint, and leaving the other host
// on pgx's default would let it fall back to an unverified or plaintext
// session, so neither is chosen silently.
var ErrMixedRDSAndOtherHosts = errors.New("the connection string lists an RDS/Aurora endpoint alongside a host that is not one, so the embedded RDS CA bundle cannot verify every host")

// listsRDSHost reports whether any network host of cc is an RDS/Aurora
// endpoint.
func listsRDSHost(cc *pgconn.Config) bool {
	return slices.ContainsFunc(networkHosts(cc), IsRDSHost)
}

// pinToRDSBundle auto-enables verify-full with the embedded RDS bundle on a
// connection string that names no sslmode: every network host keeps one
// verified attempt (see pinEveryAttempt), so multi-host failover survives.
//
// Every network host must be an RDS/Aurora endpoint the bundle covers; the
// first that is not is refused by name, before anything is changed.
func pinToRDSBundle(cc *pgconn.Config) error {
	for _, host := range networkHosts(cc) {
		if !IsRDSHost(host) {
			return mixedRDSHostsError(host)
		}
		if !RDSBundleCovers(host) {
			return uncoveredRDSHostError(host)
		}
	}
	pinEveryAttempt(cc, rdsRootPool())
	return nil
}

// supplyRDSRoots honors the caller's explicit sslmode on every attempt, but
// gives each RDS/Aurora attempt that asks for verification without roots
// the embedded RDS roots, which are not in system trust stores. Other
// attempts — modes that need no roots, attempts with roots of their own,
// hosts that are not RDS/Aurora endpoints — are left as pgx parsed them.
//
// An RDS/Aurora attempt asking for verification in a partition the bundle
// does not cover is refused by name rather than left to fail its handshake.
func supplyRDSRoots(cc *pgconn.Config) error {
	roots := rdsRootPool()
	primary, err := withRDSRoots(cc.Host, cc.TLSConfig, roots)
	if err != nil {
		return err
	}
	cc.TLSConfig = primary
	for _, fb := range cc.Fallbacks {
		attempt, err := withRDSRoots(fb.Host, fb.TLSConfig, roots)
		if err != nil {
			return err
		}
		fb.TLSConfig = attempt
	}
	return nil
}

// withRDSRoots returns the TLS config for one attempt at host: c itself,
// or, when host is an RDS/Aurora endpoint and c asks for verification
// without roots, a copy of c verifying against roots.
func withRDSRoots(host string, c *tls.Config, roots *x509.CertPool) (*tls.Config, error) {
	if !IsRDSHost(host) {
		return c, nil
	}
	if !requestsVerificationWithoutRoots(c) {
		return c, nil
	}
	if !RDSBundleCovers(host) {
		return nil, uncoveredRDSHostError(host)
	}
	c = c.Clone()
	c.RootCAs = roots
	return c, nil
}

// mixedRDSHostsError names the host the embedded bundle cannot verify beside
// an RDS/Aurora endpoint and the two ways to verify every host.
func mixedRDSHostsError(host string) error {
	return fmt.Errorf("configure TLS for %s: %w; set CACertPath to a CA bundle that verifies every host, "+
		"or choose an sslmode in the connection string (sslmode=verify-full verifies RDS/Aurora endpoints "+
		"against the embedded bundle and other hosts against the system roots)",
		host, ErrMixedRDSAndOtherHosts)
}
