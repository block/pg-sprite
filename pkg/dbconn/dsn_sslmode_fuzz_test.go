package dbconn

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// FuzzDSNSSLModeAgreesWithPGX holds the scanners to pgx beyond the fixed
// tables: for any string pgx parses into a single-host TCP config whose TLS
// attempts name one sslmode unambiguously, the mode the scanner reads (pgx's
// default, prefer, when it reads none) is that mode. Strings pgx rejects never
// reach configureTLS, so they only seed the corpus: their mutations are where
// an escaped delimiter or a repeated alias is most likely to split the two.
// Run it beyond the seeds with
// go test -run '^$' -fuzz FuzzDSNSSLModeAgreesWithPGX ./pkg/dbconn/
func FuzzDSNSSLModeAgreesWithPGX(f *testing.F) {
	clearSSLEnv(f)
	for _, dsn := range pgxAcceptedDSNs {
		f.Add(dsn)
	}
	for _, dsn := range pgxRejectedDSNs {
		f.Add(dsn)
	}
	f.Fuzz(func(t *testing.T, dsn string) {
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			return
		}
		want, ok := pgxTLSMode(cfg)
		if !ok {
			return
		}
		got := dsnSSLMode(dsn)
		if got == "" {
			got = "prefer"
		}
		if got != want {
			t.Fatalf("scanner reads sslmode %q from %q, pgx applied %q", got, dsn, want)
		}
	})
}

// pgxTLSMode names the sslmode pgx applied, read from the TLS attempts it
// built, or reports false when the attempts do not name one: a Unix socket
// (no TLS), more than one host, a root certificate (require then behaves as
// verify-ca, and sslrootcert=system forces verify-full), or direct TLS
// negotiation (prefer then behaves as require).
func pgxTLSMode(cfg *pgconn.Config) (string, bool) {
	if strings.HasPrefix(cfg.Host, "/") {
		return "", false
	}
	for _, fb := range cfg.Fallbacks {
		if fb.Host != cfg.Host || fb.Port != cfg.Port {
			return "", false
		}
	}
	if anyTLS(cfg, func(tc *tls.Config) bool { return tc.RootCAs != nil || len(tc.NextProtos) > 0 }) {
		return "", false
	}
	tc := cfg.TLSConfig
	switch {
	case tc == nil && len(cfg.Fallbacks) == 0:
		return "disable", true
	case tc == nil && len(cfg.Fallbacks) == 1 && cfg.Fallbacks[0].TLSConfig != nil:
		return "allow", true
	case tc == nil:
		return "", false
	case len(cfg.Fallbacks) == 1 && cfg.Fallbacks[0].TLSConfig == nil:
		return "prefer", true
	case len(cfg.Fallbacks) != 0:
		return "", false
	case tc.VerifyPeerCertificate != nil:
		return "verify-ca", true
	case tc.InsecureSkipVerify:
		return "require", true
	case tc.ServerName == cfg.Host:
		return "verify-full", true
	default:
		return "", false
	}
}

// anyTLS reports whether any TLS attempt in cfg satisfies pred.
func anyTLS(cfg *pgconn.Config, pred func(*tls.Config) bool) bool {
	if cfg.TLSConfig != nil && pred(cfg.TLSConfig) {
		return true
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig != nil && pred(fb.TLSConfig) {
			return true
		}
	}
	return false
}
