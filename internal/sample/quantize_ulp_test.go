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
			v := decodeIntRef(dst[i*width:], bits)
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

// TestConvertFloatCrossesBlockBoundary drives the float path past the internal
// block size, which nothing else in the suite does.
//
// convertFloatToInt stages through a fixed 1024-sample buffer. A regression in
// its block arithmetic, processing only the first block or dropping a trailing
// partial one, is invisible to any input that fits in a single block, and every
// other float test in this package is smaller than that. The sizes below are
// chosen to give two full blocks plus a partial tail, and an exact multiple.
func TestConvertFloatCrossesBlockBoundary(t *testing.T) {
	for _, samples := range []int{blockSamples - 1, blockSamples, blockSamples + 1, 2*blockSamples + 7, 3 * blockSamples} {
		for _, srcBits := range []int{32, 64} {
			for _, dstBits := range []int{16, 24, 32} {
				srcWidth := srcBits / 8
				src := make([]byte, samples*srcWidth)
				// A deterministic ramp inside full scale, so every sample
				// reaches the rounding path rather than the clamp.
				for i := range samples {
					v := float64(i%2001-1000) / 1001.0
					if srcBits == 32 {
						putU32LE(src[i*4:], math.Float32bits(float32(v)))
					} else {
						putF64(src[i*8:], v)
					}
				}

				dst := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
				n, err := Convert(dst, src, wav.SampleFormatFloat, srcBits, dstBits)
				if err != nil {
					t.Fatalf("f%d->s%d n=%d: %v", srcBits, dstBits, samples, err)
				}
				if want := samples * (dstBits / 8); n != want {
					t.Fatalf("f%d->s%d n=%d: wrote %d bytes, want %d", srcBits, dstBits, samples, n, want)
				}

				// Every sample must match a direct scalar quantisation, so a
				// block that was skipped or truncated shows up as a zero run.
				fullScale := float64(int64(1) << uint(dstBits-1))
				posLimit, negLimit := fullScale-1, -fullScale
				width := dstBits / 8
				for i := range samples {
					v := float64(i%2001-1000) / 1001.0
					if srcBits == 32 {
						v = float64(float32(v))
					}
					want := quantize(v, fullScale, posLimit, negLimit)
					if got := decodeIntRef(dst[i*width:], dstBits); got != want {
						t.Fatalf("f%d->s%d n=%d: sample %d = %d, want %d",
							srcBits, dstBits, samples, i, got, want)
					}
				}
			}
		}
	}
}

func putU32LE(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
