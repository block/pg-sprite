package statement_test

import (
	"errors"
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// ExampleParseDesiredFile routes a desired-state schema file to the planner
// that owns it. Exactly one of RowSecurity and Schema reports ok after a nil
// error; hand that value to diffplan.PlanWithRowSecurity or diffplan.Plan.
func ExampleParseDesiredFile() {
	for _, sql := range []string{
		"CREATE TABLE documents (id bigint PRIMARY KEY);\n" +
			"CREATE INDEX documents_id_idx ON documents (id);",
		"CREATE TABLE documents (id bigint PRIMARY KEY);\n" +
			"ALTER TABLE documents ENABLE ROW LEVEL SECURITY;",
		"CREATE TABLE documents (id bigint PRIMARY KEY);\n" +
			"ALTER TABLE documents ADD COLUMN title text;",
		"CREATE TABLE documents (",
	} {
		file, err := statement.ParseDesiredFile(sql)
		switch {
		case errors.Is(err, statement.ErrParseDesired):
			fmt.Println("not valid SQL")
			continue
		case errors.Is(err, statement.ErrDisallowedStatement):
			fmt.Println("refused:", statement.ErrDisallowedStatement)
			continue
		case err != nil:
			fmt.Println("refused:", err)
			continue
		}
		if _, ok := file.RowSecurity(); ok {
			fmt.Printf("row-security planner: %s, %d statements\n", file.Table(), len(file.Statements()))
			continue
		}
		if _, ok := file.Schema(); ok {
			fmt.Printf("ordinary planner: %s, %d statements\n", file.Table(), len(file.Statements()))
		}
	}
	// Output:
	// ordinary planner: documents, 2 statements
	// row-security planner: documents, 2 statements
	// refused: statement kind not allowed in a desired schema
	// not valid SQL
}

// ExampleFormatDesired formats a desired-state schema file the way the fmt
// command does. A file is printed in canonical form; a commented one is
// refused, since the parser would drop the comment; a syntax error is told
// from a refusal by ErrParseDesired. On any error the text is empty.
func ExampleFormatDesired() {
	for _, sql := range []string{
		"create table documents (id bigint primary key);\n" +
			"create index documents_id_idx on documents (id);",
		"CREATE TABLE documents (id bigint PRIMARY KEY); -- keep me",
		"CREATE TABLE documents (title text DEFAULT 'oops);",
		"CREATE TABLE documents (id bigint);\nALTER TABLE documents ADD COLUMN title text;",
	} {
		formatted, err := statement.FormatDesired(sql)
		switch {
		case errors.Is(err, statement.ErrCommentLoss):
			fmt.Println("refused: commented input")
		case errors.Is(err, statement.ErrParseDesired):
			fmt.Println("not valid SQL")
		case errors.Is(err, statement.ErrDisallowedStatement):
			fmt.Println("refused:", statement.ErrDisallowedStatement)
		case err != nil:
			fmt.Println("refused:", err)
		default:
			fmt.Print(formatted)
		}
	}
	// Output:
	// CREATE TABLE documents (id bigint PRIMARY KEY);
	// CREATE INDEX documents_id_idx ON documents USING btree (id);
	// refused: commented input
	// not valid SQL
	// refused: statement kind not allowed in a desired schema
}
