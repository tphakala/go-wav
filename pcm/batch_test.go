package pcm

import (
	"math"
	"testing"
)

// srcWidths and dstWidths are every stored and requested sample width the
// converting decoder can be asked for: 8, 16, 24, 32 and 64 bit sources, and
// 8, 16, 24 and 32 bit destinations.
var (
	srcWidths = []int{1, 2, 3, 4, 8}
	dstWidths = []int{1, 2, 3, 4}
)

// TestConvertBatchLenIsBounded pins the property that removes the wrap: the
// staged batch never exceeds maxConvertBatch, however large a buffer the
// caller passes. The multiplication that sizes it is what used to overflow on
// a 32-bit target, so the sizes here include the two that were shown to wrap
// there and the largest length any slice can have.
func TestConvertBatchLenIsBounded(t *testing.T) {
	t.Parallel()

	bufLens := []int{
		1, 7, 4096, 64 << 10,
		255 << 20, // last size that did not wrap on a 32-bit target
		256 << 20, // wrapped negative, then panicked slicing srcBuf
		512 << 20, // wrapped to exactly 0, reported a clean EOF mid-file
		1 << 30,
		math.MaxInt,
	}
	remainings := []int64{-1, 0, 1, 3, 64 << 10, math.MaxInt64}

	for _, srcWidth := range srcWidths {
		for _, dstWidth := range dstWidths {
			for _, bufLen := range bufLens {
				for _, remaining := range remainings {
					got := convertBatchLen(bufLen, srcWidth, dstWidth, remaining)
					if got < 0 {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want a non-negative count",
							bufLen, srcWidth, dstWidth, remaining, got)
					}
					if got > maxConvertBatch {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at most the %d byte batch cap",
							bufLen, srcWidth, dstWidth, remaining, got, maxConvertBatch)
					}
					if got%srcWidth != 0 {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want a whole number of %d byte samples",
							bufLen, srcWidth, dstWidth, remaining, got, srcWidth)
					}
					if remaining >= 0 && int64(got) > remaining {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at most the %d bytes left in the chunk",
							bufLen, srcWidth, dstWidth, remaining, got, remaining)
					}
				}
			}
		}
	}
}

// TestConvertBatchLenMakesProgress covers the reason the batch is never zero
// for a chunk that still holds a whole sample: a Read that stages nothing
// reports io.EOF, so a zero here would end a stream early. That is what the
// 512 MiB case did on a 32-bit target, and it is the silent half of the
// defect.
func TestConvertBatchLenMakesProgress(t *testing.T) {
	t.Parallel()

	bufLens := []int{1, 7, 4096, 256 << 20, 512 << 20, math.MaxInt}

	for _, srcWidth := range srcWidths {
		for _, dstWidth := range dstWidths {
			for _, bufLen := range bufLens {
				// Unknown length, and a length long enough to hold one
				// sample, are the two cases where a batch must be staged.
				for _, remaining := range []int64{-1, int64(srcWidth), math.MaxInt64} {
					got := convertBatchLen(bufLen, srcWidth, dstWidth, remaining)
					if got < srcWidth {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at least one %d byte sample",
							bufLen, srcWidth, dstWidth, remaining, got, srcWidth)
					}
				}
			}
		}
	}
}

// TestConvertBatchLenClampsToRemaining covers the end of the data chunk, where
// the batch is bounded by what the chunk still holds rather than by the
// caller's buffer, and a trailing fragment shorter than one sample is dropped
// because nothing can be converted from it.
func TestConvertBatchLenClampsToRemaining(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		bufLen    int
		srcWidth  int
		dstWidth  int
		remaining int64
		want      int
	}{
		{"exhausted", 4096, 2, 2, 0, 0},
		{"fragment shorter than a sample", 4096, 2, 2, 1, 0},
		{"fragment after a whole sample", 4096, 2, 2, 3, 2},
		{"chunk shorter than the buffer", 4096, 4, 2, 40, 40},
		{"buffer shorter than the chunk", 8, 2, 2, 4096, 8},
		{"unknown length follows the buffer", 4096, 2, 2, -1, 4096},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := convertBatchLen(tc.bufLen, tc.srcWidth, tc.dstWidth, tc.remaining)
			if got != tc.want {
				t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want %d",
					tc.bufLen, tc.srcWidth, tc.dstWidth, tc.remaining, got, tc.want)
			}
		})
	}
}

// TestConvertBatchLenFillsSmallBuffers pins that the cap costs nothing for the
// buffer sizes callers actually use: anything the cap can serve in one batch
// is still sized from the caller's buffer, not trimmed to some smaller
// internal block.
func TestConvertBatchLenFillsSmallBuffers(t *testing.T) {
	t.Parallel()

	for _, srcWidth := range srcWidths {
		for _, dstWidth := range dstWidths {
			bufLen := 4096
			want := (bufLen / dstWidth) * srcWidth
			if want > maxConvertBatch {
				continue
			}
			got := convertBatchLen(bufLen, srcWidth, dstWidth, -1)
			if got != want {
				t.Errorf("convertBatchLen(%d, %d, %d, -1) = %d, want %d",
					bufLen, srcWidth, dstWidth, got, want)
			}
		}
	}
}
