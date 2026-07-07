package main

import (
	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/internal/cli"
)

func main() {
	k := kong.Parse(cli.New(),
		kong.Name("pg-sprite"),
		kong.Description("An online schema-change engine for Aurora PostgreSQL."),
		kong.UsageOnError(),
	)
	k.FatalIfErrorf(k.Run())
}
