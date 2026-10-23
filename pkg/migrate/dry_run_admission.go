package migrate

import (
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// errAdmissionSchemaUnknown is returned when an unqualified statement is
// admitted without the schema its table resolved to.
var errAdmissionSchemaUnknown = errors.New("an unqualified statement needs the schema its table resolved to")

// AdmitRoutedSteps runs the sequence executor's static admission over the
// steps a dry run would execute for st, and returns the refusal verdict [Run]
// ends in when that admission refuses the same steps. It is how a dry run
// learns a refusal that is decided from the statement's shape alone — an
// unnamed or IF NOT EXISTS index build, a DETACH PARTITION substituted to its
// unsupported concurrent form — instead of reporting a plan the apply turns
// down. The verdict is the apply's only when admission is the apply's first
// refusal: the checks that need live facts — the size guard, the
// partitioned-parent and privilege preflights — run before admission in
// [Run] and are not part of this function, so a dry run does not claim them.
//
// The boolean is false when every step is admitted. A non-nil error is not a
// refusal: the steps could not be parsed, or admission failed for a reason
// outside the refusal contract, exactly where [Run] would return an
// operational error.
//
// schema is the schema st's table resolved to on the session's search_path,
// as the dry run's live facts read it. [Run] resolves an unqualified table
// before its executor admits the steps, so an unqualified st has its steps
// qualified with schema before admission, and its refusal names the table
// and statement the way [Run]'s does: the table in schema, and the statement
// re-emitted qualified. A qualified st ignores schema.
func AdmitRoutedSteps(st statement.Statement, schema string, steps []string) (verdict.Verdict, bool, error) {
	unqualified := st.Schema() == "" && st.Table() != ""
	if unqualified && schema == "" {
		return verdict.Verdict{}, false, fmt.Errorf("admit routed steps for %s: %w", st.Table(), errAdmissionSchemaUnknown)
	}
	admitted := steps
	if unqualified {
		var err error
		if admitted, err = qualifySteps(steps, schema); err != nil {
			return verdict.Verdict{}, false, err
		}
	}
	admitErr := executor.AdmitSequenceShape(admitted)
	if admitErr == nil {
		return verdict.Verdict{}, false, nil
	}
	r, ok := admissionRefusal(admitErr)
	if !ok {
		return verdict.Verdict{}, false, fmt.Errorf("admit routed steps for %s: %w", qualified(st), admitErr)
	}
	target := st
	if unqualified {
		resolved, err := resolveForVerdict(st, schema)
		if err != nil {
			return verdict.Verdict{}, false, err
		}
		target = resolved
	}
	return admissionRefusalVerdict(target, admitErr, r, false), true, nil
}

// qualifySteps returns steps with their table in schema.
func qualifySteps(steps []string, schema string) ([]string, error) {
	qualifiedSteps := make([]string, len(steps))
	for i, sql := range steps {
		q, err := statement.Qualify(sql, schema)
		if err != nil {
			return nil, fmt.Errorf("qualify step %d of %d for admission: %w", i+1, len(steps), err)
		}
		qualifiedSteps[i] = q
	}
	return qualifiedSteps, nil
}

// resolveForVerdict qualifies an unqualified st with schema the way [Run]'s
// target resolution does before admission. Like that resolution it refuses
// commented input, which the re-emitted statement would silently drop, so
// the dry run ends in the error [Run] returns rather than a refusal it never
// reaches.
func resolveForVerdict(st statement.Statement, schema string) (statement.Statement, error) {
	if err := statement.CheckNoComments(st.SQL()); err != nil {
		return statement.Statement{}, err
	}
	return qualifyStatement(st, schema)
}
