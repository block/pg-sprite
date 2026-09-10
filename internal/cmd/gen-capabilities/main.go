// Command gen-capabilities renders the checked-in support matrix from embedded YAML.
package main

import (
	"fmt"
	"os"

	"github.com/block/pg-sprite/pkg/capabilities"
)

func main() {
	const path = "docs/capabilities.md"
	rows, err := capabilities.Rows()
	if err != nil {
		fail(err)
	}
	input, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	output, err := capabilities.RenderDocument(input, rows)
	if err != nil {
		fail(err)
	}
	if err = os.WriteFile(path, output, 0o644); err != nil {
		fail(err)
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
