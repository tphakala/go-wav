package sample

import (
	"bytes"
	"encoding/binary"
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

// diffTrailing are the numbers of extra bytes appended past the last whole
// sample of a differential source. Convert documents that a trailing partial
// sample is ignored, and every case below runs at each of these counts and must
// produce output identical to the aligned one, so a path that took its loop
// bound from the source length rather than from the destination would show up.
// The list runs to 3 so that a 32-bit source, four bytes wide, reaches its
// widest partial tail; stopping at 2 left that one width short of its own edge.
// Counts at or past the source width are skipped when the case runs, because
// there the tail is a further whole sample rather than a partial one, so a
// 24-bit source runs at 0, 1 and 2, a 16-bit one at 0 and 1, and an 8-bit one
// has no partial form at all and runs only at 0.
var diffTrailing = []int{0, 1, 2, 3}

// withTrailing returns src with n extra bytes appended past its last whole
// sample. The filler is neither 0x00 nor 0xFF so that a path which wrongly
// converted the partial sample shows up as a difference rather than coinciding
// with the silence or all-ones patterns.
func withTrailing(src []byte, n int) []byte {
	out := make([]byte, len(src)+n)
	copy(out, src)
	for i := len(src); i < len(out); i++ {
		out[i] = 0xA5
	}
	return out
}

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
// wrong stride. The trailing count is part of the message rather than only of
// the loop, because t.Helper() points every failure below at the same caller
// line, so without it a failure that only reproduces on a source with a partial
// tail would be indistinguishable from one on an aligned source.
func reportDiff(t *testing.T, what string, got, want, src []byte, srcBits, dstBits, n, trailing int) {
	t.Helper()
	i := firstDiff(got, want)
	if i < 0 {
		return
	}
	t.Fatalf("%s %d->%d, %d samples, %d trailing bytes: first difference at byte %d (sample %d): got %#v, want %#v (src sample %#v)",
		what, srcBits, dstBits, n, trailing, i, i/bytesPerSample(dstBits),
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
					for _, trailing := range diffTrailing {
						if trailing >= bytesPerSample(k.srcBits) {
							continue
						}
						src := withTrailing(diffSource(t, pattern, k.srcBits, n), trailing)
						want := make([]byte, ConvertedLen(len(src), k.srcBits, k.dstBits))
						got := make([]byte, len(want))
						convertIntToIntBlocked(want, src, k.srcBits, k.dstBits)
						k.kernel(got, src)
						if !bytes.Equal(got, want) {
							reportDiff(t, "kernel "+pattern, got, want, src, k.srcBits, k.dstBits, n, trailing)
						}
					}
				}
			}
		})
	}
}

// checkIntPairCase runs one differential case, one width pair at one pattern,
// length and trailing-byte count, through the dispatcher and through the public
// entry point, comparing both against the blocked path.
func checkIntPairCase(t *testing.T, pattern string, srcBits, dstBits, n, trailing int) {
	t.Helper()
	src := withTrailing(diffSource(t, pattern, srcBits, n), trailing)
	want := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
	convertIntToIntBlocked(want, src, srcBits, dstBits)

	got := make([]byte, len(want))
	convertIntToInt(got, src, srcBits, dstBits)
	if !bytes.Equal(got, want) {
		reportDiff(t, "dispatch "+pattern, got, want, src, srcBits, dstBits, n, trailing)
	}

	public := make([]byte, len(want))
	written, err := Convert(public, src, wav.SampleFormatPCM, srcBits, dstBits)
	if err != nil {
		t.Fatalf("Convert(%d -> %d, %d samples, %d trailing bytes): %v",
			srcBits, dstBits, n, trailing, err)
	}
	if written != len(want) {
		t.Fatalf("Convert(%d -> %d, %d samples, %d trailing bytes) wrote %d bytes, want %d",
			srcBits, dstBits, n, trailing, written, len(want))
	}
	if !bytes.Equal(public, want) {
		reportDiff(t, "Convert "+pattern, public, want, src, srcBits, dstBits, n, trailing)
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
						for _, trailing := range diffTrailing {
							if trailing >= bytesPerSample(srcBits) {
								continue
							}
							checkIntPairCase(t, pattern, srcBits, dstBits, n, trailing)
						}
					}
				}
			})
		}
	}
}

// TestKernelsPanicOnShortSource pins the guard the kernels' block comment
// documents: the reslice that sizes the source to the destination panics on a
// source whose capacity cannot cover it, rather than letting the loop read past
// the end one sample at a time. Every other test in the suite hands over a
// source that is exactly long enough, so nothing they observe changes if that
// reslice is deleted.
//
// Recovering from a panic is not by itself enough to pin it, which is the trap
// an earlier version of this test fell into. Delete the sizing reslice and
// three of the four kernels still panic, on the per-element access that runs
// off the end partway through the loop, which is the exact failure the guard
// exists to prevent; only convert8to16 changes observably, because it ranges
// over src and so quietly converts fewer samples than the destination holds.
// What separates a guarded kernel from an unguarded one is where the
// destination stands when the panic arrives. The sizing reslice runs before the
// loop, so dst is still untouched; a per-element access panics with the leading
// samples already written. The source below is therefore filled with a non-zero
// pattern and dst is checked to be still all zero after the recover, and that
// is what makes all four kernels fail if their sizing reslice is deleted.
//
// It does not pin the third index of those reslices. The upper bound of a slice
// expression is checked against capacity whether or not a capacity is given, so
// the panic below survives rewriting src[:n:n] as src[:n]. That index is there
// to stop a later reslice inside a kernel from extending back past the sizing
// one, which no kernel body currently does, and it is not observable from here.
func TestKernelsPanicOnShortSource(t *testing.T) {
	t.Parallel()
	const samples = 8
	for _, k := range kernelPairs {
		t.Run(fmt.Sprintf("%dto%d", k.srcBits, k.dstBits), func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, samples*bytesPerSample(k.dstBits))
			need := samples * bytesPerSample(k.srcBits)
			// One byte short of a whole destination's worth, and filled with a
			// pattern no kernel can turn into an all-zero destination sample,
			// so a write that happened before the panic is visible below.
			src := make([]byte, need-1)
			for i := range src {
				src[i] = 0xA5
			}
			defer func() {
				if recover() == nil {
					t.Fatalf("kernel %d->%d took a %d-byte source for %d samples without panicking, want a panic on the sizing reslice",
						k.srcBits, k.dstBits, len(src), samples)
				}
				for i, v := range dst {
					if v != 0 {
						t.Fatalf("kernel %d->%d wrote dst[%d]=%#x before panicking; the panic came from a per-element access, not from the sizing reslice",
							k.srcBits, k.dstBits, i, v)
					}
				}
			}()
			k.kernel(dst, src)
		})
	}
}

// TestConvert8to16Exhaustive turns the sampled comparison into a proof for the
// one pair whose entire source alphabet is 256 values. Every byte a source can
// hold appears once, so agreement with the blocked path here is agreement over
// the kernel's whole input domain rather than over a sample of it, and it costs
// microseconds.
func TestConvert8to16Exhaustive(t *testing.T) {
	t.Parallel()
	src := make([]byte, 1<<8)
	for i := range src {
		src[i] = byte(i)
	}
	want := make([]byte, ConvertedLen(len(src), 8, 16))
	convertIntToIntBlocked(want, src, 8, 16)
	got := make([]byte, len(want))
	convert8to16(got, src)
	if !bytes.Equal(got, want) {
		reportDiff(t, "exhaustive", got, want, src, 8, 16, len(src), 0)
	}
}

// TestConvert16to32Exhaustive does the same for the 65536 values a 16-bit
// sample can take, which is still a 128 KiB source and a millisecond of work.
// The two 24-bit pairs are left to the sampled corpus above: enumerating either
// means 16.7 million samples, affordable but slow enough to be felt on every
// run for a domain the patterns already cover at both extremes.
func TestConvert16to32Exhaustive(t *testing.T) {
	t.Parallel()
	const n = 1 << 16
	src := make([]byte, n*2)
	for i := range n {
		binary.LittleEndian.PutUint16(src[i*2:], uint16(i)) //nolint:gosec // G115: i < 1<<16 by construction.
	}
	want := make([]byte, ConvertedLen(len(src), 16, 32))
	convertIntToIntBlocked(want, src, 16, 32)
	got := make([]byte, len(want))
	convert16to32(got, src)
	if !bytes.Equal(got, want) {
		reportDiff(t, "exhaustive", got, want, src, 16, 32, n, 0)
	}
}
