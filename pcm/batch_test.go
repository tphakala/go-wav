package pcm

import (
	"math"
	"testing"
)

// srcFrames and dstFrames are stored and requested frame widths the converting
// decoder can be asked for: mono and multichannel frames of 8 to 64 bit
// samples (1, 2, 3, 4, 6, 8 and 18 bytes), and the widest a frame can be, 65535
// channels of 64 bit samples (524280 bytes) stored and of 32 bit samples
// (262140 bytes) requested.
var (
	srcFrames = []int{1, 2, 3, 4, 6, 8, 18, 524280}
	dstFrames = []int{1, 2, 3, 4, 6, 262140}
)

// TestConvertBatchLenIsBounded pins the property that removes the wrap: the
// staged batch never exceeds maxConvertBatch (or one frame, when a frame is
// wider than that), however large a buffer the
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

	for _, srcFrame := range srcFrames {
		for _, dstFrame := range dstFrames {
			for _, bufLen := range bufLens {
				for _, remaining := range remainings {
					got := convertBatchLen(bufLen, srcFrame, dstFrame, remaining)
					if got < 0 {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want a non-negative count",
							bufLen, srcFrame, dstFrame, remaining, got)
					}
					if limit := max(maxConvertBatch, srcFrame); got > limit {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at most the %d byte batch cap",
							bufLen, srcFrame, dstFrame, remaining, got, limit)
					}
					if got%srcFrame != 0 {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want a whole number of %d byte frames",
							bufLen, srcFrame, dstFrame, remaining, got, srcFrame)
					}
					if remaining >= 0 && int64(got) > remaining {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at most the %d bytes left in the chunk",
							bufLen, srcFrame, dstFrame, remaining, got, remaining)
					}
				}
			}
		}
	}
}

// TestConvertBatchLenMakesProgress covers the reason the batch is never zero
// for a chunk that still holds a whole frame: a Read that stages nothing
// reports io.EOF, so a zero here would end a stream early. That is what the
// 512 MiB case did on a 32-bit target, and it is the silent half of the
// defect. The widest frame, wider than the cap, must still stage one whole
// frame: losing the floor would report a premature EOF for it.
func TestConvertBatchLenMakesProgress(t *testing.T) {
	t.Parallel()

	bufLens := []int{1, 7, 4096, 256 << 20, 512 << 20, math.MaxInt}

	for _, srcFrame := range srcFrames {
		for _, dstFrame := range dstFrames {
			for _, bufLen := range bufLens {
				// Unknown length, and a length long enough to hold one
				// frame, are the two cases where a batch must be staged.
				for _, remaining := range []int64{-1, int64(srcFrame), math.MaxInt64} {
					got := convertBatchLen(bufLen, srcFrame, dstFrame, remaining)
					if got < srcFrame {
						t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want at least one %d byte frame",
							bufLen, srcFrame, dstFrame, remaining, got, srcFrame)
					}
				}
			}
		}
	}
}

// TestConvertBatchLenClampsToRemaining covers the end of the data chunk, where
// the batch is bounded by what the chunk still holds rather than by the
// caller's buffer, and a trailing fragment shorter than one frame is dropped
// because nothing whole can be converted from it.
func TestConvertBatchLenClampsToRemaining(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		bufLen    int
		srcFrame  int
		dstFrame  int
		remaining int64
		want      int
	}{
		{"exhausted", 4096, 2, 2, 0, 0},
		{"fragment shorter than a frame", 4096, 2, 2, 1, 0},
		// The widths are rejected by the caller before it gets here, but the
		// division would panic without the guard, and the test enters the
		// function directly.
		{"zero source frame width", 4096, 0, 2, -1, 0},
		{"zero destination frame width", 4096, 2, 0, -1, 0},
		{"negative source frame width", 4096, -1, 2, -1, 0},
		{"negative destination frame width", 4096, 2, -1, -1, 0},
		// A caller cannot pass a negative len(p), but the function takes a
		// length rather than a slice, so nothing in its signature says so.
		{"empty buffer still stages one frame", 0, 2, 2, -1, 2},
		{"negative buffer still stages one frame", -1, 2, 2, -1, 2},
		{"fragment after a whole frame", 4096, 2, 2, 3, 2},
		{"chunk shorter than the buffer", 4096, 4, 2, 40, 40},
		{"buffer shorter than the chunk", 8, 2, 2, 4096, 8},
		{"unknown length follows the buffer", 4096, 2, 2, -1, 4096},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := convertBatchLen(tc.bufLen, tc.srcFrame, tc.dstFrame, tc.remaining)
			if got != tc.want {
				t.Errorf("convertBatchLen(%d, %d, %d, %d) = %d, want %d",
					tc.bufLen, tc.srcFrame, tc.dstFrame, tc.remaining, got, tc.want)
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

	for _, srcFrame := range srcFrames {
		for _, dstFrame := range dstFrames {
			bufLen := 4096
			want := (bufLen / dstFrame) * srcFrame
			// A buffer smaller than one converted frame (want 0) stages a
			// whole frame regardless; the progress test covers that.
			if want == 0 || want > maxConvertBatch {
				continue
			}
			got := convertBatchLen(bufLen, srcFrame, dstFrame, -1)
			if got != want {
				t.Errorf("convertBatchLen(%d, %d, %d, -1) = %d, want %d",
					bufLen, srcFrame, dstFrame, got, want)
			}
		}
	}
}
