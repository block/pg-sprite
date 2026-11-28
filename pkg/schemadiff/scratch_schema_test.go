package schemadiff

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every scratch schema name starts with ScratchSchemaPrefix, the prefix a DDL
// guard on the target exempts, fits PostgreSQL's 63-byte identifier limit,
// and differs between builds.
func TestScratchSchemaNameStartsWithTheExportedPrefix(t *testing.T) {
	first, err := scratchSchemaName()
	require.NoError(t, err)
	second, err := scratchSchemaName()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(first, ScratchSchemaPrefix), first)
	assert.LessOrEqual(t, len(first), 63)
	assert.NotEqual(t, first, second)
}
