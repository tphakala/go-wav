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
// supported at 32 and 64 bits; the two G.711 companding laws are supported at
// the 8 bits they are defined for and at no other width. Anything else,
// including an unknown format value, returns an error wrapping
// [wav.ErrUnsupported].
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
	case wav.SampleFormatALaw, wav.SampleFormatMuLaw:
		// G.711 defines one code width and no other, so a stream declaring
		// another one describes nothing that exists and is refused rather
		// than read as though the depth field were a typo.
		if bitDepth == 8 {
			return nil
		}
		return fmt.Errorf("%s%s bit depth %d (want 8): %w", errPrefix, format, bitDepth, wav.ErrUnsupported)
	default:
		return fmt.Errorf("%ssample format %d: %w", errPrefix, int(format), wav.ErrUnsupported)
	}
}

// ConvertedLen returns the number of destination bytes [Convert] needs for a
// source of srcLen bytes going from srcBits to dstBits. A trailing partial
// sample in the source contributes nothing, because Convert ignores it. The
// result is 0 for a negative length or for a bit depth that is not a whole
// number of bytes wide, which are exactly the cases Convert rejects.
//
// The result is also 0 when it would not fit in an int, which a widening
// conversion of a large enough source reaches on a 32-bit target. Callers
// sizing a buffer from this cannot tell that 0 apart from the one a source too
// short to hold a sample produces, so they should hand the buffer to Convert
// and let it report the difference rather than treating 0 as nothing to do.
func ConvertedLen(srcLen, srcBits, dstBits int) int {
	n, _ := convertedLen(srcLen, srcBits, dstBits)
	return n
}

// convertedLen is [ConvertedLen] with the overflow case distinguishable. The
// bool is false only when the destination size is unrepresentable, which is a
// refusal; a true with a 0 length is the ordinary "nothing to convert".
//
// The check divides before multiplying, so it never performs the multiplication
// it is guarding against. Doing it here rather than at each call site is what
// keeps a future caller from reintroducing the wrap: the streaming decoder is
// safe only because it batches, and a one-shot path that hands over a whole
// file is one line away from asking for a product that does not fit.
func convertedLen(srcLen, srcBits, dstBits int) (int, bool) {
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	if srcLen <= 0 || srcWidth <= 0 || dstWidth <= 0 {
		return 0, true
	}
	samples := srcLen / srcWidth
	if samples > math.MaxInt/dstWidth {
		return 0, false
	}
	return samples * dstWidth, true
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
// convertPlan checks everything [Convert] needs to be true before it touches a
// byte, and returns the number of destination bytes it will write. It takes
// lengths rather than slices so that the size limit it enforces can be tested
// at a boundary no allocation on this machine could reach.
func convertPlan(srcLen, dstLen int, srcFormat wav.SampleFormat, srcBits, dstBits int) (int, error) {
	if err := Validate(srcFormat, srcBits); err != nil {
		return 0, err
	}
	// The destination is always integer PCM, so it is validated against that
	// format rather than against srcFormat.
	if err := Validate(wav.SampleFormatPCM, dstBits); err != nil {
		return 0, err
	}

	need, ok := convertedLen(srcLen, srcBits, dstBits)
	if !ok {
		// Refused here rather than left to the length check below, because the
		// 0 an unrepresentable size produces would otherwise pass a check no
		// destination can fail and then be read as an empty conversion. No
		// destination could ever be long enough anyway: a slice that long does
		// not exist on this platform, which is the limiting case of a short
		// buffer rather than a different kind of failure.
		return 0, fmt.Errorf("%sconverting %d bytes of %d bit source to %d bit needs more bytes than this platform can address: %w",
			errPrefix, srcLen, srcBits, dstBits, io.ErrShortBuffer)
	}
	if dstLen < need {
		return 0, fmt.Errorf("%sdestination holds %d bytes, need %d: %w",
			errPrefix, dstLen, need, io.ErrShortBuffer)
	}
	return need, nil
}

func Convert(dst, src []byte, srcFormat wav.SampleFormat, srcBits, dstBits int) (int, error) {
	need, err := convertPlan(len(src), len(dst), srcFormat, srcBits, dstBits)
	if err != nil {
		return 0, err
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

	switch srcFormat {
	case wav.SampleFormatPCM:
		if srcBits == dstBits {
			return copy(dst, src), nil
		}
		convertIntToInt(dst, src, srcBits, dstBits)
	case wav.SampleFormatALaw:
		convertCompandedToInt(dst, src, &alawTable, dstBits)
	case wav.SampleFormatMuLaw:
		convertCompandedToInt(dst, src, &muLawTable, dstBits)
	default:
		// Float, and only float: Validate above rejects every format value
		// outside the declared set, so nothing else reaches here.
		convertFloatToInt(dst, src, srcBits, dstBits)
	}
	return need, nil
}

// convertIntToIntBlocked is the general requantiser, covering every width pair.
// dst and src must already be sized to a whole number of matching samples.
func convertIntToIntBlocked(dst, src []byte, srcBits, dstBits int) {
	srcWidth := bytesPerSample(srcBits)
	dstWidth := bytesPerSample(dstBits)
	shift := dstBits - srcBits

	// The work is blocked through a small stack buffer so that the width
	// switches can be hoisted out of the per-sample loops. Decoding and
	// encoding each become one specialised loop per width, chosen once per
	// block rather than once per sample, which is worth about twice the
	// throughput of switching per sample.
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

// convertIntToInt requantises signed integer PCM by shifting. Widening shifts
// left, which is exact; narrowing shifts right arithmetically, which truncates
// toward negative infinity and drops the low bits without rounding or dither.
// dst and src must already be sized to a whole number of matching samples.
//
// The four pairs below get a kernel of their own, and are not merely the
// blocked path unrolled: every supported depth is a whole number of bytes, so a
// shift by a multiple of eight turns into moving source bytes to a different
// position in the destination, with no intermediate value, no staging buffer
// and no per-block call. That collapse is available for all sixteen pairs, but
// only these four are worth the code, because between them they cover the
// overwhelming majority of real conversions. They measure 30 to 54 percent
// faster than the blocked path, which stays as the fallback for everything
// else. Each kernel states its own derivation, and the differential test
// compares all four against the blocked path rather than against themselves.
func convertIntToInt(dst, src []byte, srcBits, dstBits int) {
	switch {
	case srcBits == 8 && dstBits == 16:
		convert8to16(dst, src)
	case srcBits == 16 && dstBits == 32:
		convert16to32(dst, src)
	case srcBits == 24 && dstBits == 32:
		convert24to32(dst, src)
	case srcBits == 24 && dstBits == 16:
		convert24to16(dst, src)
	default:
		convertIntToIntBlocked(dst, src, srcBits, dstBits)
	}
}

// Every kernel below drives its loop off the destination, as the blocked path
// does, and reslices the source to the extent that implies, with an explicit
// capacity. That reslice is the kernel's one guard: a source too short for the
// destination fails on it, at a known line, rather than being read past the end
// sample by sample. What "too short" means there is short capacity, not short
// length, because the upper bound of a slice expression is checked against
// capacity. It is nevertheless a real guard as these kernels are wired up,
// because Convert caps src with the three-index form before dispatching, which
// makes capacity and length equal; a caller reaching a kernel directly with a
// short length inside a longer capacity is outside the contract and this does
// not catch it. The explicit capacity on those reslices is not what raises the
// panic, since the upper bound is checked against capacity with or without it;
// it is there so that a later reslice inside a kernel cannot extend back past
// the sizing one.
//
// What the sizing reslice does not do is remove the per-element bounds checks,
// however plausible that sounds. Under -d=ssa/check_bce/debug=1 every kernel
// still emits an IsSliceInBounds for every reslice it takes inside the loop:
// the dst[i*N:] all four write through, the src[i*M:] the three that read the
// source by offset take (convert8to16 does not, because it ranges over src
// instead), and in the two 24-bit kernels the [:3] window on top of those. To
// the reslices add an IsInBounds per inlined encoding/binary helper, which is
// one in three of the kernels and two in convert16to32, where a Uint16 read
// feeds a PutUint32 write. The one place the slicing does buy something is that
// window: inside an explicit [:3], the reads of b[0], b[1] and b[2] emit
// nothing at all, so it costs one check on itself and nothing further per byte.
// None of this is amd64-specific: the flag's output for this package is
// byte-identical under GOARCH=amd64 and GOARCH=arm64. Re-run it before
// asserting otherwise.

// convert8to16 widens biased 8-bit PCM to signed 16-bit.
//
// Removing the 128 bias and re-encoding the result as a 16-bit sample keeps
// only the low 16 bits, which makes the bias a flip of the top bit of the
// source byte, and the shift by 8 then puts that byte straight into the high
// half. The low byte of every destination sample is therefore zero.
func convert8to16(dst, src []byte) {
	n := len(dst) / 2
	src = src[:n:n]
	dst = dst[: n*2 : n*2]
	for i, b := range src {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(b^0x80)<<8)
	}
}

// convert16to32 widens signed 16-bit PCM to signed 32-bit.
//
// The shift by 16 moves the sample into the high half untouched and zeroes the
// low half. Sign extension is skipped rather than forgotten: the bits it would
// produce all shift out of a 32-bit result, and the sample's own sign bit lands
// on bit 31 where the destination wants it.
func convert16to32(dst, src []byte) {
	n := len(dst) / 4
	src = src[: n*2 : n*2]
	dst = dst[: n*4 : n*4]
	for i := range n {
		binary.LittleEndian.PutUint32(dst[i*4:], uint32(binary.LittleEndian.Uint16(src[i*2:]))<<16)
	}
}

// convert24to32 widens packed 24-bit PCM to signed 32-bit.
//
// The shift by 8 moves each of the three source bytes up one position and
// zeroes the lowest, so bit 23 becomes bit 31. As in convert16to32 the sign
// extension the general path performs is discarded by that same shift.
func convert24to32(dst, src []byte) {
	n := len(dst) / 4
	src = src[: n*3 : n*3]
	dst = dst[: n*4 : n*4]
	for i := range n {
		b := src[i*3:][:3]
		u := uint32(b[0])<<8 | uint32(b[1])<<16 | uint32(b[2])<<24
		binary.LittleEndian.PutUint32(dst[i*4:], u)
	}
}

// convert24to16 narrows packed 24-bit PCM to signed 16-bit.
//
// An arithmetic shift right by 8, truncated to 16 bits, is exactly the top two
// bytes of the source sample: dropping the low byte is what truncating toward
// negative infinity means at a byte boundary, for negative samples as much as
// positive ones, so no rounding or sign fix-up is needed.
func convert24to16(dst, src []byte) {
	n := len(dst) / 2
	src = src[: n*3 : n*3]
	dst = dst[: n*2 : n*2]
	for i := range n {
		b := src[i*3:][:3]
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(b[2])<<8|uint16(b[1]))
	}
}

// blockSamples is how many samples the conversion loops stage at a time.
//
// At 1024 samples the staging buffer is 4096 bytes, and the frame holding it is
// that buffer plus somewhere around 150 bytes of everything else, which puts it
// well over a goroutine's 2 KiB starting stack, so the first conversion on a
// fresh goroutine pays one stack growth. Only two functions pay it: the four
// single-pass kernels above never touch the staging buffer and their frames are
// a couple of words wide, so the width pairs they cover are unaffected, and what
// is left paying is the float path and the eight integer pairs that still fall
// through to the blocked one.
//
// The exact frame sizes are a property of the architecture and the toolchain,
// not of this code, so read them off `go build -gcflags=-S` for the target in
// hand rather than trusting a number written here. With Go 1.26.3 the blocked
// path came out at 4240 bytes on amd64 and 4224 on arm64, the float path at
// 4280 and 4240, and each of the four kernels at 8 bytes and 16. Whether the
// kernels are additionally marked NOSPLIT varies the same way: on amd64 they
// are, on arm64 they are not.
//
// Shrinking the block to fit under that stack was tried and dropped:
// interleaved measurement put the difference at a couple of percent in favour
// of the larger block, not significant per benchmark. Fitting would also have
// meant cutting the block to a quarter: at roughly 150 bytes of frame outside
// the buffer there is room for about 470 int32 samples under 2 KiB, and on the
// two architectures measured it is (2048-144)/4 = 476 and (2048-128)/4 = 480,
// so 512 does not fit either way and 256 is the largest power of two that does
// with any room for the callers' frames above it. Go also grows a goroutine's
// starting stack adaptively, so a process converting repeatedly stops paying
// the growth at all.
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
			// The [:3] window is what makes these three reads free, not the
			// order they are written in: under -d=ssa/check_bce/debug=1,
			// rewriting them in ascending order leaves the output for this
			// package byte-identical, while dropping the window and keeping
			// the order trades the window's own IsSliceInBounds for an
			// IsInBounds on the b[2] read. encodeBlock's 24-bit arm carries a
			// similar-looking note that rests on the other mechanism: it has
			// no window, so there the ordering is what carries the one check,
			// and writing those three in ascending order costs three.
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
//
// This path deliberately gets no equivalent of the single-pass kernels above,
// and the reason is not that nobody has written them. Those kernels exist
// because a shift by a multiple of eight is a byte move, so each destination
// byte is some source byte at another offset and the sample value never has to
// be formed. Quantisation has no such form at any width: every sample needs a
// multiply by full scale, a clamp against both limits and a round to nearest,
// so the value must be computed rather than repositioned. Blocking the loop to
// hoist the two width switches, which is what happens below, is therefore the
// whole of the structural saving available here, and the rest of the work went
// into quantize instead.
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
