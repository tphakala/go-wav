package pcm_test

import (
	"bytes"
	"errors"
	"io"
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
func TestConvertingReadCapsTheBatch(t *testing.T) {
	t.Parallel()

	cfg := pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat}
	file := encodeFixture(t, cfg, pattern(8*4096))

	d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(8))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	// Far larger than any batch the decoder stages, and larger than the whole
	// converted stream, so a decoder sizing its batch from it would both
	// over-allocate and be able to answer in one read.
	buf := make([]byte, 8<<20)
	n, err := d.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n <= 0 {
		t.Fatalf("Read returned %d bytes, want progress", n)
	}
	if n == len(buf) {
		t.Fatalf("Read filled all %d bytes, want a short read from a bounded batch", n)
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
		{
			name:    "float64 narrowed to 8 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat},
			convert: 8,
			payload: pattern(8 * 5000),
		},
		{
			name:    "8 bit widened to 32 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 2, Format: wav.SampleFormatPCM},
			convert: 32,
			payload: pattern(40000),
		},
		{
			name:    "24 bit narrowed to 16 bit",
			cfg:     pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2, Format: wav.SampleFormatPCM},
			convert: 16,
			payload: pattern(3 * 2 * 7777),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := encodeFixture(t, tc.cfg, tc.payload)
			want := readAllConverted(t, file, tc.convert)

			// One below the cap, one at it, and one far above it, so the
			// bounded batch is exercised from both sides.
			for _, size := range []int{1, 3, 4096, 64 << 10, 1 << 20, 8 << 20} {
				got := drainInto(t, file, tc.convert, size)
				if !bytes.Equal(got, want) {
					t.Errorf("%d byte reads yielded %d bytes, want the %d bytes ReadAll yields",
						size, len(got), len(want))
				}
			}
		})
	}
}

// drainInto reads a converted stream to EOF through a fixed buffer size.
func drainInto(tb testing.TB, file []byte, convert, bufSize int) []byte {
	tb.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(convert))
	if err != nil {
		tb.Fatalf("NewDecoder: %v", err)
	}
	buf := make([]byte, bufSize)
	var out []byte
	for {
		n, err := d.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			tb.Fatalf("Read: %v", err)
		}
		if n == 0 {
			tb.Fatal("Read made no progress and reported no error")
		}
	}
}
