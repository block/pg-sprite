package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestByteSizeUnmarshal(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"512B", 512},
		{"4KiB", 4 << 10},
		{"4kb", 4 << 10},
		{"100MiB", 100 << 20},
		{"100MB", 100 << 20},
		{"1GiB", 1 << 30},
		{"1gb", 1 << 30},
		{"2TiB", 2 << 40},
		{" 8 MiB ", 8 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var b byteSize
			require.NoError(t, b.UnmarshalText([]byte(tt.in)))
			assert.Equal(t, byteSize(tt.want), b)
		})
	}
}

func TestByteSizeUnmarshalRejectsInvalid(t *testing.T) {
	// The last two would overflow int64 bytes after unit multiplication.
	for _, in := range []string{"", "GiB", "1XB", "-5MiB", "0", "1.5GiB", "9999999999GiB", "9223372036854775807KiB"} {
		t.Run(in, func(t *testing.T) {
			var b byteSize
			assert.Error(t, b.UnmarshalText([]byte(in)))
		})
	}
}
