package pcm_test

import (
	"bytes"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// TestConvertingReadCapsTheBatch covers the caller-facing half of the batch
// cap. A converting Read used to size its staging buffer from the caller's
// buffer, so asking for a large block made the decoder allocate a multiple of
// it to hold the source. It now stages a bounded batch and returns a short
// read, which io.Reader permits.
//
// The narrowing here is the worst case the package supports: eight source
// bytes per sample converted to one, so an uncapped batch would stage eight
// times whatever the caller asked for.
//
// Both sizes below are load-bearing, and getting either wrong makes the test
// vacuous rather than failing. The stream must hold more converted bytes than
// one capped batch yields, or the batch is bounded by the end of the data
// chunk and the cap is never the operative limit. The caller's buffer must be
// large enough that the uncapped decoder would have filled it, or the
// assertion cannot fail whatever the decoder does. Verified by deleting the
// cap: this test then reports 65536 bytes from one Read instead of 8192.
func TestConvertingReadCapsTheBatch(t *testing.T) {
	t.Parallel()

	const (
		// One batch is 64 KiB of source, which at eight bytes per sample is
		// 8192 samples and so 8192 converted bytes.
		wantFirstRead = 8192
		callerBuffer  = 64 << 10
	)

	cfg := pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat}
	// Eight capped batches' worth, so the first read is nowhere near the end.
	file := encodeFixture(t, cfg, pattern(8*callerBuffer))

	d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(8))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	buf := make([]byte, callerBuffer)
	n, err := d.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != wantFirstRead {
		t.Fatalf("Read returned %d bytes, want the %d one bounded batch yields; "+
			"filling all %d would mean the batch was sized from the caller's buffer",
			n, wantFirstRead, len(buf))
	}
}

// TestConvertingReadIsWholeAcrossBufferSizes pins that the cap costs no data:
// draining the same stream through buffers on either side of the cap must
// yield identical bytes, whatever the direction of the conversion.
func TestConvertingReadIsWholeAcrossBufferSizes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cfg     pcm.Config
		convert int
		payload []byte
	}{
		// Every payload is comfortably more than one 64 KiB batch of source,
		// so each drain crosses several capped batches. A payload below the
		// cap would be bounded by the end of the data chunk instead, and the
		// cap would never be the operative limit in any case.
		{
			name:    "float64 narrowed to 8 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat},
			convert: 8,
			payload: pattern(8 * 50000),
		},
		{
			name:    "8 bit widened to 32 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 2, Format: wav.SampleFormatPCM},
			convert: 32,
			payload: pattern(400000),
		},
		{
			name:    "24 bit narrowed to 16 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2, Format: wav.SampleFormatPCM},
			convert: 16,
			payload: pattern(3 * 2 * 77777),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := encodeFixture(t, tc.cfg, tc.payload)
			want := readAllConverted(t, file, tc.convert)

			// Sizes on both sides of the cap, from a buffer smaller than one
			// converted sample up to one far larger than any batch, so the
			// bound is exercised whichever of the two is the operative one.
			for _, size := range []int{1, 4096, 64 << 10, 1 << 20} {
				got := drainInto(t, file, tc.convert, size)
				if !bytes.Equal(got, want) {
					t.Errorf("%d byte reads yielded %d bytes, want the %d bytes ReadAll yields",
						size, len(got), len(want))
				}
			}
		})
	}
}
