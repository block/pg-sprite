package planner

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// ScriptClassifier classifies the statements of one script in order, with
// zero live facts except the constraint names the script's own earlier
// statements leave behind. Running the script, the server numbers an
// unnamed CHECK or FOREIGN KEY past a name an earlier statement claimed, so
// a later safer sequence must state the numbered name too, or it would
// collide with the constraint the script just added. Names are tracked per
// schema as the statement writes it, an unqualified table under its own
// key. The zero value is ready to use.
type ScriptClassifier struct {
	names map[string]map[string]bool
}

// Classify parses and routes sql, the script's next statement, as
// [Classify] does, then records the constraint names it leaves: each
// constraint an ALTER TABLE adds, by its stated name or the name its NOT
// VALID sequence states, less each it drops. Any other constraint added
// unnamed is not recorded, since the planner states no name for it.
func (c *ScriptClassifier) Classify(sql string) (Plan, error) {
	st, err := statement.ParseOne(sql)
	if err != nil {
		return Plan{}, err
	}
	plan, err := Classify(sql, Facts{ConstraintNames: c.names[st.Schema()]})
	if err != nil {
		return Plan{}, err
	}
	if st.Kind() != statement.KindAlterTable {
		return plan, nil
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return Plan{}, err
	}
	if len(ops) != len(plan.Decisions) {
		return Plan{}, fmt.Errorf("planner produced %d decisions for %d operations", len(plan.Decisions), len(ops))
	}
	for i, op := range ops {
		switch op.Kind {
		case statement.OpAddConstraint:
			name, err := addedConstraintName(op, plan.Decisions[i])
			if err != nil {
				return Plan{}, err
			}
			if name != "" {
				c.claim(st.Schema(), name)
			}
		case statement.OpDropConstraint:
			delete(c.names[st.Schema()], op.Name)
		}
	}
	return plan, nil
}

// addedConstraintName is the name op's constraint gets: the one it states,
// else, for a CHECK or FOREIGN KEY, the one its NOT VALID sequence states,
// else "" when neither does.
func addedConstraintName(op statement.Op, d Decision) (string, error) {
	if op.Name != "" || !validatedOnAdd(op) || len(d.SaferSQL) == 0 {
		return op.Name, nil
	}
	ops, err := statement.ParseOps(d.SaferSQL[0])
	if err != nil {
		return "", fmt.Errorf("read the constraint the safer sequence adds: %w", err)
	}
	if len(ops) != 1 || ops[0].Kind != statement.OpAddConstraint {
		return "", fmt.Errorf("safer sequence for %s does not start with one ADD CONSTRAINT: %s", op.Describe(), d.SaferSQL[0])
	}
	return ops[0].Name, nil
}

// claim records name as taken in schema.
func (c *ScriptClassifier) claim(schema, name string) {
	if c.names == nil {
		c.names = map[string]map[string]bool{}
	}
	if c.names[schema] == nil {
		c.names[schema] = map[string]bool{}
	}
	c.names[schema][name] = true
}
