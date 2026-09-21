package dbconn

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"regexp"
)

// rdsGlobalBundle is the AWS RDS/Aurora global certificate bundle from
// https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem, embedded
// so RDS/Aurora connections verify out of the box with no bundle to install.
//
//go:embed rdsGlobalBundle.pem
var rdsGlobalBundle []byte

// ErrRDSBundleDoesNotCoverHost reports an RDS/Aurora endpoint whose partition
// has no roots in the embedded bundle, so the connection cannot be verified
// without a caller-supplied CA (Config.CACertPath, or sslrootcert alongside
// an explicit verifying sslmode).
var ErrRDSBundleDoesNotCoverHost = errors.New("the embedded RDS CA bundle has no roots for this endpoint's partition")

// rdsHostPattern matches Amazon RDS/Aurora hostnames under `rds.amazonaws.com`
// with an optional :port suffix. The leading `\.` ensures only legitimate
// subdomains match, so a hostname like fake-rds.amazonaws.com cannot spoof
// its way into the auto-TLS path.
//
// The match is case-insensitive because DNS is: nothing normalizes the host
// before it gets here, so an endpoint copied uppercased from a console or a
// config file is the same server and must get the same TLS.
var rdsHostPattern = regexp.MustCompile(`(?i)\.rds\.amazonaws\.com(:\d+)?$`)

// govCloudHostPattern matches RDS/Aurora endpoints in the AWS GovCloud
// partition. Unlike China (`amazonaws.com.cn`), GovCloud endpoints are
// ordinary `<name>.<hash>.us-gov-<region>.rds.amazonaws.com` names that the
// suffix check alone cannot tell apart from commercial ones.
var govCloudHostPattern = regexp.MustCompile(`(?i)\.us-gov-[a-z]+-\d+\.rds\.amazonaws\.com(:\d+)?$`)

// IsRDSHost reports whether host is an Amazon RDS/Aurora endpoint under
// `rds.amazonaws.com`, with or without a port — a statement about where the
// server lives, not about whether pg-sprite can verify it. China endpoints
// (`amazonaws.com.cn`) carry a different suffix and report false.
//
// Whether such a host gets TLS automatically is RDSBundleCovers's question.
func IsRDSHost(host string) bool {
	return rdsHostPattern.MatchString(host)
}

// RDSBundleCovers reports whether host is an RDS/Aurora endpoint whose
// partition the embedded global bundle carries roots for, so the connection
// can be verified with no CA bundle to install (see configureTLS).
//
// GovCloud endpoints report false: the embedded bundle has no roots for that
// partition, so a verify-full handshake against it fails for a reason the
// x509 error does not name. Reach them with an explicit CA bundle
// (Config.CACertPath) holding that partition's roots.
func RDSBundleCovers(host string) bool {
	return IsRDSHost(host) && !govCloudHostPattern.MatchString(host)
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
