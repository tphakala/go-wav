package sample

import (
	"encoding/binary"
	"math"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// benchFrames is one second of 48 kHz mono, a realistic per-call chunk.
const benchFrames = 48000

// benchFramesLong is ten seconds of the same, which puts both buffers well past
// the last-level cache a one-second chunk still fits in. The integer pairs are
// measured at both sizes because a conversion that only wins while everything
// is cache-resident has not won anything worth having.
const benchFramesLong = 10 * benchFrames

func benchConvert(b *testing.B, format wav.SampleFormat, srcBits, dstBits int) {
	b.Helper()
	benchConvertN(b, format, srcBits, dstBits, benchFrames)
}

func benchConvertN(b *testing.B, format wav.SampleFormat, srcBits, dstBits, frames int) {
	b.Helper()
	srcWidth := bytesPerSample(srcBits)
	src := make([]byte, frames*srcWidth)
	if format == wav.SampleFormatFloat {
		// Fill with audio-like samples inside full scale. A byte pattern
		// reinterpreted as float produces a near-uniform exponent, so about
		// half the samples land past full scale and return from the clamp
		// before reaching the rounding path, which is the path these
		// benchmarks exist to measure.
		for i := range frames {
			v := math.Sin(2 * math.Pi * 440 * float64(i) / float64(frames))
			if srcBits == 32 {
				binary.LittleEndian.PutUint32(src[i*4:], math.Float32bits(float32(v)))
			} else {
				binary.LittleEndian.PutUint64(src[i*8:], math.Float64bits(v))
			}
		}
	} else {
		for i := range src {
			src[i] = byte(i * 7)
		}
	}
	dst := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
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

func BenchmarkConvertWiden8to16Long(b *testing.B) {
	benchConvertN(b, wav.SampleFormatPCM, 8, 16, benchFramesLong)
}

func BenchmarkConvertWiden16to32Long(b *testing.B) {
	benchConvertN(b, wav.SampleFormatPCM, 16, 32, benchFramesLong)
}

func BenchmarkConvertNarrow24to16Long(b *testing.B) {
	benchConvertN(b, wav.SampleFormatPCM, 24, 16, benchFramesLong)
}

func BenchmarkConvertWiden24to32Long(b *testing.B) {
	benchConvertN(b, wav.SampleFormatPCM, 24, 32, benchFramesLong)
}

// The four pairs above reach a single-pass kernel. These two do not: 8 to 32
// and 32 to 16 fall through the dispatcher to convertIntToIntBlocked, so they
// keep the eight non-kernel pairs under measurement through the public entry
// point, where a regression in the fallback would otherwise go unseen.
func BenchmarkConvertWiden8to32(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 8, 32)
}

func BenchmarkConvertNarrow32to16(b *testing.B) {
	benchConvert(b, wav.SampleFormatPCM, 32, 16)
}

// The BenchmarkKernel and BenchmarkBlocked families below are the A/B pair the
// "30 to 54 percent" claim in convertIntToInt's doc comment rests on; without
// them the comparison could not be re-derived from this repository at all. Both
// enter at the same level, calling their conversion function directly on
// identical buffers at identical sizes, so neither side pays the two Validate
// calls, the ConvertedLen, the short-buffer check, the two reslices and the
// dispatcher switch that Convert does first. Both also reach their conversion
// through the same one indirect call per b.Loop() iteration, which covers tens
// of thousands of samples, so what is left between them is the loop.
//
// The BenchmarkConvert functions above are a different measurement and not half
// of this pair: they time the public entry point, which is what a caller
// actually pays and what would catch a regression in the dispatch itself.
//
// Reading the A/B comparison off `go test -bench=... -count=N` does not work.
// The testing package runs each benchmark N times CONSECUTIVELY, all N runs of
// the first before the first run of the second, so the two functions are
// measured in separate blocks of time rather than interleaved. Anything that
// drifts between the blocks (clock frequency stepping down as the machine
// heats, the scheduler moving the process to another core, another process
// arriving) is then folded straight into the difference. A measurement of this
// exact code was distorted that way. A trustworthy figure needs repeated
// single-count runs that alternate which of the two goes first, pinned to one
// core, with a significance test over the resulting pairs.
func benchDirect(b *testing.B, convert func(dst, src []byte), srcBits, dstBits, frames int) {
	b.Helper()
	src := make([]byte, frames*bytesPerSample(srcBits))
	for i := range src {
		src[i] = byte(i * 7)
	}
	dst := make([]byte, ConvertedLen(len(src), srcBits, dstBits))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		convert(dst, src)
	}
}

// benchBlockedN is the fallback half of that pair, bound to one width pair.
func benchBlockedN(b *testing.B, srcBits, dstBits, frames int) {
	b.Helper()
	benchDirect(b, func(dst, src []byte) {
		convertIntToIntBlocked(dst, src, srcBits, dstBits)
	}, srcBits, dstBits, frames)
}

func BenchmarkKernelWiden8to16(b *testing.B) {
	benchDirect(b, convert8to16, 8, 16, benchFrames)
}

func BenchmarkKernelWiden16to32(b *testing.B) {
	benchDirect(b, convert16to32, 16, 32, benchFrames)
}

func BenchmarkKernelNarrow24to16(b *testing.B) {
	benchDirect(b, convert24to16, 24, 16, benchFrames)
}

func BenchmarkKernelWiden24to32(b *testing.B) {
	benchDirect(b, convert24to32, 24, 32, benchFrames)
}

func BenchmarkKernelWiden8to16Long(b *testing.B) {
	benchDirect(b, convert8to16, 8, 16, benchFramesLong)
}

func BenchmarkKernelWiden16to32Long(b *testing.B) {
	benchDirect(b, convert16to32, 16, 32, benchFramesLong)
}

func BenchmarkKernelNarrow24to16Long(b *testing.B) {
	benchDirect(b, convert24to16, 24, 16, benchFramesLong)
}

func BenchmarkKernelWiden24to32Long(b *testing.B) {
	benchDirect(b, convert24to32, 24, 32, benchFramesLong)
}

func BenchmarkBlockedWiden8to16(b *testing.B) {
	benchBlockedN(b, 8, 16, benchFrames)
}

func BenchmarkBlockedWiden16to32(b *testing.B) {
	benchBlockedN(b, 16, 32, benchFrames)
}

func BenchmarkBlockedNarrow24to16(b *testing.B) {
	benchBlockedN(b, 24, 16, benchFrames)
}

func BenchmarkBlockedWiden24to32(b *testing.B) {
	benchBlockedN(b, 24, 32, benchFrames)
}

func BenchmarkBlockedWiden8to16Long(b *testing.B) {
	benchBlockedN(b, 8, 16, benchFramesLong)
}

func BenchmarkBlockedWiden16to32Long(b *testing.B) {
	benchBlockedN(b, 16, 32, benchFramesLong)
}

func BenchmarkBlockedNarrow24to16Long(b *testing.B) {
	benchBlockedN(b, 24, 16, benchFramesLong)
}

func BenchmarkBlockedWiden24to32Long(b *testing.B) {
	benchBlockedN(b, 24, 32, benchFramesLong)
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
