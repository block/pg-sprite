package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// runDryRun is the imperative dry-run flow: the identical classify-and-route
// pipeline the declarative front-end uses, with the diff step skipped — the
// submitted statement feeds the classifier directly. It prints the routed
// plan and never executes anything. The statement-type gate runs first,
// exactly as it does on apply, so a gated kind dry-runs to the same refusal
// verdict (and refusal exit code) the apply would end in — for a
// single-index DROP INDEX after the same catalog read of the index's kind
// the apply makes; so does the refusal of any statement but the stopped
// one while a copy-and-swap run on the table is stopped, the
// destructive-change gate, unless --allow-destructive names the table, the
// closed copy-and-swap route, unless --copy-and-swap opens it, and
// the executor's shape admission of the routed steps, once they are known, when
// no earlier apply check refuses first, the partitioned index and key
// builds' check of a unique key against the tree's partition keys, and the
// copy-and-swap route's refusal of a table in a partition or inheritance
// tree. The checks that need the apply's
// live facts — the size guard and the privilege preflight, which the apply
// runs before admission, and the rest of the copy-and-swap preflight — are
// not run. Introspecting the
// target table sharpens type-change classification; a missing table means
// zero facts and a strictly more conservative plan.
func (c *MigrateCmd) runDryRun(ctx context.Context, out io.Writer) error {
	logger := c.diag()
	st, err := statement.ParseOne(c.Alter)
	if err != nil {
		return err
	}
	logger.Debug("statement parsed", "kind", st.Kind(), "schema", st.Schema(), "table", st.Table())
	if v, refused := migrate.Gate(st); refused && !migrate.GateReadsCatalog(st) {
		return c.emit(out, v)
	}

	pool, err := dbconn.NewPool(ctx, c.Config())
	if err != nil {
		return err
	}
	defer pool.Close()

	// A single-index DROP INDEX is refused on apply too, but which refusal
	// depends on whether the index is partitioned, so the dry run reads
	// the same catalog the apply does.
	gated, gateRefused, err := migrate.GateWithCatalog(ctx, pool, st)
	if err != nil {
		return err
	}
	if gateRefused {
		return c.emit(out, gated)
	}

	facts, err := migrate.LiveFacts(ctx, pool, st)
	if err != nil {
		return err
	}
	// The report carries the engine's canonical rendering, not an echo of
	// the submitted text, so both front doors describe the same change
	// with the same string (and the fingerprint agrees across them).
	canonical, err := statement.Canonical(st.SQL())
	if err != nil {
		return err
	}
	classified, err := planner.Classify(canonical, facts.Classifier)
	if err != nil {
		return err
	}
	routed := router.Route([]planner.Plan{classified})
	logger.Debug("statement routed",
		"route", string(classified.Route), "disposition", string(routed.Disposition))
	// The apply refuses any statement but the stopped one while a
	// copy-and-swap run on the table is stopped, once it has routed the
	// statement and before its other gates, and so does the dry run.
	v, refused, err := migrate.AdmitStoppedRun(ctx, pool, st, facts.Schema, facts.Table)
	if err != nil {
		return err
	}
	if refused {
		return c.emit(out, v)
	}
	// The apply refuses a destructive statement before any route runs it,
	// so without the acknowledgement the dry run ends in that same refusal
	// verdict; with it, the plan below is the one the acknowledged apply
	// takes.
	v, refused, err = migrate.AdmitDestructive(st, facts.Schema, routed.Statements[0], c.AllowDestructive)
	if err != nil {
		return err
	}
	if refused {
		return c.emit(out, v)
	}

	report := plan.NewReport(plan.SourceAlter)
	report.Schema = facts.Schema
	report.Table = facts.Table
	report.TableExists = facts.TableExists
	// The apply refuses a statement that would run through copy-and-swap
	// unless --copy-and-swap opens the route, next, and so does the dry run,
	// asking the same gate with the options the apply builds from the same
	// flags. A missing target table keeps its table-not-found report, which
	// exits with the refusal code too: an unqualified one is where the apply
	// stops first.
	if !tableMissing(report) {
		v, refused, err = migrate.AdmitCopySwapRoute(ctx, pool, st, facts.Schema, facts.Table,
			routed.Statements[0], c.options(logger))
		if err != nil {
			return err
		}
		if refused {
			return c.emit(out, v)
		}
	}
	if report.ServerVersion, err = dbconn.ServerVersion(ctx, pool); err != nil {
		return err
	}
	report.Disposition = routed.Disposition
	for _, rs := range routed.Statements {
		ps, err := plan.FromRouted(rs)
		if err != nil {
			return fmt.Errorf("plan statement %q: %w", rs.Statement, err)
		}
		report.Statements = append(report.Statements, ps)
	}
	if facts.Target.Partitioned() {
		// An ADD PRIMARY KEY or ADD UNIQUE runs as the partitioned key build
		// on apply, routed on the submitted statement before the
		// partitioned-parent policy sees the planner's steps.
		if err = plan.RoutePartitionedKeyBuilds(&report); err != nil {
			return err
		}
		// An ADD FOREIGN KEY before PostgreSQL 18 runs as the partitioned
		// foreign key build on apply, routed on the submitted statement
		// before the partitioned-parent policy sees the planner's steps.
		if err = plan.RoutePartitionedForeignKeyBuilds(&report, facts.Target.ServerMajor()); err != nil {
			return err
		}
		if err = plan.RoutePartitionedParent(&report, facts.Target.ServerMajor()); err != nil {
			return err
		}
	}
	// A refusal the apply decides from the routed steps' shape alone is
	// knowable now, so the dry run ends in that same refusal
	// verdict instead of an executable plan. The checks that need live
	// facts (size guard, privileges) stay the apply's; a missing target
	// table is reported as such, since the apply stops there first. An
	// index rename and an enum value add are each run by their own
	// executor, not the sequence executor, so the sequence admission does
	// not apply to them.
	if !tableMissing(report) && !runsOwnExecutor(st) {
		for _, ps := range report.Statements {
			if ps.Disposition != router.DispositionExecute {
				continue
			}
			// The copy-and-swap route's preflight refuses a table in a
			// partition or inheritance tree before it builds anything,
			// and so does the dry run, from the same catalog facts.
			if ps.Backend == router.BackendCopyAndSwap {
				v, refused, admitErr := migrate.AdmitCopySwapPartitioning(ctx, pool, st, report.Schema)
				if admitErr != nil {
					return admitErr
				}
				if refused {
					return c.emit(out, v)
				}
				continue
			}
			// Only the native route's steps run through the sequence
			// executor; a copy-and-swap statement has none, since the
			// engine builds its shadow from the statement itself.
			if ps.Route != planner.RouteNative {
				continue
			}
			admit := migrate.AdmitRoutedSteps
			switch ps.Execution {
			case planner.ExecutionPartitionedIndexBuild:
				admit = migrate.AdmitPartitionedIndexRequest
			case planner.ExecutionPartitionedKeyBuild:
				admit = migrate.AdmitPartitionedKeyRequest
			case planner.ExecutionPartitionedForeignKeyBuild:
				admit = migrate.AdmitPartitionedForeignKeyRequest
			}
			v, refused, admitErr := admit(st, report.Schema, ps.ExecSQL)
			if admitErr != nil {
				return admitErr
			}
			if refused {
				return c.emit(out, v)
			}
			// A unique index or key must contain the tree's partition keys,
			// and a foreign key's referenced table must be one the build
			// takes, which only the live catalog knows; the build refuses
			// one that does not before it creates anything, and so does the
			// dry run. The key and foreign key builds also refuse a
			// sub-partitioned tree there.
			if catalogAdmit := partitionedCatalogAdmission(ps.Execution); catalogAdmit != nil {
				v, refused, admitErr = catalogAdmit(ctx, pool, st, report.Schema, ps.ExecSQL)
				if admitErr != nil {
					return admitErr
				}
				if refused {
					return c.emit(out, v)
				}
			}
			// A concurrent detach is refused when the table has a DEFAULT
			// partition, which only the live catalog knows. A detach is
			// destructive, so it got this far only under the
			// acknowledgement, which the apply's refusal records.
			v, refused, admitErr = migrate.AdmitDetachPartition(ctx, pool, st, report.Schema, ps.ExecSQL)
			if admitErr != nil {
				return admitErr
			}
			if refused {
				v.AllowedDestructive = ps.Destructive
				return c.emit(out, v)
			}
		}
	}
	report.Fingerprint = report.ComputeFingerprint()

	if c.JSON {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	} else if err := writeDryRunText(out, c.palette(out), report); err != nil {
		return err
	}
	// A plan execution would not run exits with the refusal code — the same
	// contract migrate uses for refusal verdicts — so CI can gate on the
	// dry run without parsing the report. A missing target table carries
	// the same code: the plan was classified from zero facts and running
	// without --dry-run would fail, so a gate must not read it as green.
	if report.Disposition != router.DispositionExecute || tableMissing(report) {
		return verdict.ErrRefused
	}
	return nil
}

// partitionedCatalogAdmission returns the catalog check a partitioned build
// makes before it creates anything — a unique key against the tree's
// partition keys, a foreign key's referenced table and tree — or nil for an
// execution that makes none.
func partitionedCatalogAdmission(e planner.Execution) func(context.Context, *pgxpool.Pool, statement.Statement, string, []string) (verdict.Verdict, bool, error) {
	switch e {
	case planner.ExecutionPartitionedIndexBuild:
		return migrate.AdmitPartitionedUniqueIndex
	case planner.ExecutionPartitionedKeyBuild:
		return migrate.AdmitPartitionedKeyCatalog
	case planner.ExecutionPartitionedForeignKeyBuild:
		return migrate.AdmitPartitionedForeignKeyCatalog
	default:
		return nil
	}
}

// runsOwnExecutor reports whether the apply runs the statement through an
// executor of its own rather than the sequence executor.
func runsOwnExecutor(st statement.Statement) bool {
	return st.Kind() == statement.KindRenameIndex || st.Kind() == statement.KindAddEnumValue
}
