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
	src = src[:(need/dstWidth)*srcWidth]
	dst = dst[:need]

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
	// A shift is computed once rather than per sample; the sign selects the
	// direction. Both operands stay int64 so a 32-bit host behaves identically.
	shift := dstBits - srcBits
	up := shift >= 0
	if !up {
		shift = -shift
	}
	for si, di := 0, 0; di < len(dst); si, di = si+srcWidth, di+dstWidth {
		v := decodeInt(src[si:], srcBits)
		if up {
			v <<= uint(shift)
		} else {
			v >>= uint(shift)
		}
		encodeInt(dst[di:], v, dstBits)
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
	wide := srcBits == 64
	for si, di := 0, 0; di < len(dst); si, di = si+srcWidth, di+dstWidth {
		var f float64
		if wide {
			f = math.Float64frombits(binary.LittleEndian.Uint64(src[si:]))
		} else {
			f = float64(math.Float32frombits(binary.LittleEndian.Uint32(src[si:])))
		}
		encodeInt(dst[di:], quantize(f, fullScale, posLimit, negLimit), dstBits)
	}
}

// quantize scales one float sample to an integer sample. NaN becomes 0 so a
// broken sample cannot poison the output; the clamp then absorbs both
// infinities and every finite value past full scale, which real-world float WAV
// files carry routinely. Only in-range values reach math.Round, which rounds
// half away from zero.
func quantize(f, fullScale, posLimit, negLimit float64) int64 {
	if math.IsNaN(f) {
		return 0
	}
	v := f * fullScale
	switch {
	case v >= posLimit:
		// +Inf >= posLimit is true, so this arm covers positive overflow of
		// every magnitude without a separate IsInf test.
		return int64(posLimit)
	case v <= negLimit:
		return int64(negLimit)
	default:
		return int64(math.Round(v))
	}
}

// decodeInt reads one little-endian integer PCM sample of the given width and
// returns it as a signed value. The 8-bit case is the odd one out: it is stored
// biased by 128, so the bias is removed here and nowhere else.
func decodeInt(b []byte, bits int) int64 {
	switch bits {
	case 8:
		return int64(b[0]) - 128
	case 16:
		return int64(int16(binary.LittleEndian.Uint16(b)))
	case 24:
		u := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if u&0x800000 != 0 {
			u |= 0xFF000000 // sign-extend bit 23 into the unused high byte
		}
		return int64(int32(u))
	default: // 32
		return int64(int32(binary.LittleEndian.Uint32(b)))
	}
}

// encodeInt writes one signed sample as little-endian integer PCM of the given
// width. v must already be in range for bits; every caller either shifts into
// range or clamps. The 8-bit case re-applies the 128 bias.
func encodeInt(b []byte, v int64, bits int) {
	switch bits {
	case 8:
		b[0] = byte(v + 128)
	case 16:
		binary.LittleEndian.PutUint16(b, uint16(v)) //nolint:gosec // G115: v is in int16 range by construction.
	case 24:
		u := uint32(v) //nolint:gosec // G115: v is in 24-bit signed range by construction.
		b[0] = byte(u)
		b[1] = byte(u >> 8)
		b[2] = byte(u >> 16) // packed, three bytes, no padding
	default: // 32
		binary.LittleEndian.PutUint32(b, uint32(v)) //nolint:gosec // G115: v is in int32 range by construction.
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
