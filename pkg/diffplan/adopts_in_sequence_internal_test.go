package diffplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A new key's adoption belongs only to a sequence whose step attaches the
// key's own index. A key the planner runs as one ADD CONSTRAINT, which builds
// its index under the table's lock, takes over nothing, and neither does an
// attach of another index.
func TestAdoptsInSequence(t *testing.T) {
	for name, tt := range map[string]struct {
		execSQL []string
		want    bool
	}{
		"a build, then the attach of the key's index": {
			execSQL: []string{
				`CREATE UNIQUE INDEX CONCURRENTLY "events_name_key" ON "public"."events" ("name")`,
				`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key"`,
			},
			want: true,
		},
		"one ADD CONSTRAINT that builds its own index": {
			execSQL: []string{`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE ("name")`},
		},
		"the attach of another index": {
			execSQL: []string{`ALTER TABLE "public"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_idx"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := adoptsInSequence(tt.execSQL, "events_name_key")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
