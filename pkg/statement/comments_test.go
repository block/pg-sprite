package statement

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckNoComments(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{"no comments", "CREATE TABLE t (id int)", nil},
		{"line comment", "-- events table\nCREATE TABLE t (id int)", ErrCommentLoss},
		{"inline line comment", "CREATE TABLE t (\n  id int -- surrogate key\n)", ErrCommentLoss},
		{"block comment", "/* header */ CREATE TABLE t (id int)", ErrCommentLoss},
		{"comment inside a string literal is not a comment", "CREATE TABLE t (id int DEFAULT length('--'))", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckNoComments(tt.sql)
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}
