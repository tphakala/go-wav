package sample

import (
	"math"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// TestQuantizeMatchesMathRoundAtBinadeBoundary pins the two inputs where
// rounding by adding a half would disagree with math.Round.
//
// The largest float64 below 0.5, plus 0.5, lands exactly on 1.0 once rounded
// to the nearest representable value, because the sum crosses into a binade
// whose significand is a bit coarser. Quantising that to 1 rather than 0 would
// be a silent one-LSB change in decoded audio, so quantize takes the sub-half
// band before it can happen. These are the only two such inputs.
func TestQuantizeMatchesMathRoundAtBinadeBoundary(t *testing.T) {
	justBelowHalf := math.Nextafter(0.5, 0)
	cases := []struct {
		name string
		v    float64
		want int64
	}{
		{"largest value below +0.5", justBelowHalf, 0},
		{"largest value above -0.5", -justBelowHalf, 0},
		{"exactly +0.5 rounds away from zero", 0.5, 1},
		{"exactly -0.5 rounds away from zero", -0.5, -1},
		{"just above +0.5", math.Nextafter(0.5, 1), 1},
		{"just below -0.5", math.Nextafter(-0.5, -1), -1},
	}

	for _, bits := range []int{8, 16, 24, 32} {
		fullScale := float64(int64(1) << uint(bits-1))
		posLimit, negLimit := fullScale-1, -fullScale
		for _, tc := range cases {
			// quantize scales by fullScale, so feed the pre-scaled value.
			f := tc.v / fullScale
			got := quantize(f, fullScale, posLimit, negLimit)
			if got != tc.want {
				t.Errorf("bits=%d %s: quantize(%v) = %d, want %d", bits, tc.name, tc.v, got, tc.want)
			}
			// And it must agree with math.Round, which is the contract.
			if want := int64(math.Round(tc.v)); got != want {
				t.Errorf("bits=%d %s: quantize gave %d, math.Round gives %d", bits, tc.name, got, want)
			}
		}
	}
}

// TestQuantizeAgreesWithMathRoundAcrossULPBoundaries sweeps every representable
// value either side of each half-integer, which is where a hand-rolled rounding
// can diverge from math.Round without any example-based test noticing.
func TestQuantizeAgreesWithMathRoundAcrossULPBoundaries(t *testing.T) {
	const bits = 32
	fullScale := float64(int64(1) << uint(bits-1))
	posLimit, negLimit := fullScale-1, -fullScale

	for k := 0.0; k < 48; k++ {
		for _, base := range []float64{k + 0.5, -(k + 0.5), k, -k} {
			v := base
			for range 5 {
				v = math.Nextafter(v, math.Inf(-1))
			}
			for range 11 {
				if v > negLimit && v < posLimit {
					got := quantize(v/fullScale, fullScale, posLimit, negLimit)
					if want := int64(math.Round(v)); got != want {
						t.Errorf("quantize(%.20g) = %d, math.Round = %d (bits %016x)",
							v, got, want, math.Float64bits(v))
					}
				}
				v = math.Nextafter(v, math.Inf(1))
			}
		}
	}
}

// TestConvertFloatSubHalfBand checks the fix through the public entry point,
// so the guard cannot be correct in quantize yet bypassed by the block loop.
func TestConvertFloatSubHalfBand(t *testing.T) {
	justBelowHalf := math.Nextafter(0.5, 0)
	for _, bits := range []int{8, 16, 24, 32} {
		fullScale := float64(int64(1) << uint(bits-1))
		src := make([]byte, 16)
		// Two samples: +just-below-half and -just-below-half, in sample units.
		putF64(src[0:], justBelowHalf/fullScale)
		putF64(src[8:], -justBelowHalf/fullScale)

		dst := make([]byte, ConvertedLen(len(src), 64, bits))
		if _, err := Convert(dst, src, wav.SampleFormatFloat, 64, bits); err != nil {
			t.Fatalf("bits=%d: %v", bits, err)
		}
		width := bits / 8
		for i := range 2 {
			v := decodeInt(dst[i*width:], bits)
			if v != 0 {
				t.Errorf("bits=%d sample %d: got %d, want 0 (a value inside half an LSB must quantise to zero)",
					bits, i, v)
			}
		}
	}
}

func putF64(b []byte, v float64) {
	u := math.Float64bits(v)
	for i := range 8 {
		b[i] = byte(u >> (8 * i))
	}
}
