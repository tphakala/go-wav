package pcm

import (
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/sample"
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

// TestUnrepresentableSizeErrorWrapsNothing pins the sentinel choice this
// package made for a conversion whose result cannot be expressed as a length.
//
// internal/sample refuses the same condition with an error wrapping
// io.ErrShortBuffer, on the grounds that it holds a dst that could in
// principle have been longer. DecodeInterleaved holds no destination, so there
// is nothing for a caller to grow and nothing to retry, and it deliberately
// wraps nothing instead. That asymmetry looks like an oversight to anyone
// reading one site without the other, which is exactly how it would get
// "fixed" into agreement.
//
// The branch cannot be reached from a decode on a 64-bit machine, since it
// needs a source larger than one can allocate, so the error is built directly.
func TestUnrepresentableSizeErrorWrapsNothing(t *testing.T) {
	t.Parallel()

	err := errUnrepresentableSize(1<<30, 8, 32)
	if err == nil {
		t.Fatal("errUnrepresentableSize returned nil")
	}

	// The sentinel its sibling in internal/sample uses. Matching it here would
	// tell a caller to grow a buffer this call does not take.
	if errors.Is(err, io.ErrShortBuffer) {
		t.Errorf("error wraps io.ErrShortBuffer: %v", err)
	}
	// Not this one either: ErrTooLarge means the 4 GiB RIFF limit with RF64
	// unavailable, which a caller answers by changing the container. Nothing
	// about the container helps here.
	if errors.Is(err, wav.ErrTooLarge) {
		t.Errorf("error wraps wav.ErrTooLarge, whose remedy does not apply: %v", err)
	}
	if errors.Unwrap(err) != nil {
		t.Errorf("error wraps %v, want nothing", errors.Unwrap(err))
	}

	// Wrapping nothing puts the whole burden on the message, so it has to name
	// the call a caller would look for and say the size is unaddressable
	// rather than merely too large.
	for _, want := range []string{"DecodeInterleaved", "than this platform can address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q does not contain %q", err.Error(), want)
		}
	}
}

// TestConvertedBytesFitAgreesWithConvertedLen pins the precedence between this
// package's guard and the one inside the conversion.
//
// DecodeInterleaved's own check is meant to answer first, so that the refusal
// a caller sees names the exported call rather than arriving from inside
// internal/sample with a different sentinel. That holds only while the two
// apply the same ceiling: if this guard were ever loosened relative to
// sample.ConvertedLen, a source would slip past it and be refused by the inner
// one instead, silently changing which error a caller gets and whether
// errors.Is(err, io.ErrShortBuffer) matches.
//
// Stated in lengths, because the sources involved cannot be allocated.
func TestConvertedBytesFitAgreesWithConvertedLen(t *testing.T) {
	t.Parallel()

	// Every widening pair the package supports, plus the boundary either side
	// of the ceiling for each.
	for _, bits := range []struct{ src, dst int }{
		{8, 16}, {8, 24}, {8, 32}, {16, 24}, {16, 32}, {24, 32},
	} {
		srcWidth := (bits.src + 7) / 8
		dstWidth := (bits.dst + 7) / 8
		maxSamples := math.MaxInt / dstWidth

		for _, samples := range []int{1, maxSamples - 1, maxSamples, maxSamples + 1} {
			if samples <= 0 || samples > math.MaxInt/srcWidth {
				continue
			}
			srcLen := samples * srcWidth

			outer := convertedBytesFit(samples, dstWidth, math.MaxInt)
			inner := sample.ConvertedLen(srcLen, bits.src, bits.dst) != 0
			if outer != inner {
				t.Errorf("%d bit to %d bit, %d samples: convertedBytesFit=%v but ConvertedLen reports representable=%v; "+
					"the two ceilings have drifted, so the refusal a caller sees would change",
					bits.src, bits.dst, samples, outer, inner)
			}
		}
	}
}
