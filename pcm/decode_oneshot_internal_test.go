package pcm

import (
	"math"
	"testing"
)

// TestConvertedBytesFit covers the arithmetic that keeps a widening one-shot
// conversion from asking for a length that cannot be expressed. The boundary is
// what matters: one sample either side of the limit must be answered
// differently, and the division must not round a case that does not fit into
// one that does.
func TestConvertedBytesFit(t *testing.T) {
	cases := []struct {
		name    string
		samples int
		width   int
		limit   int
		want    bool
	}{
		{"nothing to convert", 0, 4, 1000, true},
		{"well inside the limit", 10, 4, 1000, true},
		{"exactly the limit", 250, 4, 1000, true},
		{"one sample past the limit", 251, 4, 1000, false},
		{"a limit that does not divide evenly", 250, 3, 751, true},
		{"one sample past a limit that does not divide evenly", 251, 3, 751, false},
		{"a whole platform word of samples", math.MaxInt, 2, math.MaxInt, false},
		{"one byte wide never overflows", math.MaxInt, 1, math.MaxInt, true},
		{"a width that is not positive is not this check's business", 10, 0, 1000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := convertedBytesFit(tc.samples, tc.width, tc.limit); got != tc.want {
				t.Errorf("convertedBytesFit(%d, %d, %d) = %v, want %v",
					tc.samples, tc.width, tc.limit, got, tc.want)
			}
		})
	}
}
