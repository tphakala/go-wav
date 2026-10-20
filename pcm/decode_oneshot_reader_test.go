package pcm_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/pcm"
)

// TestDecodeInterleavedReaderMatchesBytes checks that the io.Reader one-shot
// DecodeInterleaved returns the same samples and StreamInfo as the byte-slice
// DecodeInterleavedBytes for the same stream, across a range of formats, with
// a conversion option and with a truncated file. The two entry points share a decoder, so a divergence
// would mean one of them is reading the stream differently.
func TestDecodeInterleavedReaderMatchesBytes(t *testing.T) {
	cases := []struct {
		name string
		cfg  pcm.Config
		opts []pcm.Option
		trim int // bytes cut off the end of the file, so the header overstates the audio
	}{
		{"pcm8_mono", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}, nil, 0},
		{"pcm16_mono", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, nil, 0},
		{"pcm24_stereo", pcm.Config{SampleRate: 44100, BitDepth: 24, Channels: 2}, nil, 0},
		{"float32_stereo", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}, nil, 0},
		{"convert_24_to_16", pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1}, []pcm.Option{pcm.WithConvertTo(16)}, 0},
		// A file cut short decodes to the audio that is present, with no error;
		// a read that insisted on filling the header's claimed length would fail.
		{"pcm16_mono_truncated", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, nil, 1001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := encodeFixture(t, tc.cfg, pattern(3000))
			file = file[:len(file)-tc.trim]

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

// directReadRecorder is a plain io.Reader (no Seek) that records, for every
// Read, the stream offset the read started at and the slice it was given, so a
// test can see which of the caller's buffers the source wrote into.
type directReadRecorder struct {
	data  []byte
	pos   int
	reads []recordedRead
}

type recordedRead struct {
	off int
	p   []byte // the destination window, already cut to the n bytes served
}

func (r *directReadRecorder) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.reads = append(r.reads, recordedRead{off: r.pos, p: p[:n]})
	r.pos += n
	return n, nil
}

// TestDecodeInterleavedReadsStraightIntoResult pins that the io.Reader one-shot
// has the source write the bulk of a pass-through stream into the slice it
// returns, instead of staging it in a second buffer and copying. It looks at
// where the source's own Read calls landed: the sum of bytes whose destination
// is the matching position of the result.
//
// The slack allows for the two buffered layers in front of the source: the
// header parse leaves up to a 512-byte window of audio in the decoder's reader,
// and a read into a window under 64 KiB goes through the 64 KiB audio reader
// instead of directly. That the audio reader reads straight into a slice of at
// least its own size is bufio behaviour (go1.27.3 src/bufio/bufio.go:224-240),
// so a toolchain that changes it fails this test rather than the code.
//
// An exact-fit reservation that regrew to discover EOF would move the result to
// a new array after the source had filled the old one, and fail here too.
func TestDecodeInterleavedReadsStraightIntoResult(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := pattern(1 << 20)
	file := encodeFixture(t, cfg, payload)
	dataStart := dataOffset(t, file)

	src := &directReadRecorder{data: file}
	got, _, err := pcm.DecodeInterleaved(src)
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("samples = %d bytes, want %d", len(got), len(payload))
	}

	direct := 0
	for _, rd := range src.reads {
		i := rd.off - dataStart
		if len(rd.p) == 0 || i < 0 || i+len(rd.p) > len(got) {
			continue
		}
		if &rd.p[0] == &got[i] {
			direct += len(rd.p)
		}
	}
	const slack = 128 << 10
	if direct < len(got)-slack {
		t.Errorf("source wrote %d of %d result bytes directly into the result, want at least %d",
			direct, len(got), len(got)-slack)
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
		// Without a declared length there is no reservation, so the first
		// growth is what has to respect the ceiling rather than take a full
		// 64 KiB window.
		for name, opts := range map[string][]pcm.Option{
			"declared length": nil,
			"no length":       {pcm.WithIgnoreLength()},
		} {
			t.Run(name, func(t *testing.T) {
				samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), 100, opts...)
				if !errors.Is(err, pcm.ErrDecodeLimit) {
					t.Fatalf("err = %v, want ErrDecodeLimit", err)
				}
				if samples != nil {
					t.Errorf("samples = %d bytes on a limited decode, want nil", len(samples))
				}
			})
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
		for _, limit := range []int{0, -1} {
			samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), limit)
			if err != nil {
				t.Fatalf("DecodeInterleavedLimit(max=%d): %v", limit, err)
			}
			if !bytes.Equal(samples, payload) {
				t.Errorf("max=%d: samples = %d bytes, want %d", limit, len(samples), len(payload))
			}
		}
	})

	t.Run("exactly at the ceiling succeeds", func(t *testing.T) {
		// The reservation fills exactly, and the probe read behind it reports
		// the end of the stream, so a ceiling equal to the payload length admits
		// it.
		samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(file), len(payload))
		if err != nil {
			t.Fatalf("DecodeInterleavedLimit at exact size: %v", err)
		}
		if !bytes.Equal(samples, payload) {
			t.Errorf("samples = %d bytes, want %d", len(samples), len(payload))
		}
	})
}

// TestDecodeInterleavedLimitBoundary pins the ceiling byte for byte on outputs
// of several reads: a ceiling one below the output is refused, one equal to it
// and one above it are served. The oracle is DecodeInterleavedBytes, which does
// not share the read loop. The rows cover a reservation that matches the output
// (declared length), growth from nothing (no length), a narrowing conversion
// whose reads are short, and a widening companded expansion.
func TestDecodeInterleavedLimitBoundary(t *testing.T) {
	pcm16 := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	pcm24 := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1}
	codes := pattern(100000)

	cases := []struct {
		name string
		file []byte
		opts []pcm.Option
	}{
		{"declared_length", encodeFixture(t, pcm16, pattern(200000)), nil},
		{"no_length", encodeFixture(t, pcm16, pattern(200000)), []pcm.Option{pcm.WithIgnoreLength()}},
		{"narrowing_no_length", encodeFixture(t, pcm24, pattern(210000)),
			[]pcm.Option{pcm.WithConvertTo(16), pcm.WithIgnoreLength()}},
		{"alaw_expansion", compandedFile(t, tagALaw, 1, 8000, codes), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, _, err := pcm.DecodeInterleavedBytes(tc.file, tc.opts...)
			if err != nil {
				t.Fatalf("DecodeInterleavedBytes: %v", err)
			}
			n := len(want)
			if n <= 64<<10 {
				t.Fatalf("output is %d bytes, want more than one 64 KiB read", n)
			}

			samples, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(tc.file), n-1, tc.opts...)
			if !errors.Is(err, pcm.ErrDecodeLimit) {
				t.Fatalf("limit n-1: err = %v, want ErrDecodeLimit", err)
			}
			if errors.Is(err, wav.ErrCorruptStream) {
				t.Errorf("limit n-1: a resource limit was reported as a corrupt stream: %v", err)
			}
			if samples != nil {
				t.Errorf("limit n-1: samples = %d bytes, want nil", len(samples))
			}

			for _, limit := range []int{n, n + 1} {
				got, _, err := pcm.DecodeInterleavedLimit(bytes.NewReader(tc.file), limit, tc.opts...)
				if err != nil {
					t.Fatalf("limit %d (n=%d): %v", limit, n, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("limit %d (n=%d): got %d bytes that differ from the oracle", limit, n, len(got))
				}
			}
		})
	}
}

// TestDecodeInterleavedReadError checks that a source failing partway through
// the audio surfaces its error unchanged with the stream info still populated
// and no samples, and that a ceiling already crossed by the audio read before
// the failure is reported as the limit instead.
func TestDecodeInterleavedReadError(t *testing.T) {
	sentinel := errors.New("source failed mid-stream")
	file := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(2*65536))
	newSrc := func() *errReader {
		return &errReader{data: file, at: len(file) - 20000, err: sentinel}
	}

	t.Run("default ceiling returns the source error", func(t *testing.T) {
		samples, info, err := pcm.DecodeInterleaved(newSrc())
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the source's sentinel", err)
		}
		if errors.Is(err, pcm.ErrDecodeLimit) {
			t.Errorf("a read error was reported as the decode limit: %v", err)
		}
		if samples != nil {
			t.Errorf("samples = %d bytes on a failed decode, want nil", len(samples))
		}
		if info.SampleRate != 48000 {
			t.Errorf("info.SampleRate = %d on a failed decode, want 48000", info.SampleRate)
		}
	})

	t.Run("ceiling crossed before the error is the limit", func(t *testing.T) {
		_, _, err := pcm.DecodeInterleavedLimit(newSrc(), 1000)
		if !errors.Is(err, pcm.ErrDecodeLimit) {
			t.Fatalf("err = %v, want ErrDecodeLimit", err)
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
