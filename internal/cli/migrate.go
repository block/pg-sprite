package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// run is the migrate flow: parse the statement, gate its type before
// dialing, and hand it to the engine's imperative pipeline (pkg/migrate),
// which classifies, routes, executes, and ends in exactly one verdict.
// Refusal verdicts are printed to out and returned as verdict.ErrRefused so
// the entry point maps them to the refusal exit code; an execution failure
// prints a failed verdict — the stable executor code plus the committed
// prefix — and still returns the operational error. --dry-run diverts to
// the classify-and-route plan instead.
func (c *MigrateCmd) run(ctx context.Context, out io.Writer) error {
	if c.DryRun {
		return c.runDryRun(ctx, out)
	}
	logger := c.diag()
	st, err := statement.ParseOne(c.Alter)
	if err != nil {
		return err
	}
	logger.Debug("statement parsed", "kind", st.Kind(), "schema", st.Schema(), "table", st.Table())
	// Gate before dialing so an unsupported statement kind refuses without
	// a database connection. Run re-checks the gate — this early check is
	// an ordering choice, not the safety boundary.
	if v, refused := migrate.Gate(st); refused {
		return c.emit(out, v)
	}

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	v, runErr := migrate.Run(ctx, pool, st, migrate.Options{
		Force:             c.Force,
		MaxTableSizeBytes: int64(c.MaxTableSize),
		Budget: executor.SequenceBudget{
			Brief:      executor.Budget{LockTimeout: c.LockTimeout, StatementTimeout: c.StatementTimeout},
			Concurrent: executor.ConcurrentBudget{Overall: c.IndexBuildTimeout},
			Validate:   executor.ValidateBudget{LockTimeout: c.LockTimeout, Overall: c.ValidateTimeout},
		},
		Retry:  c.retryPolicy(),
		Logger: logger,
		Audit:  c.audit(),
	})
	if runErr != nil {
		// The failed verdict is the error's machine-readable twin on
		// stdout, while the error itself still returns so the process
		// exits 1, not the refusal code. An error without a verdict means
		// the pipeline stopped before reaching one.
		if v.Outcome == verdict.OutcomeFailed {
			if emitErr := c.emit(out, v); emitErr != nil {
				return emitErr
			}
		}
		return runErr
	}
	return c.emit(out, v)
}

func (c *MigrateCmd) retryPolicy() executor.RetryPolicy {
	// Programmatic callers do not pass through Kong's default population.
	// Preserve the safe defaults for a zero-valued command while rejecting
	// partially configured or invalid policies in the executor.
	if c.LockAttempts == 0 && c.LockBackoff == 0 && c.LockBackoffMax == 0 {
		return executor.DefaultRetryPolicy()
	}
	return executor.RetryPolicy{MaxAttempts: c.LockAttempts, InitialBackoff: c.LockBackoff, MaxBackoff: c.LockBackoffMax}
}

// emit prints the verdict in the selected format and returns ErrRefused for
// refusals so the exit code distinguishes them from operational errors. The
// JSON contract stays plain; the human rendering styles its labels.
func (c *MigrateCmd) emit(out io.Writer, v verdict.Verdict) error {
	if c.JSON {
		text, err := v.JSON()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, text); err != nil {
			return fmt.Errorf("write verdict: %w", err)
		}
	} else if err := writeVerdictText(out, c.palette(out), v); err != nil {
		return err
	}
	if v.Outcome == verdict.OutcomeRefused {
		return verdict.ErrRefused
	}
	return nil
}
