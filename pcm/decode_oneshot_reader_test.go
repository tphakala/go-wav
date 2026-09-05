package pcm_test

import (
	"bytes"
	"errors"
	"testing"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/pcm"
)

// TestDecodeInterleavedReaderMatchesBytes checks that the io.Reader one-shot
// DecodeInterleaved returns the same samples and StreamInfo as the byte-slice
// DecodeInterleavedBytes for the same stream, across a range of formats and with
// a conversion option. The two entry points share a decoder, so a divergence
// would mean one of them is reading the stream differently.
func TestDecodeInterleavedReaderMatchesBytes(t *testing.T) {
	cases := []struct {
		name string
		cfg  pcm.Config
		opts []pcm.Option
	}{
		{"pcm8_mono", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}, nil},
		{"pcm16_mono", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, nil},
		{"pcm24_stereo", pcm.Config{SampleRate: 44100, BitDepth: 24, Channels: 2}, nil},
		{"float32_stereo", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}, nil},
		{"convert_24_to_16", pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1}, []pcm.Option{pcm.WithConvertTo(16)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := encodeFixture(t, tc.cfg, pattern(3000))

			wantSamples, wantInfo, err := pcm.DecodeInterleavedBytes(file, tc.opts...)
			if err != nil {
				t.Fatalf("DecodeInterleavedBytes: %v", err)
			}

			gotSamples, gotInfo, err := pcm.DecodeInterleaved(bytes.NewReader(file), tc.opts...)
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if gotInfo != wantInfo {
				t.Errorf("StreamInfo mismatch\n got %+v\nwant %+v", gotInfo, wantInfo)
			}
			if !bytes.Equal(gotSamples, wantSamples) {
				t.Errorf("samples: reader path returned %d bytes, byte-slice path returned %d",
					len(gotSamples), len(wantSamples))
			}
		})
	}
}

// TestDecodeInterleavedReaderCompanded checks the companded expansion path
// through the io.Reader one-shot: an A-law stream, which the decoder always
// expands to linear 16-bit PCM, must come back the same whether decoded from a
// reader or from the byte slice.
func TestDecodeInterleavedReaderCompanded(t *testing.T) {
	codes := make([]byte, 2000)
	for i := range codes {
		//nolint:gosec // G115: deterministic test payload, truncation is intended.
		codes[i] = byte(i)
	}
	file := compandedFile(t, tagALaw, 1, 8000, codes)

	wantSamples, wantInfo, err := pcm.DecodeInterleavedBytes(file)
	if err != nil {
		t.Fatalf("DecodeInterleavedBytes: %v", err)
	}
	gotSamples, gotInfo, err := pcm.DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if gotInfo.SourceFormat != wav.SampleFormatALaw {
		t.Errorf("SourceFormat = %v, want A-law", gotInfo.SourceFormat)
	}
	if gotInfo != wantInfo {
		t.Errorf("StreamInfo mismatch\n got %+v\nwant %+v", gotInfo, wantInfo)
	}
	if !bytes.Equal(gotSamples, wantSamples) {
		t.Errorf("expanded samples differ: reader %d bytes, byte-slice %d bytes",
			len(gotSamples), len(wantSamples))
	}
	if len(gotSamples) != 2*len(codes) {
		t.Errorf("A-law expansion = %d bytes, want %d (2x the codes)", len(gotSamples), 2*len(codes))
	}
}

// TestDecodeInterleavedLimit checks the byte ceiling: a decode that would exceed
// maxBytes stops with a wrapped ErrDecodeLimit and returns no samples, a
// generous ceiling decodes the whole stream, and a non-positive ceiling removes
// the limit.
func TestDecodeInterleavedLimit(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := pattern(16000) // 16000 bytes of audio
	file := encodeFixture(t, cfg, payload)

	t.Run("exceeded returns wrapped ErrDecodeLimit", func(t *testing.T) {
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), 100)
		if !errors.Is(err, pcm.ErrDecodeLimit) {
			t.Fatalf("err = %v, want ErrDecodeLimit", err)
		}
		if samples != nil {
			t.Errorf("samples = %d bytes on a limited decode, want nil", len(samples))
		}
	})

	t.Run("generous ceiling decodes the whole stream", func(t *testing.T) {
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), len(payload)+1)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit: %v", err)
		}
		if !bytes.Equal(samples, payload) {
			t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
		}
	})

	t.Run("non-positive ceiling is unbounded", func(t *testing.T) {
		for _, max := range []int{0, -1} {
			samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), max)
			if err != nil {
				t.Fatalf("DecodeInterleavedLimit(max=%d): %v", max, err)
			}
			if !bytes.Equal(samples, payload) {
				t.Errorf("max=%d: samples = %d bytes, want %d", max, len(samples), len(payload))
			}
		}
	})

	t.Run("exactly at the ceiling succeeds", func(t *testing.T) {
		// The stream fits in a single WriteTo block, so a ceiling equal to the
		// payload length admits it: the one write does not carry the total past
		// max.
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), len(payload))
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit at exact size: %v", err)
		}
		if !bytes.Equal(samples, payload) {
			t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
		}
	})
}

// TestDecodeInterleavedLimitMultiBlock exercises the cappedWriter accumulation
// path that TestDecodeInterleavedLimit's single-block payloads do not: a payload
// larger than WriteTo's 64 KiB block, so the ceiling is crossed on a later block
// with earlier blocks already accumulated. A regression dropping the running
// total (c.n += n) would still pass the single-block tests but fail here.
func TestDecodeInterleavedLimitMultiBlock(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := pattern(200000) // several 64 KiB WriteTo blocks
	file := encodeFixture(t, cfg, payload)

	t.Run("ceiling crossed on a later block", func(t *testing.T) {
		// Above one 64 KiB block, below two: the first block is admitted, the
		// second crosses the ceiling, so the accumulated total is what trips it.
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), 100000)
		if !errors.Is(err, pcm.ErrDecodeLimit) {
			t.Fatalf("err = %v, want ErrDecodeLimit", err)
		}
		if samples != nil {
			t.Errorf("samples = %d bytes on a limited decode, want nil", len(samples))
		}
	})

	t.Run("generous ceiling accumulates every block", func(t *testing.T) {
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), len(payload)+1)
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit: %v", err)
		}
		if !bytes.Equal(samples, payload) {
			t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
		}
	})
}

// TestDecodeInterleavedReaderIgnoreLength decodes through the reader path with
// WithIgnoreLength, where the header's declared length is discarded so
// StreamInfo.TotalFrames is 0 and presizeHint skips the pre-grow, reading to
// EOF instead. The result must still match the stream's audio.
func TestDecodeInterleavedReaderIgnoreLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := pattern(4096)
	file := encodeFixture(t, cfg, payload)

	samples, info, err := pcm.DecodeInterleaved(bytes.NewReader(file), pcm.WithIgnoreLength())
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if info.TotalFrames != 0 {
		t.Errorf("TotalFrames = %d under WithIgnoreLength, want 0", info.TotalFrames)
	}
	if !bytes.Equal(samples, payload) {
		t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
	}
}

// TestDecodeInterleavedDefaultCeiling checks that the default entry point decodes
// an ordinary stream, which sits far below DefaultMaxDecodedBytes, without
// tripping the limit.
func TestDecodeInterleavedDefaultCeiling(t *testing.T) {
	if pcm.DefaultMaxDecodedBytes != 1<<30 {
		t.Errorf("DefaultMaxDecodedBytes = %d, want %d", pcm.DefaultMaxDecodedBytes, 1<<30)
	}
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}
	payload := pattern(8000)
	file := encodeFixture(t, cfg, payload)

	samples, _, err := pcm.DecodeInterleaved(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if !bytes.Equal(samples, payload) {
		t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
	}
}

// TestDecodeInterleavedReaderRejectsMalformed checks that the reader path
// reports the same failure NewDecoder would for a stream that is not a WAVE
// stream, rather than returning partial data.
func TestDecodeInterleavedReaderRejectsMalformed(t *testing.T) {
	samples, _, err := pcm.DecodeInterleaved(bytes.NewReader([]byte("RIFFnonsenseWAVE")))
	if err == nil {
		t.Fatal("a malformed stream was accepted")
	}
	if samples != nil {
		t.Errorf("samples = %d bytes on a rejected stream, want nil", len(samples))
	}
}

// TestDecodeInterleavedNonSeekableReader checks that the one-shot decode works
// over a reader that offers nothing but Read (nonSeekReader hides the io.Seeker
// and io.ReaderAt that bytes.Reader also implements), matching the byte-slice
// result. This is the pure streaming path a caller passing an arbitrary
// io.Reader, such as a network socket or a pipe, would take.
func TestDecodeInterleavedNonSeekableReader(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}
	payload := pattern(4096)
	file := encodeFixture(t, cfg, payload)

	want, _, err := pcm.DecodeInterleavedBytes(file)
	if err != nil {
		t.Fatalf("DecodeInterleavedBytes: %v", err)
	}
	got, _, err := pcm.DecodeInterleaved(nonSeekReader{r: bytes.NewReader(file)})
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("non-seekable decode: got %d bytes, want %d", len(got), len(want))
	}
}
