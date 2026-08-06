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

// rdsHostPattern matches Amazon RDS/Aurora hostnames with an optional :port
// suffix. The leading `\.` ensures only legitimate *.rds.amazonaws.com
// subdomains match, so a hostname like fake-rds.amazonaws.com cannot spoof
// its way into the auto-TLS path.
var rdsHostPattern = regexp.MustCompile(`\.rds\.amazonaws\.com(:\d+)?$`)

// IsRDSHost reports whether host is an Amazon RDS/Aurora endpoint.
func IsRDSHost(host string) bool {
	return rdsHostPattern.MatchString(host)
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
