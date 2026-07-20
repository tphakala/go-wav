package sample

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// intDepths are the integer PCM depths Convert accepts on either side.
var intDepths = [...]int{8, 16, 24, 32}

// kernelPairs pins which width pairs are expected to have a single-pass kernel
// and which function each pair must reach. Naming the functions here rather
// than only going through the dispatcher means a kernel that is never wired up
// still gets compared against the blocked path.
var kernelPairs = []struct {
	srcBits, dstBits int
	kernel           func(dst, src []byte)
}{
	{8, 16, convert8to16},
	{16, 32, convert16to32},
	{24, 32, convert24to32},
	{24, 16, convert24to16},
}

// diffLengths are the sample counts every differential case runs at, in
// samples rather than bytes. They straddle the staging block of the blocked
// path, because a kernel that gets its loop bound or its final partial block
// wrong can still agree on a whole number of blocks.
var diffLengths = []int{0, 1, 2, 3, 7, 63, blockSamples - 1, blockSamples, blockSamples + 1, 2*blockSamples + 5, 4099}

// The byte patterns each differential case runs. Random bytes cover the
// ordinary sample, and the three fixed patterns pin the corners that random
// data reaches only by accident: silence, all bits set, and the extremes of
// each width.
const (
	patternRandom   = "random"
	patternZero     = "zero"
	patternOnes     = "ones"
	patternExtremes = "extremes"
)

var diffPatterns = []string{patternRandom, patternZero, patternOnes, patternExtremes}

// extremeSamples returns the boundary sample values of a depth already in
// stored byte form. The 8-bit row is written as stored bytes rather than signed
// values because that depth carries the 128 bias on disk, so 0x80 is silence
// and 0x00 is the negative extreme.
func extremeSamples(bits int) []byte {
	switch bits {
	case 8:
		return pcm8(0x00, 0x80, 0xFF, 0x7F, 0x81, 0x01)
	case 16:
		return pcm16(math.MinInt16, math.MaxInt16, -1, 0, 1, math.MinInt16+1)
	case 24:
		return pcm24(-8388608, 8388607, -1, 0, 1, -8388607)
	default:
		return pcm32(math.MinInt32, math.MaxInt32, -1, 0, 1, math.MinInt32+1)
	}
}

// diffSource builds n samples of the given depth in one of the patterns above.
// The random pattern is seeded from the depth alone, so a failure reproduces on
// the next run instead of moving.
func diffSource(t *testing.T, pattern string, bits, n int) []byte {
	t.Helper()
	w := bytesPerSample(bits)
	if w == 0 {
		t.Fatalf("diffSource: unsupported bit depth %d", bits)
	}
	b := make([]byte, n*w)
	switch pattern {
	case patternZero:
		// A fresh slice is already silence at 16, 24 and 32 bits, and the
		// negative extreme at 8, which is worth covering either way.
	case patternOnes:
		for i := range b {
			b[i] = 0xFF
		}
	case patternExtremes:
		e := extremeSamples(bits)
		for i := 0; i < len(b); i += len(e) {
			copy(b[i:], e)
		}
	case patternRandom:
		rng := rand.New(rand.NewPCG(0x2545F4914F6CDD1D, uint64(bits))) //nolint:gosec // G404: reproducible test input, not a security context.
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
	default:
		t.Fatalf("diffSource: unknown pattern %q", pattern)
	}
	return b
}

// firstDiff returns the index of the first byte at which two outputs disagree,
// or -1 when they are identical.
func firstDiff(got, want []byte) int {
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			return i
		}
	}
	if len(got) > len(want) {
		return len(want)
	}
	return -1
}

// reportDiff fails the test naming the byte and the sample where two outputs
// first disagree, which is the information needed to tell a wrong shift from a
// wrong stride.
func reportDiff(t *testing.T, what string, got, want, src []byte, srcBits, dstBits, n int) {
	t.Helper()
	i := firstDiff(got, want)
	if i < 0 {
		return
	}
	t.Fatalf("%s %d->%d, %d samples: first difference at byte %d (sample %d): got %#v, want %#v (src sample %#v)",
		what, srcBits, dstBits, n, i, i/bytesPerSample(dstBits),
		got[i:min(i+8, len(got))], want[i:min(i+8, len(want))],
		src[(i/bytesPerSample(dstBits))*bytesPerSample(srcBits):min((i/bytesPerSample(dstBits)+2)*bytesPerSample(srcBits), len(src))])
}

// TestSinglePassKernelsMatchBlocked runs each specialised kernel directly
// against the blocked path, which is the only independent statement of the
// shift rules inside this package. Four hand-written kernels are four chances
// to get a width or a stride wrong, and only a comparison against code that
// does not share their arithmetic can catch that.
func TestSinglePassKernelsMatchBlocked(t *testing.T) {
	t.Parallel()
	for _, k := range kernelPairs {
		t.Run(fmt.Sprintf("%dto%d", k.srcBits, k.dstBits), func(t *testing.T) {
			t.Parallel()
			for _, pattern := range diffPatterns {
				for _, n := range diffLengths {
					src := diffSource(t, pattern, k.srcBits, n)
					want := make([]byte, ConvertedLen(len(src), k.srcBits, k.dstBits))
					got := make([]byte, len(want))
					convertIntToIntBlocked(want, src, k.srcBits, k.dstBits)
					k.kernel(got, src)
					if !bytes.Equal(got, want) {
						reportDiff(t, "kernel "+pattern, got, want, src, k.srcBits, k.dstBits, n)
					}
				}
			}
		})
	}
}

// TestConvertIntToIntMatchesBlocked runs every width pair, kernel-backed or
// not, through the dispatcher and through the public entry point. Testing the
// kernels alone would not catch a pair routed to the wrong one, and the public
// path additionally covers the slicing Convert does before dispatching.
func TestConvertIntToIntMatchesBlocked(t *testing.T) {
	t.Parallel()
	for _, srcBits := range intDepths {
		for _, dstBits := range intDepths {
			t.Run(fmt.Sprintf("%dto%d", srcBits, dstBits), func(t *testing.T) {
				t.Parallel()
				for _, pattern := range diffPatterns {
					for _, n := range diffLengths {
						src := diffSource(t, pattern, srcBits, n)
						want := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
						convertIntToIntBlocked(want, src, srcBits, dstBits)

						got := make([]byte, len(want))
						convertIntToInt(got, src, srcBits, dstBits)
						if !bytes.Equal(got, want) {
							reportDiff(t, "dispatch "+pattern, got, want, src, srcBits, dstBits, n)
						}

						public := make([]byte, len(want))
						written, err := Convert(public, src, wav.SampleFormatPCM, srcBits, dstBits)
						if err != nil {
							t.Fatalf("Convert(%d -> %d, %d samples): %v", srcBits, dstBits, n, err)
						}
						if written != len(want) {
							t.Fatalf("Convert(%d -> %d, %d samples) wrote %d bytes, want %d",
								srcBits, dstBits, n, written, len(want))
						}
						if !bytes.Equal(public, want) {
							reportDiff(t, "Convert "+pattern, public, want, src, srcBits, dstBits, n)
						}
					}
				}
			})
		}
	}
}
