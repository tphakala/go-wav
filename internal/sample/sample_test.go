package sample

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// pcm8 builds unsigned 8-bit PCM from raw on-disk byte values, so a test can
// state the stored bytes rather than the signed values they represent.
func pcm8(vals ...uint8) []byte {
	return bytes.Clone(vals)
}

// pcm16 builds little-endian signed 16-bit PCM.
func pcm16(vals ...int16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

// pcm24 builds little-endian signed 24-bit PCM packed three bytes per sample.
func pcm24(vals ...int32) []byte {
	b := make([]byte, 3*len(vals))
	for i, v := range vals {
		u := uint32(v)
		b[3*i] = byte(u)
		b[3*i+1] = byte(u >> 8)
		b[3*i+2] = byte(u >> 16)
	}
	return b
}

// pcm32 builds little-endian signed 32-bit PCM.
func pcm32(vals ...int32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], uint32(v))
	}
	return b
}

// f32 builds little-endian IEEE 754 binary32 sample data.
func f32(vals ...float32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// f64 builds little-endian IEEE 754 binary64 sample data.
func f64(vals ...float64) []byte {
	b := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(b[8*i:], math.Float64bits(v))
	}
	return b
}

// decodeAll unpacks every whole sample of the given width as a signed value, so
// assertions can read in sample space instead of byte space.
func decodeAll(t *testing.T, b []byte, bits int) []int64 {
	t.Helper()
	w := bytesPerSample(bits)
	if w == 0 {
		t.Fatalf("decodeAll: unsupported bit depth %d", bits)
	}
	out := make([]int64, 0, len(b)/w)
	for i := 0; i+w <= len(b); i += w {
		out = append(out, decodeInt(b[i:], bits))
	}
	return out
}

// convertTo runs Convert into a freshly sized destination and fails the test on
// any error, for the many cases where only the output bytes are of interest.
func convertTo(t *testing.T, src []byte, format wav.SampleFormat, srcBits, dstBits int) []byte {
	t.Helper()
	dst := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
	n, err := Convert(dst, src, format, srcBits, dstBits)
	if err != nil {
		t.Fatalf("Convert(%v, %d -> %d) returned error: %v", format, srcBits, dstBits, err)
	}
	if n != len(dst) {
		t.Fatalf("Convert(%v, %d -> %d) wrote %d bytes, want %d", format, srcBits, dstBits, n, len(dst))
	}
	return dst
}

// Expected clamp targets in the float conversion tables, naming the limit a
// case must land on rather than repeating the literal value per depth.
const (
	wantMin  = "min"
	wantMax  = "max"
	wantZero = "zero"
)

// limits are the signed bounds of each supported integer PCM depth.
var limits = map[int]struct{ min, max int64 }{
	8:  {-128, 127},
	16: {-32768, 32767},
	24: {-8388608, 8388607},
	32: {-2147483648, 2147483647},
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		format  wav.SampleFormat
		bits    int
		wantErr bool
	}{
		{"pcm 8", wav.SampleFormatPCM, 8, false},
		{"pcm 16", wav.SampleFormatPCM, 16, false},
		{"pcm 24", wav.SampleFormatPCM, 24, false},
		{"pcm 32", wav.SampleFormatPCM, 32, false},
		{"pcm 64 is not supported", wav.SampleFormatPCM, 64, true},
		{"pcm 12 is not supported", wav.SampleFormatPCM, 12, true},
		{"pcm 0 is not supported", wav.SampleFormatPCM, 0, true},
		{"pcm negative is not supported", wav.SampleFormatPCM, -16, true},
		{"float 32", wav.SampleFormatFloat, 32, false},
		{"float 64", wav.SampleFormatFloat, 64, false},
		{"float 16 is not supported", wav.SampleFormatFloat, 16, true},
		{"float 24 is not supported", wav.SampleFormatFloat, 24, true},
		{"unknown format", wav.SampleFormat(42), 16, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.format, tt.bits)
			if tt.wantErr {
				if !errors.Is(err, wav.ErrUnsupported) {
					t.Fatalf("Validate(%v, %d) = %v, want an error wrapping wav.ErrUnsupported", tt.format, tt.bits, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%v, %d) = %v, want nil", tt.format, tt.bits, err)
			}
		})
	}
}

func TestConvertedLen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		srcLen           int
		srcBits, dstBits int
		want             int
	}{
		{"16 to 16 is one for one", 8, 16, 16, 8},
		{"16 to 32 doubles", 8, 16, 32, 16},
		{"32 to 16 halves", 16, 32, 16, 8},
		{"24 to 16 is three bytes to two", 9, 24, 16, 6},
		{"float 64 to 24", 24, 64, 24, 9},
		{"trailing partial sample is dropped", 7, 16, 8, 3},
		{"source shorter than one sample", 2, 24, 16, 0},
		{"empty source", 0, 16, 8, 0},
		{"negative length", -4, 16, 8, 0},
		{"unsupported source depth", 8, 12, 16, 0},
		{"unsupported destination depth", 8, 16, 12, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ConvertedLen(tt.srcLen, tt.srcBits, tt.dstBits); got != tt.want {
				t.Fatalf("ConvertedLen(%d, %d, %d) = %d, want %d", tt.srcLen, tt.srcBits, tt.dstBits, got, tt.want)
			}
		})
	}
}

// TestConvertIntegerRoundTrip widens integer PCM and narrows it straight back.
// Because both directions are pure shifts by the same amount, the original
// bytes must come back untouched.
func TestConvertIntegerRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		low, high int
		src       []byte
	}{
		{"8 to 16 to 8", 8, 16, pcm8(0, 1, 42, 127, 128, 129, 200, 254, 255)},
		{"8 to 24 to 8", 8, 24, pcm8(0, 1, 127, 128, 129, 255)},
		{"8 to 32 to 8", 8, 32, pcm8(0, 1, 127, 128, 129, 255)},
		{"16 to 24 to 16", 16, 24, pcm16(-32768, -1, 0, 1, 12345, 32767)},
		{"16 to 32 to 16", 16, 32, pcm16(-32768, -1, 0, 1, 12345, 32767)},
		{"24 to 32 to 24", 24, 32, pcm24(-8388608, -1, 0, 1, 1234567, 8388607)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wide := convertTo(t, tt.src, wav.SampleFormatPCM, tt.low, tt.high)
			back := convertTo(t, wide, wav.SampleFormatPCM, tt.high, tt.low)
			if !bytes.Equal(back, tt.src) {
				t.Fatalf("round trip %d -> %d -> %d produced % x, want % x", tt.low, tt.high, tt.low, back, tt.src)
			}
		})
	}
}

// TestConvertIdentityIsACopy checks that an unchanged (format, depth) pair
// reproduces the source bytes exactly at every supported depth.
func TestConvertIdentityIsACopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		bits int
		src  []byte
	}{
		{"8 bit", 8, pcm8(0, 1, 128, 254, 255)},
		{"16 bit", 16, pcm16(-32768, -1, 0, 1, 32767)},
		{"24 bit", 24, pcm24(-8388608, -1, 0, 1, 8388607)},
		{"32 bit", 32, pcm32(-2147483648, -1, 0, 1, 2147483647)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := convertTo(t, tt.src, wav.SampleFormatPCM, tt.bits, tt.bits)
			if !bytes.Equal(got, tt.src) {
				t.Fatalf("identity convert at %d bits produced % x, want % x", tt.bits, got, tt.src)
			}
		})
	}
}

// TestConvertIntegerExactBytes pins the exact output bytes at every boundary
// value of every supported depth, in both the widening and narrowing direction.
func TestConvertIntegerExactBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		src              []byte
		srcBits, dstBits int
		want             []byte
	}{
		{
			name: "8 bit boundaries widen to 16 bit",
			src:  pcm8(0, 128, 255), srcBits: 8, dstBits: 16,
			want: pcm16(-32768, 0, 32512),
		},
		{
			name: "8 bit boundaries widen to 24 bit",
			src:  pcm8(0, 128, 255), srcBits: 8, dstBits: 24,
			want: pcm24(-8388608, 0, 8323072),
		},
		{
			name: "8 bit boundaries widen to 32 bit",
			src:  pcm8(0, 128, 255), srcBits: 8, dstBits: 32,
			want: pcm32(-2147483648, 0, 2130706432),
		},
		{
			name: "16 bit boundaries widen to 32 bit",
			src:  pcm16(-32768, 0, 32767), srcBits: 16, dstBits: 32,
			want: pcm32(-2147483648, 0, 2147418112),
		},
		{
			name: "24 bit boundaries widen to 32 bit",
			src:  pcm24(-8388608, 0, 8388607), srcBits: 24, dstBits: 32,
			want: pcm32(-2147483648, 0, 2147483392),
		},
		{
			name: "16 bit boundaries narrow to 8 bit",
			src:  pcm16(-32768, 0, 32767, -1), srcBits: 16, dstBits: 8,
			// -1 >> 8 is -1, not 0: narrowing truncates toward negative
			// infinity, so a value just below zero stays just below zero.
			want: pcm8(0, 128, 255, 127),
		},
		{
			name: "24 bit boundaries narrow to 16 bit",
			src:  pcm24(-8388608, 0, 8388607, -1), srcBits: 24, dstBits: 16,
			want: pcm16(-32768, 0, 32767, -1),
		},
		{
			name: "32 bit boundaries narrow to 24 bit",
			src:  pcm32(-2147483648, 0, 2147483647), srcBits: 32, dstBits: 24,
			want: pcm24(-8388608, 0, 8388607),
		},
		{
			name: "32 bit boundaries narrow to 8 bit",
			src:  pcm32(-2147483648, 0, 2147483647), srcBits: 32, dstBits: 8,
			want: pcm8(0, 128, 255),
		},
		{
			name: "narrowing truncates rather than rounding",
			src:  pcm16(255, 256, 257, -255, -256, -257), srcBits: 16, dstBits: 8,
			// 255 >> 8 is 0 and -255 >> 8 is -1: no rounding, no dither.
			want: pcm8(128, 129, 129, 127, 127, 126),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := convertTo(t, tt.src, wav.SampleFormatPCM, tt.srcBits, tt.dstBits)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("Convert(%d -> %d) produced % x, want % x", tt.srcBits, tt.dstBits, got, tt.want)
			}
		})
	}
}

// TestPack24Bit covers the three-byte packed representation on its own: sign
// extension out of 24 bits, and an exact byte round trip back in.
func TestPack24Bit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		bytes []byte
		want  int64
	}{
		{"zero", []byte{0x00, 0x00, 0x00}, 0},
		{"one", []byte{0x01, 0x00, 0x00}, 1},
		{"all ones is minus one", []byte{0xFF, 0xFF, 0xFF}, -1},
		{"sign bit only is the minimum", []byte{0x00, 0x00, 0x80}, -8388608},
		{"sign bit clear is the maximum", []byte{0xFF, 0xFF, 0x7F}, 8388607},
		{"little endian ordering", []byte{0x56, 0x34, 0x12}, 0x123456},
		{"negative mid range", []byte{0x00, 0x00, 0xFF}, -65536},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeInt(tt.bytes, 24); got != tt.want {
				t.Fatalf("decodeInt(% x, 24) = %d, want %d", tt.bytes, got, tt.want)
			}
			var packed [3]byte
			encodeInt(packed[:], tt.want, 24)
			if !bytes.Equal(packed[:], tt.bytes) {
				t.Fatalf("encodeInt(%d, 24) = % x, want % x", tt.want, packed[:], tt.bytes)
			}
		})
	}
}

// TestPack24BitRoundTripsEveryDecade walks the full 24-bit range at a coarse
// stride and asserts pack then unpack is the identity.
func TestPack24BitRoundTripsEveryDecade(t *testing.T) {
	t.Parallel()
	var packed [3]byte
	for v := int64(-8388608); v <= 8388607; v += 4093 {
		encodeInt(packed[:], v, 24)
		if got := decodeInt(packed[:], 24); got != v {
			t.Fatalf("24-bit round trip of %d produced %d (bytes % x)", v, got, packed[:])
		}
	}
}

// TestConvertFloatClamps checks that float input at and beyond full scale lands
// exactly on the representable limits at every target depth. The positive limit
// is one below full scale, which is the detail a naive implementation gets
// wrong by wrapping +1.0 to the negative minimum.
func TestConvertFloatClamps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value float64
		want  string // wantMax or wantMin
	}{
		{"exactly positive full scale", 1.0, wantMax},
		{"exactly negative full scale", -1.0, wantMin},
		{"one and a half times full scale", 1.5, wantMax},
		{"minus one and a half times full scale", -1.5, wantMin},
		{"twice full scale", 2.0, wantMax},
		{"minus twice full scale", -2.0, wantMin},
		{"far beyond full scale", 1e30, wantMax},
		{"far below negative full scale", -1e30, wantMin},
	}
	for _, dstBits := range []int{8, 16, 24, 32} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s to %d bit", tt.name, dstBits), func(t *testing.T) {
				t.Parallel()
				want := limits[dstBits].max
				if tt.want == wantMin {
					want = limits[dstBits].min
				}
				for _, srcBits := range []int{32, 64} {
					src := f64(tt.value)
					if srcBits == 32 {
						src = f32(float32(tt.value))
					}
					got := decodeAll(t, convertTo(t, src, wav.SampleFormatFloat, srcBits, dstBits), dstBits)
					if len(got) != 1 || got[0] != want {
						t.Fatalf("float%d %g to %d-bit produced %v, want [%d]", srcBits, tt.value, dstBits, got, want)
					}
				}
			})
		}
	}
}

// TestConvertFloatNonFinite pins NaN to silence and the infinities to the
// clamp limits, so a corrupt float sample can never become a wrapped or
// undefined integer.
func TestConvertFloatNonFinite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value float64
		want  string // wantZero, wantMax or wantMin
	}{
		{"not a number", math.NaN(), wantZero},
		{"positive infinity", math.Inf(1), wantMax},
		{"negative infinity", math.Inf(-1), wantMin},
	}
	for _, dstBits := range []int{8, 16, 24, 32} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s to %d bit", tt.name, dstBits), func(t *testing.T) {
				t.Parallel()
				var want int64
				switch tt.want {
				case wantMax:
					want = limits[dstBits].max
				case wantMin:
					want = limits[dstBits].min
				}
				for _, srcBits := range []int{32, 64} {
					src := f64(tt.value)
					if srcBits == 32 {
						src = f32(float32(tt.value))
					}
					got := decodeAll(t, convertTo(t, src, wav.SampleFormatFloat, srcBits, dstBits), dstBits)
					if len(got) != 1 || got[0] != want {
						t.Fatalf("float%d %v to %d-bit produced %v, want [%d]", srcBits, tt.value, dstBits, got, want)
					}
				}
			})
		}
	}
}

// TestConvertFloatRoundsHalfAwayFromZero checks the tie-breaking rule in both
// directions. The inputs are exact powers of two divided by full scale, so no
// representation error can blur the tie.
func TestConvertFloatRoundsHalfAwayFromZero(t *testing.T) {
	t.Parallel()
	const dstBits = 16
	const fullScale = 32768.0
	tests := []struct {
		name   string
		scaled float64 // the value in destination sample units
		want   int64
	}{
		{"positive half rounds up", 0.5, 1},
		{"negative half rounds down", -0.5, -1},
		{"positive one and a half rounds up", 1.5, 2},
		{"negative one and a half rounds down", -1.5, -2},
		{"positive two and a half rounds up", 2.5, 3},
		{"negative two and a half rounds down", -2.5, -3},
		{"positive quarter rounds to zero", 0.25, 0},
		{"negative quarter rounds to zero", -0.25, 0},
		{"exact integer is unchanged", 7.0, 7},
		{"silence is zero", 0.0, 0},
		{"negative zero is zero", math.Copysign(0, -1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := tt.scaled / fullScale
			for _, srcBits := range []int{32, 64} {
				src := f64(value)
				if srcBits == 32 {
					src = f32(float32(value))
				}
				got := decodeAll(t, convertTo(t, src, wav.SampleFormatFloat, srcBits, dstBits), dstBits)
				if len(got) != 1 || got[0] != tt.want {
					t.Fatalf("float%d %g (scaled %g) produced %v, want [%d]", srcBits, value, tt.scaled, got, tt.want)
				}
			}
		})
	}
}

// TestConvertFloatMultipleSamples checks that the per-sample stride is right
// for both float widths and every target depth, which a single-sample test
// cannot catch.
func TestConvertFloatMultipleSamples(t *testing.T) {
	t.Parallel()
	values := []float64{0, 0.5, -0.5, 1.0, -1.0, 0.25}
	want := map[int][]int64{
		8:  {0, 64, -64, 127, -128, 32},
		16: {0, 16384, -16384, 32767, -32768, 8192},
		24: {0, 4194304, -4194304, 8388607, -8388608, 2097152},
		32: {0, 1073741824, -1073741824, 2147483647, -2147483648, 536870912},
	}
	for dstBits, expect := range want {
		t.Run(wavDepthName(dstBits), func(t *testing.T) {
			t.Parallel()
			got := decodeAll(t, convertTo(t, f64(values...), wav.SampleFormatFloat, 64, dstBits), dstBits)
			if len(got) != len(expect) {
				t.Fatalf("float64 to %d-bit produced %d samples, want %d", dstBits, len(got), len(expect))
			}
			for i := range expect {
				if got[i] != expect[i] {
					t.Fatalf("float64 %g to %d-bit produced %d, want %d", values[i], dstBits, got[i], expect[i])
				}
			}
			f32got := decodeAll(t, convertTo(t, f32(0, 0.5, -0.5, 1.0, -1.0, 0.25), wav.SampleFormatFloat, 32, dstBits), dstBits)
			for i := range expect {
				if f32got[i] != expect[i] {
					t.Fatalf("float32 %g to %d-bit produced %d, want %d", values[i], dstBits, f32got[i], expect[i])
				}
			}
		})
	}
}

// wavDepthName names a subtest after its bit depth.
func wavDepthName(bits int) string {
	switch bits {
	case 8:
		return "8 bit"
	case 16:
		return "16 bit"
	case 24:
		return "24 bit"
	default:
		return "32 bit"
	}
}

// TestConvertShortOrMisalignedSource checks that a source that is not a whole
// number of samples converts what it can, writes nothing past that, and never
// panics.
func TestConvertShortOrMisalignedSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		src              []byte
		format           wav.SampleFormat
		srcBits, dstBits int
		wantN            int
	}{
		{"empty source", nil, wav.SampleFormatPCM, 16, 8, 0},
		{"single byte of a 16 bit sample", []byte{0x01}, wav.SampleFormatPCM, 16, 8, 0},
		{"two bytes of a 24 bit sample", []byte{0x01, 0x02}, wav.SampleFormatPCM, 24, 16, 0},
		{"two and a half 16 bit samples", []byte{1, 2, 3, 4, 5}, wav.SampleFormatPCM, 16, 16, 4},
		{"one and two thirds 24 bit samples", []byte{1, 2, 3, 4, 5}, wav.SampleFormatPCM, 24, 32, 4},
		{"seven bytes of float32", make([]byte, 7), wav.SampleFormatFloat, 32, 16, 2},
		{"nine bytes of float32", make([]byte, 9), wav.SampleFormatFloat, 32, 24, 6},
		{"fifteen bytes of float64", make([]byte, 15), wav.SampleFormatFloat, 64, 16, 2},
		{"seventeen bytes of float64", make([]byte, 17), wav.SampleFormatFloat, 64, 8, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const canary = 0xAA
			dst := bytes.Repeat([]byte{canary}, 32)
			n, err := Convert(dst, tt.src, tt.format, tt.srcBits, tt.dstBits)
			if err != nil {
				t.Fatalf("Convert returned error: %v", err)
			}
			if n != tt.wantN {
				t.Fatalf("Convert wrote %d bytes, want %d", n, tt.wantN)
			}
			if n != ConvertedLen(len(tt.src), tt.srcBits, tt.dstBits) {
				t.Fatalf("Convert wrote %d bytes, disagreeing with ConvertedLen", n)
			}
			for i := n; i < len(dst); i++ {
				if dst[i] != canary {
					t.Fatalf("Convert wrote past its reported length at index %d", i)
				}
			}
		})
	}
}

// TestConvertShortDestination checks that an undersized destination is an
// error rather than a partial write or a panic.
func TestConvertShortDestination(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		src              []byte
		format           wav.SampleFormat
		srcBits, dstBits int
		dstLen           int
	}{
		{"identity with one byte missing", pcm16(1, 2), wav.SampleFormatPCM, 16, 16, 3},
		{"widening into a same sized buffer", pcm16(1, 2), wav.SampleFormatPCM, 16, 32, 4},
		{"narrowing into an empty buffer", pcm32(1, 2), wav.SampleFormatPCM, 32, 16, 0},
		{"24 bit destination one byte short", pcm32(1, 2), wav.SampleFormatPCM, 32, 24, 5},
		{"float into a short buffer", f32(0.5, 0.5), wav.SampleFormatFloat, 32, 16, 3},
		{"float64 into a short buffer", f64(0.5), wav.SampleFormatFloat, 64, 32, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const canary = 0x5A
			dst := bytes.Repeat([]byte{canary}, tt.dstLen)
			n, err := Convert(dst, tt.src, tt.format, tt.srcBits, tt.dstBits)
			if !errors.Is(err, io.ErrShortBuffer) {
				t.Fatalf("Convert with a short destination = %v, want an error wrapping io.ErrShortBuffer", err)
			}
			if n != 0 {
				t.Fatalf("Convert with a short destination wrote %d bytes, want 0", n)
			}
			for i := range dst {
				if dst[i] != canary {
					t.Fatalf("Convert with a short destination modified index %d", i)
				}
			}
		})
	}
}

// TestConvertRejectsUnsupported checks that every unsupported combination is
// refused before any byte is written.
func TestConvertRejectsUnsupported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		format           wav.SampleFormat
		srcBits, dstBits int
	}{
		{"pcm source at 12 bits", wav.SampleFormatPCM, 12, 16},
		{"pcm source at 64 bits", wav.SampleFormatPCM, 64, 16},
		{"float source at 16 bits", wav.SampleFormatFloat, 16, 16},
		{"float destination at 32 bits is still integer only", wav.SampleFormatFloat, 32, 64},
		{"destination at 12 bits", wav.SampleFormatPCM, 16, 12},
		{"destination at zero bits", wav.SampleFormatPCM, 16, 0},
		{"unknown source format", wav.SampleFormat(9), 16, 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, 64)
			n, err := Convert(dst, make([]byte, 64), tt.format, tt.srcBits, tt.dstBits)
			if !errors.Is(err, wav.ErrUnsupported) {
				t.Fatalf("Convert = %v, want an error wrapping wav.ErrUnsupported", err)
			}
			if n != 0 {
				t.Fatalf("Convert wrote %d bytes on a rejected call, want 0", n)
			}
		})
	}
}

// TestConvertDoesNotAllocate pins the no-allocation guarantee, which the
// streaming encoder depends on to keep a per-write conversion off the heap.
func TestConvertDoesNotAllocate(t *testing.T) {
	src := f32(make([]float32, 512)...)
	dst := make([]byte, ConvertedLen(len(src), 32, 24))
	pcm := pcm16(make([]int16, 512)...)
	wide := make([]byte, ConvertedLen(len(pcm), 16, 32))
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := Convert(dst, src, wav.SampleFormatFloat, 32, 24); err != nil {
			t.Fatalf("Convert returned error: %v", err)
		}
		if _, err := Convert(wide, pcm, wav.SampleFormatPCM, 16, 32); err != nil {
			t.Fatalf("Convert returned error: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("Convert allocated %.1f times per run, want 0", allocs)
	}
}

// FuzzConvert feeds arbitrary bytes through every format and depth
// combination, valid or not, and asserts that Convert never panics, never
// reports more than the destination holds, and never touches a byte past the
// count it reports.
func FuzzConvert(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x03, 0x04}, uint16(16), uint8(0), uint8(1), uint8(3))
	f.Add([]byte{0xFF, 0xFF, 0xFF}, uint16(8), uint8(0), uint8(2), uint8(1))
	f.Add([]byte{0x00, 0x00, 0x80, 0x3F}, uint16(4), uint8(1), uint8(3), uint8(1))
	f.Add([]byte{}, uint16(0), uint8(0), uint8(0), uint8(0))
	f.Add(bytes.Repeat([]byte{0x7F}, 33), uint16(64), uint8(1), uint8(4), uint8(3))

	// The depth table deliberately mixes supported and unsupported values so
	// the rejection paths get fuzzed alongside the conversion kernels.
	depths := []int{-8, 0, 1, 7, 8, 12, 16, 20, 24, 32, 64, 128}

	f.Fuzz(func(t *testing.T, src []byte, dstLen uint16, formatSel, srcSel, dstSel uint8) {
		format := wav.SampleFormatPCM
		switch formatSel % 3 {
		case 1:
			format = wav.SampleFormatFloat
		case 2:
			format = wav.SampleFormat(int(formatSel)) // an out-of-range format value
		}
		srcBits := depths[int(srcSel)%len(depths)]
		dstBits := depths[int(dstSel)%len(depths)]

		const canary = 0xC3
		dst := bytes.Repeat([]byte{canary}, int(dstLen%4096))
		n, err := Convert(dst, src, format, srcBits, dstBits)

		if n < 0 || n > len(dst) {
			t.Fatalf("Convert reported %d bytes for a %d byte destination", n, len(dst))
		}
		if err != nil && n != 0 {
			t.Fatalf("Convert reported %d bytes alongside error %v", n, err)
		}
		for i := n; i < len(dst); i++ {
			if dst[i] != canary {
				t.Fatalf("Convert wrote past its reported length at index %d (n=%d)", i, n)
			}
		}
		if err == nil {
			if want := ConvertedLen(len(src), srcBits, dstBits); n != want {
				t.Fatalf("Convert wrote %d bytes, ConvertedLen says %d", n, want)
			}
			if err := Validate(format, srcBits); err != nil {
				t.Fatalf("Convert accepted an unsupported source: %v", err)
			}
			if err := Validate(wav.SampleFormatPCM, dstBits); err != nil {
				t.Fatalf("Convert accepted an unsupported destination: %v", err)
			}
		}
	})
}
