package sample

// G.711 companding, decode side.
//
// A-law and mu-law store one byte per sample, and that byte is a floating point
// number in miniature: a sign bit, a three-bit segment that acts as an exponent
// and a four-bit mantissa. Expanding one is therefore a pure function of 256
// inputs, which is why every implementation of these laws is a lookup table.
//
// The tables here are computed at initialisation from the laws themselves
// rather than written out as literals. A transcribed table is 256 numbers with
// no structure a reader can check, so a single wrong entry is invisible on
// inspection and inaudible in aggregate: it puts one sample value in the wrong
// place, on whichever codes happen to occur. Deriving them means the only thing
// that can be wrong is the law, which is eight lines and is stated a second,
// independent way in the tests.
//
// The cost of deriving them is 512 iterations at process start and 1 KiB of
// data for the two tables together, which stays in L1 for the whole of any
// decode.

// alawTable and muLawTable expand each of the 256 codes of their law to the
// linear 16-bit sample it represents.
var (
	alawTable  = buildTable(decodeALaw)
	muLawTable = buildTable(decodeMuLaw)
)

// buildTable evaluates a decoding law over the whole 8-bit code space.
func buildTable(decode func(byte) int16) [256]int16 {
	var t [256]int16
	for code := range t {
		//nolint:gosec // G115: the loop index is bounded by the array length.
		t[code] = decode(byte(code))
	}
	return t
}

// A-law and mu-law field layout, shared by both laws because G.711 gives them
// the same shape and differs only in how the byte is inverted and how the
// segments are scaled.
const (
	// signBit is set for positive samples in A-law and for negative ones in
	// mu-law, in each case after the law's inversion has been undone.
	signBit byte = 0x80
	// segMask and segShift select the three-bit segment, which acts as an
	// exponent.
	segMask  byte = 0x70
	segShift byte = 4
	// mantMask selects the four-bit mantissa within a segment.
	mantMask byte = 0x0F
	// alawInvert is the even-bit inversion A-law applies on the wire. It
	// exists to keep the code for silence from being a long run of zero
	// bits, which a transmission line cannot recover a clock from.
	alawInvert byte = 0x55
	// muLawBias is the offset of 33 half-steps, scaled by the factor of four
	// that lifts mu-law into 16 bits, which makes the mu-law segments join
	// continuously. It is added before the segment scaling and removed after.
	muLawBias int32 = 0x84
)

// decodeALaw expands one A-law code to its linear 16-bit sample.
//
// The mantissa is shifted into place by four, which is the factor of eight that
// lifts the 13-bit law into a 16-bit container combined with the step of two
// between adjacent mantissa values. Segments zero and one share the finest
// step, so only segment two and above scale, each doubling the one below it.
// The constant added to the mantissa centres the sample in its quantisation
// interval instead of leaving it at the interval's floor, which halves the
// worst-case error.
func decodeALaw(code byte) int16 {
	c := code ^ alawInvert
	seg := (c & segMask) >> segShift
	mag := int32(c&mantMask) << 4

	switch seg {
	case 0:
		mag += 8
	case 1:
		mag += 0x108
	default:
		mag = (mag + 0x108) << (seg - 1)
	}

	//nolint:gosec // G115: the law's largest magnitude is 32256, inside int16.
	if c&signBit != 0 {
		return int16(mag)
	}
	return int16(-mag)
}

// decodeMuLaw expands one mu-law code to its linear 16-bit sample.
//
// Every bit is inverted on the wire, for the same clock-recovery reason A-law
// inverts every other one, so the code is complemented first. The mantissa is
// shifted by three, which is the factor of four that lifts the 14-bit law into
// a 16-bit container combined with the step of two between mantissa values.
// Unlike A-law the segments are uniform, including the first, and the bias is
// what makes them join: it is added before the segment scaling and taken off
// after.
func decodeMuLaw(code byte) int16 {
	c := ^code
	mag := (int32(c&mantMask) << 3) + muLawBias
	mag <<= (c & segMask) >> segShift

	//nolint:gosec // G115: the law's largest magnitude is 32124, inside int16.
	if c&signBit != 0 {
		return int16(muLawBias - mag)
	}
	return int16(mag - muLawBias)
}

// convertCompandedToInt expands companded samples to linear PCM at dstBits. dst
// and src must already be sized to a whole number of matching samples.
//
// The expansion always lands on 16 bits first, because that is the width the
// laws are defined against, and any other destination is then the ordinary
// integer requantisation from 16 bits: the same arithmetic shift, truncating
// toward negative infinity when narrowing, that convertIntToInt applies. Doing
// it in one pass rather than materialising the 16-bit form is a saving, not a
// different rule, and the test that pins it compares this path against decoding
// to 16 bits and converting from there.
//
// Blocking mirrors the other conversion loops so that the destination width is
// switched on once per block rather than once per sample.
func convertCompandedToInt(dst, src []byte, table *[256]int16, dstBits int) {
	dstWidth := bytesPerSample(dstBits)
	shift := dstBits - 16

	// int32 holds every value this produces. The widest case is 32 bits, where
	// the A-law peak of 32256 shifted left by 16 is 2114125824, well inside the
	// range.
	var tmp [blockSamples]int32

	total := len(dst) / dstWidth
	for done := 0; done < total; {
		n := min(blockSamples, total-done)
		codes := src[done : done+n]
		for i := range tmp[:n] {
			tmp[i] = int32(table[codes[i]])
		}
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
