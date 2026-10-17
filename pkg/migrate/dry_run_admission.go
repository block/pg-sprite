package migrate

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// AdmitRoutedSteps runs the sequence executor's static admission over the
// steps a dry run would execute for st, and returns the refusal verdict [Run]
// ends in when that admission refuses the same steps. It is how a dry run
// learns a refusal that is decided from the statement's shape alone — an
// unnamed or IF NOT EXISTS index build, a DETACH PARTITION substituted to its
// unsupported concurrent form — instead of reporting a plan the apply turns
// down.
//
// The boolean is false when every step is admitted. A non-nil error is not a
// refusal: the steps could not be parsed, or admission failed for a reason
// outside the refusal contract, exactly where [Run] would return an
// operational error. The checks that need live facts — the size guard, the
// partitioned-parent and privilege preflights — are not part of this
// function; a dry run does not claim them.
//
// An unqualified target is qualified with the schema the dry run plans
// against before admission, because [Run] resolves an unqualified table
// before its executor admits the steps.
func AdmitRoutedSteps(st statement.Statement, steps []string) (verdict.Verdict, bool, error) {
	admitted, err := qualifyForAdmission(st, steps)
	if err != nil {
		return verdict.Verdict{}, false, err
	}
	admitErr := executor.AdmitSequenceShape(admitted)
	if admitErr == nil {
		return verdict.Verdict{}, false, nil
	}
	r, ok := admissionRefusal(admitErr)
	if !ok {
		return verdict.Verdict{}, false, fmt.Errorf("admit routed steps for %s: %w", qualifiedPlanTarget(st), admitErr)
	}
	v := admissionRefusalVerdict(st, admitErr, r, false)
	v.Table = qualifiedPlanTarget(st)
	return v, true, nil
}

// qualifyForAdmission returns steps with the statement's target schema
// resolved the way the dry run plans it. A statement that already names its
// schema, or names no table, passes through unchanged.
func qualifyForAdmission(st statement.Statement, steps []string) ([]string, error) {
	if st.Schema() != "" || st.Table() == "" {
		return steps, nil
	}
	qualifiedSteps := make([]string, len(steps))
	for i, sql := range steps {
		q, err := statement.Qualify(sql, ResolvedSchema(st))
		if err != nil {
			return nil, fmt.Errorf("qualify step %d of %d for admission: %w", i+1, len(steps), err)
		}
		qualifiedSteps[i] = q
	}
	return qualifiedSteps, nil
}

// qualifiedPlanTarget renders the statement's target table with the schema
// the dry run plans against, empty when the statement has no table.
func qualifiedPlanTarget(st statement.Statement) string {
	if st.Table() == "" {
		return ""
	}
	return ResolvedSchema(st) + "." + st.Table()
}
