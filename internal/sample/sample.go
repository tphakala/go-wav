package sample

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	wav "github.com/tphakala/go-wav"
)

// errPrefix is the context prefix on every error this package raises, matching
// the "go-wav/<package>: " convention used across the library.
const errPrefix = "go-wav/internal/sample: "

// Validate reports whether a (format, bitDepth) pair is one this library
// supports. Integer PCM is supported at 8, 16, 24 and 32 bits; float is
// supported at 32 and 64 bits. Anything else, including an unknown format
// value, returns an error wrapping [wav.ErrUnsupported].
func Validate(format wav.SampleFormat, bitDepth int) error {
	switch format {
	case wav.SampleFormatPCM:
		switch bitDepth {
		case 8, 16, 24, 32:
			return nil
		}
		return fmt.Errorf("%sinteger pcm bit depth %d: %w", errPrefix, bitDepth, wav.ErrUnsupported)
	case wav.SampleFormatFloat:
		switch bitDepth {
		case 32, 64:
			return nil
		}
		return fmt.Errorf("%sfloat bit depth %d: %w", errPrefix, bitDepth, wav.ErrUnsupported)
	default:
		return fmt.Errorf("%ssample format %d: %w", errPrefix, int(format), wav.ErrUnsupported)
	}
}

// ConvertedLen returns the number of destination bytes [Convert] needs for a
// source of srcLen bytes going from srcBits to dstBits. A trailing partial
// sample in the source contributes nothing, because Convert ignores it. The
// result is 0 for a negative length or for a bit depth that is not a whole
// number of bytes wide, which are exactly the cases Convert rejects.
func ConvertedLen(srcLen, srcBits, dstBits int) int {
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	if srcLen <= 0 || srcWidth <= 0 || dstWidth <= 0 {
		return 0
	}
	return (srcLen / srcWidth) * dstWidth
}

// Convert rewrites src, encoded as srcFormat at srcBits, into dst as signed
// little-endian integer PCM at dstBits, and returns the number of bytes
// written. dst must be at least [ConvertedLen] bytes long; Convert never
// allocates and never grows dst, reporting a short destination as an error
// wrapping [io.ErrShortBuffer] instead.
//
// A src whose length is not a whole number of samples is not an error: the
// whole samples are converted and the trailing partial sample is ignored, so
// the returned count reflects only what was actually written. A caller
// streaming arbitrary chunk boundaries can therefore hand over whatever it has
// without pre-checking alignment.
//
// dstBits must be 8, 16, 24 or 32; converting to float output is not
// supported. When srcFormat is [wav.SampleFormatPCM] and srcBits equals
// dstBits the call degenerates to a copy.
//
// See the package documentation for the shift, clamp and rounding rules.
func Convert(dst, src []byte, srcFormat wav.SampleFormat, srcBits, dstBits int) (int, error) {
	if err := Validate(srcFormat, srcBits); err != nil {
		return 0, err
	}
	// The destination is always integer PCM, so it is validated against that
	// format rather than against srcFormat.
	if err := Validate(wav.SampleFormatPCM, dstBits); err != nil {
		return 0, err
	}

	need := ConvertedLen(len(src), srcBits, dstBits)
	if len(dst) < need {
		return 0, fmt.Errorf("%sdestination holds %d bytes, need %d: %w",
			errPrefix, len(dst), need, io.ErrShortBuffer)
	}
	if need == 0 {
		return 0, nil
	}

	// Reslicing both sides to their exact extent lets the loops below drive off
	// dst alone and keeps the indexed accesses provably in range.
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	// Capped as well as sliced: the block helpers reslice these to the extent
	// they expect, and a slice expression is bounded by capacity, so without
	// the cap a mistake there would silently read or write past the length
	// these lines establish instead of panicking.
	src = src[: (need/dstWidth)*srcWidth : (need/dstWidth)*srcWidth]
	dst = dst[:need:need]

	if srcFormat == wav.SampleFormatPCM {
		if srcBits == dstBits {
			return copy(dst, src), nil
		}
		convertIntToInt(dst, src, srcBits, dstBits)
		return need, nil
	}
	convertFloatToInt(dst, src, srcBits, dstBits)
	return need, nil
}

// convertIntToInt requantises signed integer PCM by shifting. Widening shifts
// left, which is exact; narrowing shifts right arithmetically, which truncates
// toward negative infinity and drops the low bits without rounding or dither.
// dst and src must already be sized to a whole number of matching samples.
func convertIntToInt(dst, src []byte, srcBits, dstBits int) {
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	shift := dstBits - srcBits

	// The work is blocked through a small stack buffer so that the width
	// switches can be hoisted out of the per-sample loops. Decoding and
	// encoding each become one specialised loop per width, chosen once per
	// block rather than once per sample, which is worth about twice the
	// throughput; the alternative, a specialised loop for all sixteen width
	// pairs, is the same idea written four times over.
	//
	// int32 holds every value this can produce. The widest shift is 8 bits to
	// 32, where -128<<24 lands exactly on MinInt32: that is the binding case,
	// with no headroom, and it fits.
	var tmp [blockSamples]int32

	total := len(dst) / dstWidth
	for done := 0; done < total; {
		n := min(blockSamples, total-done)
		decodeBlock(tmp[:n], src[done*srcWidth:], srcBits)
		switch {
		case shift > 0:
			for i := range tmp[:n] {
				tmp[i] <<= uint(shift)
			}
		case shift < 0:
			for i := range tmp[:n] {
				tmp[i] >>= uint(-shift)
			}
		}
		encodeBlock(dst[done*dstWidth:], tmp[:n], dstBits)
		done += n
	}
}

// blockSamples is how many samples the conversion loops stage at a time.
//
// The buffer puts these frames at roughly 4.2 KiB, over a goroutine's 2 KiB
// starting stack, so the first conversion on a fresh goroutine pays one stack
// growth. Shrinking the block to fit under that stack was tried and dropped:
// interleaved measurement put the difference at a couple of percent in favour
// of the larger block, not significant per benchmark, and fitting would have
// meant going down to about 160 samples rather than the 256 that looks like it
// fits. Go also grows a goroutine's starting stack adaptively, so a process
// converting repeatedly stops paying the growth at all.
//
// The size is therefore chosen for the block loops themselves rather than for
// the stack: large enough to amortise the per-block width switch, small enough
// to stay in L1.
const blockSamples = 1024

// decodeBlock reads len(out) samples of the given width into out as signed
// values.
// The width is switched on once here rather than once per sample.
func decodeBlock(out []int32, src []byte, bits int) {
	switch bits {
	case 8:
		src = src[:len(out)]
		for i := range out {
			// Stored biased by 128, so the bias comes off here.
			out[i] = int32(src[i]) - 128
		}
	case 16:
		src = src[:len(out)*2]
		for i := range out {
			out[i] = int32(int16(binary.LittleEndian.Uint16(src[i*2:])))
		}
	case 24:
		src = src[:len(out)*3]
		for i := range out {
			b := src[i*3:][:3]
			// Highest index first, as in encodeBlock, so the compiler drops
			// the per-element checks on the other two.
			u := uint32(b[2])<<16 | uint32(b[0]) | uint32(b[1])<<8
			if u&0x800000 != 0 {
				u |= 0xFF000000 // sign-extend bit 23 into the unused high byte
			}
			out[i] = int32(u)
		}
	default: // 32
		src = src[:len(out)*4]
		for i := range out {
			out[i] = int32(binary.LittleEndian.Uint32(src[i*4:]))
		}
	}
}

// encodeBlock writes the samples of in as little-endian integer PCM of the
// given width. Every value is already in range for bits, because the caller
// either shifted into range or clamped.
func encodeBlock(dst []byte, in []int32, bits int) {
	switch bits {
	case 8:
		dst = dst[:len(in)]
		for i, v := range in {
			dst[i] = byte(v + 128) // the 128 bias goes back on
		}
	case 16:
		dst = dst[:len(in)*2]
		for i, v := range in {
			binary.LittleEndian.PutUint16(dst[i*2:], uint16(v)) //nolint:gosec // G115: in int16 range by construction.
		}
	case 24:
		dst = dst[:len(in)*3]
		for i, v := range in {
			u := uint32(v) //nolint:gosec // G115: in 24-bit signed range by construction.
			b := dst[i*3:]
			b[2] = byte(u >> 16) // highest index first, so the rest need no check
			b[0] = byte(u)
			b[1] = byte(u >> 8)
		}
	default: // 32
		dst = dst[:len(in)*4]
		for i, v := range in {
			binary.LittleEndian.PutUint32(dst[i*4:], uint32(v)) //nolint:gosec // G115: in int32 range by construction.
		}
	}
}

// convertFloatToInt quantises IEEE 754 samples to signed integer PCM. dst and
// src must already be sized to a whole number of matching samples.
func convertFloatToInt(dst, src []byte, srcBits, dstBits int) {
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	// int64 keeps 1<<31 well clear of the 32-bit int range, so dstBits == 32 is
	// safe on a 32-bit host. The positive limit is one below full scale because
	// +1.0 scales to exactly full scale, which the signed range cannot hold.
	fullScale := float64(int64(1) << uint(dstBits-1))
	posLimit := fullScale - 1
	negLimit := -fullScale

	// Blocked like the integer path, so the source width and the destination
	// width are each switched on once per block rather than once per sample.
	var tmp [blockSamples]int32

	total := len(dst) / dstWidth
	for done := 0; done < total; {
		n := min(blockSamples, total-done)
		block := src[done*srcWidth:]
		if srcBits == 64 {
			block = block[:n*8]
			for i := range tmp[:n] {
				f := math.Float64frombits(binary.LittleEndian.Uint64(block[i*8:]))
				//nolint:gosec // G115: quantize clamps into int32 range for every dstBits it is called with here.
				tmp[i] = int32(quantize(f, fullScale, posLimit, negLimit))
			}
		} else {
			block = block[:n*4]
			for i := range tmp[:n] {
				f := float64(math.Float32frombits(binary.LittleEndian.Uint32(block[i*4:])))
				//nolint:gosec // G115: as above.
				tmp[i] = int32(quantize(f, fullScale, posLimit, negLimit))
			}
		}
		encodeBlock(dst[done*dstWidth:], tmp[:n], dstBits)
		done += n
	}
}

// quantize scales one float sample to an integer sample.
//
// The clamp absorbs both infinities and every finite value past full scale,
// which real-world float WAV files carry routinely. NaN becomes 0 so a broken
// sample cannot poison the output; it is tested after the clamp rather than
// before it, which is equivalent because NaN compares false against both
// limits, and keeps it off the common path.
//
// Rounding is half away from zero, matching math.Round, but hand-rolled rather
// than calling it. The equivalence is exact and pinned by test; see the
// sub-half arm below for the one place the two would otherwise differ.
func quantize(f, fullScale, posLimit, negLimit float64) int64 {
	v := f * fullScale
	switch {
	case v >= posLimit:
		// +Inf >= posLimit is true, so this arm covers positive overflow of
		// every magnitude without a separate IsInf test.
		return int64(posLimit)
	case v <= negLimit:
		return int64(negLimit)
	case v != v:
		// NaN, which compares false against both limits above, so it lands
		// here. Testing it last keeps it off the common path.
		return 0
	case v > -0.5 && v < 0.5:
		// Everything inside half an LSB rounds to zero, and taking it here
		// keeps it away from the addition below.
		//
		// That is not just a shortcut. Adding a half to a value smaller than
		// a half carries it into the next binade, where the significand is
		// one bit coarser, so the sum is NOT exact: the largest float64 below
		// 0.5 plus 0.5 rounds to exactly 1.0, which would quantise to 1 where
		// math.Round gives 0. Those two values, one per sign, are the only
		// inputs where the arithmetic below would disagree with math.Round,
		// and this arm removes them.
		return 0
	case v >= 0:
		// Round half away from zero, matching math.Round. Above half an LSB
		// the addition cannot change binade, so it is exact and the
		// conversion's truncation toward zero finishes the job. Doing this
		// inline rather than calling math.Round matters: Round was 14 percent
		// of the float conversion path, and its cost also pushed this
		// function past the inlining budget.
		return int64(v + 0.5)
	default:
		return int64(v - 0.5)
	}
}

// bytesPerSample is the storage width in bytes of one sample of the given bit
// depth. It returns 0 for a depth this package does not store, so callers can
// use a zero result as a rejection.
func bytesPerSample(bits int) int {
	switch bits {
	case 8:
		return 1
	case 16:
		return 2
	case 24:
		return 3
	case 32:
		return 4
	case 64:
		return 8
	default:
		return 0
	}
}
