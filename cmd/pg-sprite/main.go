// Command pg-sprite is an online schema-change engine for PostgreSQL.
package main

import (
	"os"

	"github.com/block/pg-sprite/internal/cli"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(cli.Main(os.Args[1:], version, os.Stdout, os.Stderr))
}
