package dbconn

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"regexp"
)

// rdsGlobalBundle is the AWS RDS/Aurora global certificate bundle from
// https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem, embedded
// so RDS/Aurora connections verify out of the box with no bundle to install.
//
//go:embed rdsGlobalBundle.pem
var rdsGlobalBundle []byte

// rdsHostPattern matches Amazon RDS/Aurora hostnames in the commercial `aws`
// partition with an optional :port suffix. The leading `\.` ensures only
// legitimate *.rds.amazonaws.com subdomains match, so a hostname like
// fake-rds.amazonaws.com cannot spoof its way into the auto-TLS path.
//
// The match is case-insensitive because DNS is: nothing normalizes the host
// before it gets here, so an endpoint copied uppercased from a console or a
// config file is the same server and must get the same TLS.
var rdsHostPattern = regexp.MustCompile(`(?i)\.rds\.amazonaws\.com(:\d+)?$`)

// govCloudHostPattern matches RDS/Aurora endpoints in the AWS GovCloud
// partition, which rdsHostPattern would otherwise accept: unlike China
// (`amazonaws.com.cn`), GovCloud endpoints are ordinary
// `<name>.<hash>.us-gov-<region>.rds.amazonaws.com` names that the suffix
// check alone cannot tell apart from commercial ones.
var govCloudHostPattern = regexp.MustCompile(`(?i)\.us-gov-[a-z]+-\d+\.rds\.amazonaws\.com(:\d+)?$`)

// IsRDSHost reports whether host is an Amazon RDS/Aurora endpoint in the
// commercial `aws` partition, with or without a port. Such hosts get TLS
// automatically against the embedded RDS global bundle (see configureTLS).
//
// GovCloud and China endpoints report false: the embedded bundle carries no
// roots for those partitions, so treating them as RDS would replace a
// connection that works today with one that fails certificate verification
// for a reason the x509 error does not name. Reach them with an explicit CA
// bundle (Config.CACertPath) holding that partition's roots.
func IsRDSHost(host string) bool {
	return rdsHostPattern.MatchString(host) && !govCloudHostPattern.MatchString(host)
}

// rdsRootPool returns a cert pool holding the embedded RDS global bundle.
func rdsRootPool() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rdsGlobalBundle) {
		// The bundle is embedded at compile time, so failing to parse it is
		// a build defect, not an operating error.
		panic("embedded RDS global bundle contains no usable certificates")
	}
	return pool
}

// rdsTLSConfig returns a verify-full TLS config for an RDS/Aurora host using
// the embedded global bundle.
func rdsTLSConfig(host string) *tls.Config {
	return &tls.Config{
		RootCAs:    rdsRootPool(),
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
}
