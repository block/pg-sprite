// Package cli defines the pg-sprite command tree. All subcommands are Phase 0
// stubs; each later build-plan phase fills one in.
package cli

import "fmt"

// CLI is the root command tree.
type CLI struct {
	Migrate MigrateCmd `cmd:"" help:"Run a schema change safely."`
	Diff    DiffCmd    `cmd:"" help:"Diff a desired-state schema file against the live schema."`
	Fmt     FmtCmd     `cmd:"" help:"Canonicalize a schema file."`
	Lint    LintCmd    `cmd:"" help:"Lint DDL for unsafe patterns."`
	Status  StatusCmd  `cmd:"" help:"Report the status of a running migration."`
}

// New returns an empty command tree for kong.Parse.
func New() *CLI { return &CLI{} }

func notImplemented(cmd string) error {
	return fmt.Errorf("%s: not implemented yet (Phase 0 stub)", cmd)
}

// MigrateCmd runs a schema change (imperative front-end).
type MigrateCmd struct {
	Alter string `help:"Imperative ALTER statement to run." name:"alter"`
}

// Run implements the migrate subcommand.
func (c *MigrateCmd) Run() error { return notImplemented("migrate") }

// DiffCmd derives statements from a desired-state schema (declarative front-end).
type DiffCmd struct {
	Desired string `help:"Path to the desired-state CREATE TABLE .sql file." name:"desired" type:"existingfile"`
}

// Run implements the diff subcommand.
func (c *DiffCmd) Run() error { return notImplemented("diff") }

// FmtCmd canonicalizes a schema file.
type FmtCmd struct {
	Path string `arg:"" optional:"" help:"Schema file to format." type:"existingfile"`
}

// Run implements the fmt subcommand.
func (c *FmtCmd) Run() error { return notImplemented("fmt") }

// LintCmd checks DDL for unsafe or unsupported patterns.
type LintCmd struct{}

// Run implements the lint subcommand.
func (c *LintCmd) Run() error { return notImplemented("lint") }

// StatusCmd reports migration progress.
type StatusCmd struct{}

// Run implements the status subcommand.
func (c *StatusCmd) Run() error { return notImplemented("status") }
