package dbconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDSNNamesSSLMode(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want bool
	}{
		{"URL without a query", "postgres://user@db.example:5432/app", false},
		{"URL with sslmode", "postgres://user@db.example:5432/app?sslmode=verify-full", true},
		{"URL with sslmode after another parameter", "postgresql://user@db.example/app?application_name=x&sslmode=disable", true},
		{"URL whose other parameter mentions sslmode in its value", "postgres://user@db.example/app?application_name=sslmode%3Dx", false},
		{"URL with sslrootcert but no sslmode", "postgres://user@db.example/app?sslrootcert=/tmp/ca.pem", false},
		{"keyword form without sslmode", "host=db.example user=app dbname=app", false},
		{"keyword form with sslmode", "host=db.example sslmode=require dbname=app", true},
		{"keyword form with whitespace around the equals sign", "host=db.example sslmode = disable", true},
		{"keyword form with sslmode as the first keyword", "sslmode=verify-ca host=db.example", true},
		{"keyword form whose key merely ends in sslmode", "host=db.example xsslmode=disable", false},
		{"keyword form with sslrootcert but no sslmode", "host=db.example sslrootcert=/tmp/ca.pem", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dsnNamesSSLMode(tt.dsn))
		})
	}
}
