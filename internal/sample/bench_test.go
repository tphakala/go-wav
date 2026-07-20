package sample

import (
	"testing"

	wav "github.com/tphakala/go-wav"
)

// benchFrames is one second of 48 kHz mono, a realistic per-call chunk.
const benchFrames = 48000

func benchConvert(b *testing.B, format wav.SampleFormat, srcBits, dstBits int) {
	b.Helper()
	srcWidth := bytesPerSample(srcBits)
	src := make([]byte, benchFrames*srcWidth)
	for i := range src {
		src[i] = byte(i * 7)
	}
	dst := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := Convert(dst, src, format, srcBits, dstBits); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertIdentity16(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 16, 16)
}

func BenchmarkConvertWiden8to16(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 8, 16)
}

func BenchmarkConvertWiden16to32(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 16, 32)
}

func BenchmarkConvertNarrow24to16(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 24, 16)
}

func BenchmarkConvertWiden24to32(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 24, 32)
}

func BenchmarkConvertFloat32to16(b *testing.B) {
	benchConvert(b, wav.SampleFormatFloat, 32, 16)
}

func BenchmarkConvertFloat32to32(b *testing.B) {
	benchConvert(b, wav.SampleFormatFloat, 32, 32)
}

func BenchmarkConvertFloat64to32(b *testing.B) {
	benchConvert(b, wav.SampleFormatFloat, 64, 32)
}
