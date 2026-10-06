package testutil

import "math"

// UnlimitedDisk is the free-disk figure a test hands to the copy-and-swap
// environment check when the volume is not measured: more free space than
// any fixture table needs, so the proof under test is the shape, the
// privileges, or the decoding facts rather than disk headroom. It is a test
// stand-in only; a run against a real volume measures the figure.
const UnlimitedDisk int64 = math.MaxInt64
