package sample

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// The oracle below states G.711 the way the recommendation's segment tables
// state it: a code carries a sign, a segment index and a mantissa, and the
// decoded magnitude is the midpoint of the quantisation interval that pair
// selects, scaled by the step size of the segment. The shipped tables are built
// from the bit-manipulating form instead. The two are independent statements of
// the same law, so a mistake in either shows up as a disagreement across all
// 256 codes rather than being confirmed by its own logic, which is the same
// arrangement the integer oracle in oracle_test.go provides.

// alawRef decodes one A-law code from the segment structure of G.711.
//
// The even bits are inverted on the wire, so they come back first. Segment zero
// and segment one share the finest step, which is why the first segment is the
// one arm that does not scale; from segment one upward each segment doubles the
// step, and the bias of 33 half-steps is what places the mantissa at the middle
// of its interval rather than at the bottom. The factor of eight lifts the
// 13-bit law into the 16-bit container.
func alawRef(code byte) int16 {
	c := code ^ 0x55
	seg := int(c>>4) & 0x07
	mant := int(c & 0x0F)

	mag := (2*mant + 1) * 8
	if seg > 0 {
		mag = ((2*mant + 33) * 8) << (seg - 1)
	}
	//nolint:gosec // G115: the largest magnitude the law produces is 32256.
	if c&0x80 == 0 {
		return int16(-mag)
	}
	return int16(mag)
}

// muLawRef decodes one mu-law code from the segment structure of G.711.
//
// Every bit is inverted on the wire, so the code is complemented first. Unlike
// A-law the segments are uniform: each doubles the step of the one below it,
// including the first. The bias of 33 half-steps is added before the segment
// scaling and taken off after, which is what makes the segment boundaries
// continuous, and the factor of four lifts the 14-bit law into the 16-bit
// container.
func muLawRef(code byte) int16 {
	c := ^code
	seg := int(c>>4) & 0x07
	mant := int(c & 0x0F)

	mag := (((2*mant + 33) << seg) - 33) * 4
	//nolint:gosec // G115: the largest magnitude the law produces is 32124.
	if c&0x80 != 0 {
		return int16(-mag)
	}
	return int16(mag)
}

// TestCompandedTablesMatchG711Oracle checks every one of the 256 codes of each
// law against the independent reference above.
//
// A single wrong entry is a silent audio defect: it neither fails to decode nor
// looks wrong in any aggregate, it just puts one sample value in the wrong
// place. Exhaustive comparison is cheap at 256 entries and is the only way to
// rule that out.
func TestCompandedTablesMatchG711Oracle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table *[256]int16
		ref   func(byte) int16
	}{
		{"a-law", &alawTable, alawRef},
		{"mu-law", &muLawTable, muLawRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for code := range 256 {
				got, want := tc.table[code], tc.ref(byte(code))
				if got != want {
					t.Errorf("code 0x%02X decoded to %d, G.711 gives %d", code, got, want)
				}
			}
		})
	}
}

// TestCompandedTableShape pins the properties that distinguish the two laws
// from each other, so that a table built from the wrong law, or with the sign
// or the bias transposed between them, cannot pass by matching an oracle that
// made the same mistake.
//
// A-law reaches 32256 in steps of 8 and has no code for silence, because its
// first quantisation interval is centred half a step above zero. Mu-law reaches
// 32124 in steps of 4 and does decode to zero, from the two codes its bias
// leaves at the origin, which is why it has 255 distinct values rather than
// 256.
func TestCompandedTableShape(t *testing.T) {
	for _, tc := range []struct {
		name          string
		table         *[256]int16
		peak, step    int16
		zeroes        int
		distinctCount int
	}{
		{"a-law", &alawTable, 32256, 8, 0, 256},
		{"mu-law", &muLawTable, 32124, 4, 2, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(map[int16]bool, 256)
			var lo, hi int16
			zeroes := 0
			for code := range 256 {
				v := tc.table[code]
				if v%tc.step != 0 {
					t.Errorf("code 0x%02X decoded to %d, not a multiple of the step %d", code, v, tc.step)
				}
				if v == 0 {
					zeroes++
				}
				lo, hi = min(lo, v), max(hi, v)
				seen[v] = true
			}
			if lo != -tc.peak || hi != tc.peak {
				t.Errorf("range is [%d, %d], want [%d, %d]", lo, hi, -tc.peak, tc.peak)
			}
			if zeroes != tc.zeroes {
				t.Errorf("%d codes decode to silence, want %d", zeroes, tc.zeroes)
			}
			if len(seen) != tc.distinctCount {
				t.Errorf("%d distinct values, want %d", len(seen), tc.distinctCount)
			}
		})
	}
}

// TestValidateCompandedDepth checks that the two laws are accepted at the only
// width they are stored in and rejected everywhere else. A fmt chunk claiming
// A-law at 16 bits describes nothing that exists, so it is refused rather than
// read as though the depth field were a typo.
func TestValidateCompandedDepth(t *testing.T) {
	for _, format := range []wav.SampleFormat{wav.SampleFormatALaw, wav.SampleFormatMuLaw} {
		t.Run(format.String(), func(t *testing.T) {
			if err := Validate(format, 8); err != nil {
				t.Errorf("Validate(%v, 8) = %v, want nil", format, err)
			}
			for _, bits := range []int{0, 1, 4, 12, 16, 24, 32, 64} {
				err := Validate(format, bits)
				if !errors.Is(err, wav.ErrUnsupported) {
					t.Errorf("Validate(%v, %d) = %v, want wav.ErrUnsupported", format, bits, err)
				}
			}
		})
	}
}

// TestConvertCompandedMatchesDecodeThenRequantise is the statement of what
// converting a companded source means: expand the code to its linear 16-bit
// value, then requantise that value by the same shift rule every other integer
// pair uses. Going straight to 24 bits must therefore give exactly what going
// to 16 and then to 24 gives, which is the property a caller passing
// WithConvertTo some width other than 16 depends on.
func TestConvertCompandedMatchesDecodeThenRequantise(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}

	for _, tc := range []struct {
		format wav.SampleFormat
		ref    func(byte) int16
	}{
		{wav.SampleFormatALaw, alawRef},
		{wav.SampleFormatMuLaw, muLawRef},
	} {
		// The linear 16-bit form of the same 256 codes, built from the oracle
		// rather than from the package, so the comparison stays independent.
		linear := make([]byte, 512)
		for i, b := range all {
			encodeIntRef(linear[i*2:], int64(tc.ref(b)), 16)
		}

		for _, dstBits := range []int{8, 16, 24, 32} {
			t.Run(fmt.Sprintf("%s_to_s%d", tc.format, dstBits), func(t *testing.T) {
				got := make([]byte, ConvertedLen(len(all), 8, dstBits))
				n, err := Convert(got, all, tc.format, 8, dstBits)
				if err != nil {
					t.Fatalf("Convert from %v: %v", tc.format, err)
				}
				if n != len(got) {
					t.Fatalf("Convert wrote %d bytes, want %d", n, len(got))
				}

				want := make([]byte, ConvertedLen(len(linear), 16, dstBits))
				if _, err := Convert(want, linear, wav.SampleFormatPCM, 16, dstBits); err != nil {
					t.Fatalf("Convert from linear 16: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("converting %v straight to %d bits differs from going via 16 bits",
						tc.format, dstBits)
				}
			})
		}
	}
}

// TestConvertCompandedRejectsWrongSourceWidth checks that the width guard on
// the source reaches Convert and not only Validate.
func TestConvertCompandedRejectsWrongSourceWidth(t *testing.T) {
	dst := make([]byte, 64)
	src := make([]byte, 64)
	for _, format := range []wav.SampleFormat{wav.SampleFormatALaw, wav.SampleFormatMuLaw} {
		if _, err := Convert(dst, src, format, 16, 16); !errors.Is(err, wav.ErrUnsupported) {
			t.Errorf("Convert(%v at 16 bits) = %v, want wav.ErrUnsupported", format, err)
		}
	}
}

// FuzzConvertCompanded checks the companded path sample by sample against the
// oracle for an arbitrary payload and every destination width, which is what
// pins the blocking in the conversion loop: a block boundary landing wrong
// would show up here as a sample taken from the wrong offset, and nowhere in
// the fixed-length tests above.
func FuzzConvertCompanded(f *testing.F) {
	f.Add([]byte{0x00, 0x55, 0xD5, 0xFF}, uint8(16))
	f.Add(bytes.Repeat([]byte{0x7F}, 3000), uint8(24))
	f.Add([]byte{}, uint8(8))

	depths := []int{8, 16, 24, 32}
	f.Fuzz(func(t *testing.T, src []byte, sel uint8) {
		dstBits := depths[int(sel)%len(depths)]
		for _, tc := range []struct {
			format wav.SampleFormat
			ref    func(byte) int16
		}{
			{wav.SampleFormatALaw, alawRef},
			{wav.SampleFormatMuLaw, muLawRef},
		} {
			width := dstBits / 8
			dst := make([]byte, ConvertedLen(len(src), 8, dstBits))
			n, err := Convert(dst, src, tc.format, 8, dstBits)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			if n != len(src)*width {
				t.Fatalf("Convert wrote %d bytes for %d codes at %d bits", n, len(src), dstBits)
			}
			for i, code := range src {
				want := int64(tc.ref(code))
				if shift := dstBits - 16; shift > 0 {
					want <<= uint(shift)
				} else {
					want >>= uint(-shift)
				}
				if got := decodeIntRef(dst[i*width:], dstBits); got != want {
					t.Fatalf("%v code 0x%02X at %d bits gave %d, want %d",
						tc.format, code, dstBits, got, want)
				}
			}
		}
	})
}
