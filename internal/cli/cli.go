// Package cli defines the pg-sprite command tree (Kong): migrate and
// status (the optimistic front door), diff and fmt (the declarative front
// door), and lint and suggest (the offline checker and advisor).
package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/alecthomas/kong"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// CLI is the root command tree.
type CLI struct {
	Version kong.VersionFlag `help:"Print version and exit."`

	Migrate MigrateCmd `cmd:"" help:"Run a schema change safely."`
	Diff    DiffCmd    `cmd:"" help:"Diff a desired-state schema file against the live schema."`
	Fmt     FmtCmd     `cmd:"" help:"Canonicalize a schema file."`
	Lint    LintCmd    `cmd:"" help:"Lint DDL for unsafe patterns."`
	Suggest SuggestCmd `cmd:"" help:"Recommend safer native forms for risky DDL."`
	Status  StatusCmd  `cmd:"" help:"Report the status of a running schema change."`
}

// New returns an empty command tree for kong.Parse.
func New() *CLI { return &CLI{} }

// DBFlags are the connection flags shared by every command that talks to the
// database, so every entry point carries the same bounded session defaults.
type DBFlags struct {
	URL              string        `help:"PostgreSQL connection URL or key=value DSN." env:"PGSPRITE_URL" required:""`
	CACert           string        `help:"CA bundle path for verify-full TLS. RDS/Aurora endpoints verify with the embedded bundle automatically." env:"PGSPRITE_CA_CERT" type:"existingfile"`
	LockTimeout      time.Duration `help:"Session lock_timeout applied to every statement." default:"3s"`
	StatementTimeout time.Duration `help:"Session statement_timeout applied to every statement." default:"30s"`
	Debug            bool          `help:"Log statement-level tracing and lifecycle diagnostics to stderr."`

	// diagOut overrides the diagnostics destination (stderr) in tests. Kong
	// ignores unexported fields.
	diagOut io.Writer
}

// Config translates the flags into the connectivity layer's configuration.
// The tracer is wired only under --debug: dbconn skips statement tracing
// entirely for a nil logger.
func (f DBFlags) Config() dbconn.Config {
	cfg := dbconn.Config{
		URL:              f.URL,
		CACertPath:       f.CACert,
		LockTimeout:      f.LockTimeout,
		StatementTimeout: f.StatementTimeout,
	}
	if f.Debug {
		cfg.Logger = f.diag()
	}
	return cfg
}

// diag returns the diagnostics logger: debug-level text on stderr (or the
// test override) under --debug, a discarding logger otherwise. Diagnostics
// never share stdout with command output.
func (f DBFlags) diag() *slog.Logger {
	if !f.Debug {
		return slog.New(slog.DiscardHandler)
	}
	out := f.diagOut
	if out == nil {
		out = os.Stderr
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// audit returns the operator audit logger: warn-level text on stderr (or the
// test override), always on — an audit record of a deliberate safety
// override must not depend on --debug. The machine-readable counterpart is
// the verdict itself.
func (f DBFlags) audit() *slog.Logger {
	out := f.diagOut
	if out == nil {
		out = os.Stderr
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// MigrateCmd runs a schema change (imperative front-end): classify the
// statement, substitute the planner's safer native sequence by default when
// the submitted form blocks, and execute every step under bounded budgets.
// Everything the engine cannot run safely is refused with an explicit
// verdict.
type MigrateCmd struct {
	DBFlags     `embed:""`
	OutputFlags `embed:""`

	Alter             string        `help:"Imperative ALTER statement to run." name:"alter" required:""`
	MaxTableSize      byteSize      `help:"Size threshold above which the optimistic attempt is skipped, measured as the table's full on-disk footprint: heap, indexes, and TOAST, all partitions (binary units: B, KiB, MiB, GiB, TiB). Planner-proven online steps (concurrent index builds, constraint validation) are not size-guarded." default:"1GiB"`
	IndexBuildTimeout time.Duration `help:"Overall bound (statement_timeout) for one concurrent index build step; expect large tables to need a generous value." default:"30m"`
	ValidateTimeout   time.Duration `help:"Overall bound (statement_timeout) for one VALIDATE CONSTRAINT step; expect large tables to need a generous value." default:"30m"`
	Force             string        `help:"Run the submitted form as-is, overriding a safer-sequence substitution or a rewrite-required/backend-unavailable refusal. The value is the typed acknowledgement: it must name the resolved schema-qualified target table exactly. The forced run is still parsed, preflighted, size-guarded, and budget-bounded; planner refusals (no known safe path) and unsupported statement kinds cannot be forced." placeholder:"SCHEMA.TABLE"`
	LockAttempts      int           `help:"Maximum bounded attempts when native DDL exceeds lock_timeout; 1 disables retry." default:"3"`
	LockBackoff       time.Duration `help:"Initial exponential backoff between lock-timeout attempts." default:"100ms"`
	LockBackoffMax    time.Duration `help:"Maximum exponential backoff between lock-timeout attempts." default:"1s"`
	DryRun            bool          `help:"Classify and route the statement, print the plan, and execute nothing."`
	JSON              bool          `help:"Emit the verdict (or dry-run plan) as JSON."`
}

// Validate rejects flag combinations with no coherent meaning. A dry run
// reports the plan pg-sprite would execute without an override, so --force
// has nothing to acknowledge there; accepting it would let a forced apply
// ship with a dry run that reported a refusal it never checked.
func (c *MigrateCmd) Validate() error {
	if c.DryRun && c.Force != "" {
		return errors.New("--force cannot be combined with --dry-run: the dry run reports the unforced plan")
	}
	return nil
}

// Run implements the migrate subcommand.
func (c *MigrateCmd) Run() error { return c.run(context.Background(), os.Stdout) }

// DiffCmd derives statements from a desired-state schema (declarative
// front-end): introspect the live table, materialize the desired state on a
// rolled-back scratch schema, and print the ordered plan without executing
// anything.
type DiffCmd struct {
	DBFlags     `embed:""`
	OutputFlags `embed:""`

	Desired string `help:"Path to the desired-state CREATE TABLE .sql file." name:"desired" type:"existingfile" required:""`
	Schema  string `help:"Schema containing the live table." default:"public"`
	JSON    bool   `help:"Emit the plan as JSON."`
	SQL     bool   `help:"Print the plan as an executable SQL script instead of the diagnostic report."`
}

// Validate rejects flag combinations with no coherent meaning: --json and
// --sql each replace the default report with a different whole-output
// format, so combining them names no single output.
func (c *DiffCmd) Validate() error {
	if c.JSON && c.SQL {
		return errors.New("--sql cannot be combined with --json: each replaces the report with a different format")
	}
	return nil
}

// Run implements the diff subcommand.
func (c *DiffCmd) Run() error { return c.run(context.Background(), os.Stdout) }

// FmtCmd canonicalizes a schema file. It is offline — no database flags.
type FmtCmd struct {
	Path string `arg:"" optional:"" help:"Schema file to format; stdin when omitted." type:"existingfile"`
}

// Run implements the fmt subcommand.
func (c *FmtCmd) Run() error { return c.runFmt(os.Stdin, os.Stdout) }

// LintCmd checks a DDL script for patterns the engine would refuse,
// rewrite, or gate. It is offline — no database flags.
type LintCmd struct {
	OutputFlags `embed:""`

	Path string `arg:"" optional:"" help:"DDL file to lint; stdin when omitted." type:"existingfile"`
	JSON bool   `help:"Emit the findings report as JSON."`
}

// Run implements the lint subcommand.
func (c *LintCmd) Run() error { return c.runLint(os.Stdin, os.Stdout) }

// SuggestCmd maps risky-as-written DDL to the safer native form the engine
// would run instead, with typed caveats. It is offline and advisory — no
// database flags, nothing executes, and it always exits zero.
type SuggestCmd struct {
	OutputFlags `embed:""`

	Path string `arg:"" optional:"" help:"DDL file to advise on; stdin when omitted." type:"existingfile"`
	JSON bool   `help:"Emit the suggestions report as JSON."`
}

// Run implements the suggest subcommand.
func (c *SuggestCmd) Run() error { return c.runSuggest(os.Stdin, os.Stdout) }

// StatusCmd reports schema-change progress.
type StatusCmd struct {
	DBFlags `embed:""`

	JSON bool `help:"Emit the session listing as JSON."`
}

// Run implements the status subcommand.
func (c *StatusCmd) Run() error { return c.run(context.Background(), os.Stdout) }
