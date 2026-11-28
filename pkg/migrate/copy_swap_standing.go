package migrate

import (
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// This file is the CLI operator's account of a copy-and-swap run stopped
// on a table changed under it: what the run leaves standing, and the two
// commands past it. The Runner names what stands on a
// *schemachange.StandingRunError beside its refusal, with its own remedy
// in the library's option names; the verdict detail names the CLI's
// commands instead, as copySwapStoppedRemedy's does, and a library host
// routes on the verdict's cause.

// sourceChangedDetail is the detail of a resume refused as
// resume-source-changed: the table was changed since the stopped run
// built its shadow, so the run cannot resume, and what it leaves stands
// until it is started over or discarded.
func sourceChangedDetail(standing *schemachange.StandingRunError) string {
	table := standing.Schema + "." + standing.Table
	return fmt.Sprintf("%s changed since this run built its shadow (for example a column or index added by hand); "+
		"the stopped run cannot resume, and it leaves %s; %s",
		table, standing.Leftovers(), standingRunRemedy(table))
}

// cutoverDriftDetail is the detail of a run the cutover gate stopped
// because the source or the shadow changed under it: the gate's own
// account, what the run leaves, and the two commands past it. A refusal
// that names nothing the run leaves is rendered as it came.
func cutoverDriftDetail(err error) string {
	standing, ok := errors.AsType[*schemachange.StandingRunError](err)
	if !ok {
		return err.Error()
	}
	refused, ok := errors.AsType[*schemachange.RefusalError](err)
	if !ok {
		return err.Error()
	}
	table := standing.Schema + "." + standing.Table
	return fmt.Sprintf("%s; the run stopped and leaves %s; %s", refused.Error(), standing.Leftovers(), standingRunRemedy(table))
}

// standingRunRemedy names the two ways past a stopped run on table that
// cannot go on: start it over, or abandon it without running the change.
func standingRunRemedy(table string) string {
	return fmt.Sprintf("start over by running the same pg-sprite migrate command with --restart %s, "+
		"or abandon the run without running the change with pg-sprite discard %s", table, table)
}
