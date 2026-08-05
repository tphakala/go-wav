package pcm

import (
	"testing"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/sample"
)

// TestConfigBytesPerSampleRoutesThroughCanonical pins that the encoder's
// per-sample width is the one canonical mapping in internal/sample, not a second
// inline copy of (bits+7)/8. If the two ever disagreed, an unsupported depth
// would size an encoder buffer one way and the conversion path another; routing
// both through the same function is what keeps that from happening.
func TestConfigBytesPerSampleRoutesThroughCanonical(t *testing.T) {
	for bits := -8; bits <= 72; bits++ {
		want := int64(sample.BytesPerSample(bits))
		if got := (Config{BitDepth: bits}).bytesPerSample(); got != want {
			t.Errorf("Config{BitDepth: %d}.bytesPerSample() = %d, want %d", bits, got, want)
		}
	}

	// The sweep above compares the two functions against each other, so a value
	// regression present in BOTH would slip past. Pin a few widths against
	// independent literals as well, including the sub-byte and oversized depths
	// where the routing has to answer 0, so this test alone catches a regression
	// in either Config.bytesPerSample or sample.BytesPerSample.
	literals := map[int]int64{8: 1, 16: 2, 24: 3, 32: 4, 64: 8, 12: 0, 20: 0, 48: 0}
	for bits, want := range literals {
		if got := (Config{BitDepth: bits}).bytesPerSample(); got != want {
			t.Errorf("Config{BitDepth: %d}.bytesPerSample() = %d, want %d (literal)", bits, got, want)
		}
	}
}

// TestOnDiskWidthAndConversionWidthDifferDeliberately guards the one place the
// two width questions are allowed to disagree. StreamInfo.BytesPerSample reports
// the on-disk footprint and rounds a sub-byte depth up to a whole byte; the
// conversion path's sample.BytesPerSample reports a width its kernels can store
// and answers 0 for a depth they cannot. A 20-bit sample is the case that tells
// them apart. If a future change folds one into the other, this fails.
func TestOnDiskWidthAndConversionWidthDifferDeliberately(t *testing.T) {
	if got := (wav.StreamInfo{BitDepth: 20}).BytesPerSample(); got != 3 {
		t.Errorf("StreamInfo{BitDepth: 20}.BytesPerSample() = %d, want 3 (on-disk width rounds up)", got)
	}
	if got := sample.BytesPerSample(20); got != 0 {
		t.Errorf("sample.BytesPerSample(20) = %d, want 0 (unsupported conversion width)", got)
	}
	// On every width the kernels do store, the two agree, so the divergence is
	// confined to the depths a real stream never carries as BitDepth.
	for _, bits := range []int{8, 16, 24, 32, 64} {
		onDisk := (wav.StreamInfo{BitDepth: bits}).BytesPerSample()
		conv := sample.BytesPerSample(bits)
		if onDisk != conv {
			t.Errorf("bit depth %d: on-disk width %d != conversion width %d, expected agreement on supported depths", bits, onDisk, conv)
		}
	}
}
