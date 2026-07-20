package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// dataOffset is the absolute offset of the first audio byte of a stream, which
// is what the returned slice must begin at.
func dataOffset(tb testing.TB, b []byte) int {
	tb.Helper()
	return requireChunk(tb, b, "data").payload
}

// withTrailer appends a chunk after the end of b, which is how a real file
// carries a LIST or id3 trailer behind its audio. The caller must pass a stream
// whose data chunk is even length, so that the trailer starts word aligned.
func withTrailer(id string, payload, b []byte) []byte {
	out := make([]byte, 0, len(b)+8+len(payload))
	out = append(out, b...)
	out = append(out, id...)
	var size [4]byte
	//nolint:gosec // G115: test payloads are far below 4 GiB.
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	out = append(out, size[:]...)
	return append(out, payload...)
}

// decoderResult is what a streaming Decoder makes of the same stream, which is
// the reference every one-shot result is checked against.
func decoderResult(tb testing.TB, b []byte, opts ...pcm.Option) (info wav.StreamInfo, audio []byte) {
	tb.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(b), opts...)
	if err != nil {
		tb.Fatalf("NewDecoder: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		tb.Fatalf("ReadAll: %v", err)
	}
	return d.Info(), got
}

// assertMatchesDecoder pins the invariant that binds the two entry points: the
// one-shot path must yield exactly what streaming the same bytes yields. It is
// the one assertion that cannot go stale, since it is checked against the
// implementation the library already had.
func assertMatchesDecoder(tb testing.TB, b []byte, opts ...pcm.Option) []byte {
	tb.Helper()
	wantInfo, want := decoderResult(tb, b, opts...)
	gotInfo, got, err := pcm.DecodeInterleaved(b, opts...)
	if err != nil {
		tb.Fatalf("DecodeInterleaved: %v", err)
	}
	if gotInfo != wantInfo {
		tb.Errorf("StreamInfo: got %+v want %+v", gotInfo, wantInfo)
	}
	if !bytes.Equal(got, want) {
		tb.Errorf("audio: got %d bytes want %d%s", len(got), len(want), describeDifference(got, want))
	}
	return got
}

// describeDifference says where two byte strings first diverge, as a clause to
// append to a message that has already reported their lengths. A mismatch used
// to report only those lengths, which says nothing at all in the case that
// matters most: equal lengths and different bytes.
func describeDifference(got, want []byte) string {
	n := min(len(got), len(want))
	for i := range n {
		if got[i] != want[i] {
			return fmt.Sprintf(", first differing at offset %d: got %#02x want %#02x", i, got[i], want[i])
		}
	}
	if len(got) == len(want) {
		return ""
	}
	return fmt.Sprintf(", agreeing on the first %d bytes and then one of them ending", n)
}

// TestDecodeInterleavedAliases is the whole design: with no conversion asked
// for, the returned slice is a window onto the caller's own buffer, so a write
// through either one is visible through the other.
func TestDecodeInterleavedAliases(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)
	file := encodeFixture(t, cfg, src)
	start := dataOffset(t, file)

	info, got, err := pcm.DecodeInterleaved(file)
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if info.BitDepth != 16 || info.Channels != 1 || info.SampleRate != 48000 {
		t.Errorf("StreamInfo: %+v does not describe the stream", info)
	}
	if !bytes.Equal(got, src) {
		t.Fatalf("audio: got %d bytes want %d", len(got), len(src))
	}

	// A write through the returned slice must land in the caller's buffer.
	got[0] ^= 0xFF
	if file[start] != got[0] {
		t.Errorf("writing through the returned slice did not reach the input: input holds %#02x, the slice holds %#02x",
			file[start], got[0])
	}

	// And a write through the caller's buffer must be visible in the slice.
	last := len(got) - 1
	file[start+last] ^= 0xFF
	if got[last] != file[start+last] {
		t.Errorf("writing through the input did not reach the returned slice: input holds %#02x, the slice holds %#02x",
			file[start+last], got[last])
	}
}

// TestDecodeInterleavedReturnedSliceCannotReachTheTrailer checks that the
// returned slice cannot be grown into whatever follows the audio. An append by
// a caller who did not ask for one would otherwise overwrite a trailing chunk
// of the file it was handed.
func TestDecodeInterleavedReturnedSliceCannotReachTheTrailer(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	base := encodeFixture(t, cfg, pattern(64))
	file := withTrailer("LIST", []byte("INFOhello world!"), base)
	trailerAt := len(base)
	trailer := bytes.Clone(file[trailerAt:])

	_, got, err := pcm.DecodeInterleaved(file)
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if cap(got) != len(got) {
		t.Errorf("returned slice has capacity %d for a length of %d, so an append would reach past the audio",
			cap(got), len(got))
	}
	// Eight bytes is well inside the spare room a slice bounded only by the end
	// of the input would have had, so an append lands on the trailer rather
	// than on a fresh array.
	_ = append(got, make([]byte, 8)...) //nolint:gocritic // appending to the result is exactly what is under test.
	if !bytes.Equal(file[trailerAt:], trailer) {
		t.Error("appending to the returned slice overwrote the trailing chunk of the input")
	}
}

// TestDecodeInterleavedStopsAtTheDataChunk checks that a trailing chunk is not
// handed back as audio, which is what bounding by the declared size buys.
func TestDecodeInterleavedStopsAtTheDataChunk(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)
	file := withTrailer("id3 ", pattern(40), encodeFixture(t, cfg, src))

	got := assertMatchesDecoder(t, file)
	if !bytes.Equal(got, src) {
		t.Errorf("audio: got %d bytes want %d, so the trailer leaked in", len(got), len(src))
	}
}

// TestDecodeInterleavedRoundTrip covers every supported width and both sample
// formats through the encoder that wrote them.
func TestDecodeInterleavedRoundTrip(t *testing.T) {
	cases := []struct {
		cfg    pcm.Config
		frames int
	}{
		{pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}, 33},
		{pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 2}, 17},
		{pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, 64},
		{pcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2}, 40},
		{pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 1}, 21},
		{pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}, 30},
		{pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1}, 25},
		{pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 6}, 11},
		{pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}, 26},
		{pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat}, 19},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%v %dbit %dch", tc.cfg.Format, tc.cfg.BitDepth, tc.cfg.Channels)
		t.Run(name, func(t *testing.T) {
			samples := tc.frames * tc.cfg.Channels
			src := pattern(samples * (tc.cfg.BitDepth / 8))
			if tc.cfg.Format == wav.SampleFormatFloat {
				src = floatPattern(samples, tc.cfg.BitDepth)
			}
			file := encodeFixture(t, tc.cfg, src)

			info, got, err := pcm.DecodeInterleaved(file)
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if info.BitDepth != tc.cfg.BitDepth || info.Format != tc.cfg.Format ||
				info.Channels != tc.cfg.Channels || info.SampleRate != tc.cfg.SampleRate {
				t.Errorf("StreamInfo %+v does not match config %+v", info, tc.cfg)
			}
			if want := uint64(tc.frames); info.TotalFrames != want {
				t.Errorf("TotalFrames: got %d want %d", info.TotalFrames, want)
			}
			if !bytes.Equal(got, src) {
				t.Fatalf("audio: got %d bytes want %d", len(got), len(src))
			}
			// The payload was not copied, so the encoder's own bytes are what
			// came back.
			if start := dataOffset(t, file); &got[0] != &file[start] {
				t.Error("the returned slice does not start at the data chunk of the input")
			}
		})
	}
}

// TestDecodeInterleavedUnknownLength covers a data chunk whose size field was
// never patched, where there is no declared length to slice by and the bound
// has to come from what is actually present.
func TestDecodeInterleavedUnknownLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)

	for _, declared := range []uint32{0, 0xFFFFFFFF} {
		t.Run(fmt.Sprintf("size field %#x", declared), func(t *testing.T) {
			file := patchDataSize(t, encodeFixture(t, cfg, src), declared)

			got := assertMatchesDecoder(t, file)
			if !bytes.Equal(got, src) {
				t.Errorf("audio: got %d bytes want %d", len(got), len(src))
			}
			if start := dataOffset(t, file); len(got) != len(file)-start {
				t.Errorf("audio: got %d bytes, want the %d bytes actually present",
					len(got), len(file)-start)
			}
		})
	}
}

// TestDecodeInterleavedIgnoreLength covers the option that discards a declared
// length the caller does not trust, which puts the one-shot path on the same
// read-to-the-end bound as the unknown-length case.
func TestDecodeInterleavedIgnoreLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)
	// A trailer is handed back as audio here, exactly as WithIgnoreLength
	// documents, because without a length nothing tells audio and trailer
	// apart.
	file := withTrailer("LIST", pattern(8), encodeFixture(t, cfg, src))

	got := assertMatchesDecoder(t, file, pcm.WithIgnoreLength())
	if start := dataOffset(t, file); len(got) != len(file)-start {
		t.Errorf("audio: got %d bytes, want the %d bytes that follow the header",
			len(got), len(file)-start)
	}
}

// TestDecodeInterleavedTruncated covers a header that declares more audio than
// the buffer holds. The declared size is only a claim, so slicing by it would
// panic; the result is bounded by what is there and, like a truncated read,
// reported as a short result rather than as damage.
func TestDecodeInterleavedTruncated(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)

	t.Run("bytes cut off the end", func(t *testing.T) {
		full := encodeFixture(t, cfg, src)
		file := full[:len(full)-20]
		got := assertMatchesDecoder(t, file)
		if want := len(src) - 20; len(got) != want {
			t.Errorf("audio: got %d bytes want %d", len(got), want)
		}
	})

	t.Run("size field larger than the file", func(t *testing.T) {
		file := patchDataSize(t, encodeFixture(t, cfg, src), 1<<30)
		got := assertMatchesDecoder(t, file)
		if len(got) != len(src) {
			t.Errorf("audio: got %d bytes want %d", len(got), len(src))
		}
	})

	t.Run("header only", func(t *testing.T) {
		full := encodeFixture(t, cfg, src)
		file := full[:dataOffset(t, full)]
		info, got, err := pcm.DecodeInterleaved(file)
		if err != nil {
			t.Fatalf("DecodeInterleaved: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("a stream that ends at its data chunk yielded %d bytes", len(got))
		}
		if info.SampleRate != cfg.SampleRate || info.Channels != cfg.Channels {
			t.Errorf("StreamInfo %+v does not describe the header", info)
		}
	})
}

// TestDecodeInterleavedExcludesPadByte covers the trailing byte an odd length
// data chunk carries for alignment, which is not audio and must not be handed
// back as though it were.
func TestDecodeInterleavedExcludesPadByte(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
	src := pattern(7)
	file := encodeFixture(t, cfg, src)

	start := dataOffset(t, file)
	if len(file)-start != 8 {
		t.Fatalf("fixture holds %d bytes after the header, want 8 including the pad byte",
			len(file)-start)
	}

	got := assertMatchesDecoder(t, file)
	if len(got) != 7 {
		t.Fatalf("audio: got %d bytes want 7, so the pad byte came back as a sample", len(got))
	}
	if !bytes.Equal(got, src) {
		t.Error("audio does not match what was encoded")
	}
}

// TestDecodeInterleavedEmptyDataChunk covers a stream that carries a header and
// no samples at all.
func TestDecodeInterleavedEmptyDataChunk(t *testing.T) {
	configs := []pcm.Config{
		{SampleRate: 48000, BitDepth: 16, Channels: 1},
		{SampleRate: 48000, BitDepth: 24, Channels: 2},
		{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat},
	}
	for _, cfg := range configs {
		t.Run(fmt.Sprintf("%dbit %dch", cfg.BitDepth, cfg.Channels), func(t *testing.T) {
			file := encodeFixture(t, cfg, nil)
			info, got, err := pcm.DecodeInterleaved(file)
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("an empty stream yielded %d bytes", len(got))
			}
			if info.TotalFrames != 0 {
				t.Errorf("TotalFrames: got %d want 0", info.TotalFrames)
			}
			// Converting nothing is not an error either. The streaming decoder
			// reaches the end before it ever converts a sample, so the one-shot
			// path must not report a failure where the decoder reports none.
			if _, converted, cerr := pcm.DecodeInterleaved(file, pcm.WithConvertTo(16)); cerr != nil {
				t.Errorf("converting an empty stream: %v", cerr)
			} else if len(converted) != 0 {
				t.Errorf("converting an empty stream yielded %d bytes", len(converted))
			}
		})
	}
}

// TestDecodeInterleavedConvert covers the conversion option, where the result
// is a fresh buffer rather than a window onto the input.
func TestDecodeInterleavedConvert(t *testing.T) {
	sources := []pcm.Config{
		{SampleRate: 8000, BitDepth: 8, Channels: 1},
		{SampleRate: 48000, BitDepth: 16, Channels: 2},
		{SampleRate: 48000, BitDepth: 24, Channels: 1},
		{SampleRate: 48000, BitDepth: 32, Channels: 2},
		{SampleRate: 44100, BitDepth: 32, Channels: 1, Format: wav.SampleFormatFloat},
		{SampleRate: 48000, BitDepth: 64, Channels: 2, Format: wav.SampleFormatFloat},
	}
	for _, cfg := range sources {
		for _, to := range []int{8, 16, 24, 32} {
			name := fmt.Sprintf("%v %d to %d", cfg.Format, cfg.BitDepth, to)
			t.Run(name, func(t *testing.T) {
				const frames = 24
				samples := frames * cfg.Channels
				src := pattern(samples * (cfg.BitDepth / 8))
				if cfg.Format == wav.SampleFormatFloat {
					src = floatPattern(samples, cfg.BitDepth)
				}
				file := encodeFixture(t, cfg, src)

				got := assertMatchesDecoder(t, file, pcm.WithConvertTo(to))
				if want := samples * (to / 8); len(got) != want {
					t.Fatalf("converted audio: got %d bytes want %d", len(got), want)
				}

				// The converted buffer is freshly allocated, so neither buffer
				// can be seen through the other.
				before := bytes.Clone(file)
				got[0] ^= 0xFF
				if !bytes.Equal(file, before) {
					t.Error("writing through the converted slice changed the input")
				}
				snapshot := bytes.Clone(got)
				for i := range file {
					file[i] ^= 0xFF
				}
				if !bytes.Equal(got, snapshot) {
					t.Error("writing through the input changed the converted slice")
				}
			})
		}
	}
}

// TestDecodeInterleavedConvertInfo pins that the returned StreamInfo describes
// the returned bytes, which is the same promise Decoder.Info makes.
func TestDecodeInterleavedConvertInfo(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1, Format: wav.SampleFormatFloat}
	file := encodeFixture(t, cfg, floatPattern(32, 32))

	info, got, err := pcm.DecodeInterleaved(file, pcm.WithConvertTo(16))
	if err != nil {
		t.Fatalf("DecodeInterleaved: %v", err)
	}
	if info.BitDepth != 16 || info.Format != wav.SampleFormatPCM {
		t.Errorf("Info reports %d bit %v, want 16 bit pcm", info.BitDepth, info.Format)
	}
	if info.SourceBitDepth != 32 || info.SourceFormat != wav.SampleFormatFloat {
		t.Errorf("Info reports a source of %d bit %v, want 32 bit float",
			info.SourceBitDepth, info.SourceFormat)
	}
	if want := 32 * 2; len(got) != want {
		t.Errorf("converted audio: got %d bytes want %d", len(got), want)
	}
}

// TestDecodeInterleavedConvertPartialSample covers audio whose length is not a
// whole number of stored samples, which happens whenever a file is cut short.
// The trailing fragment cannot be converted, so it is dropped rather than
// reported.
func TestDecodeInterleavedConvertPartialSample(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1}
	src := pattern(12) // four samples
	full := encodeFixture(t, cfg, src)

	// Two bytes short of the fourth sample, with the header still claiming all
	// twelve.
	file := full[:len(full)-2]
	got := assertMatchesDecoder(t, file, pcm.WithConvertTo(16))
	if want := 3 * 2; len(got) != want {
		t.Fatalf("converted audio: got %d bytes want %d whole samples", len(got), want)
	}
}

// TestDecodeInterleavedRejectsMalformed checks that a stream the streaming
// decoder refuses is refused here too, with the same error.
func TestDecodeInterleavedRejectsMalformed(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	good := encodeFixture(t, cfg, pattern(64))

	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"nil", nil, wav.ErrNotRIFF},
		{"empty", []byte{}, wav.ErrNotRIFF},
		{"short of a file header", good[:8], wav.ErrNotRIFF},
		{"not a riff stream", append([]byte("XXXX"), good[4:]...), wav.ErrNotRIFF},
		{"no data chunk", good[:requireChunk(t, good, "data").payload-8], wav.ErrCorruptStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, got, err := pcm.DecodeInterleaved(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error: got %v want one wrapping %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("a rejected stream still yielded %d bytes", len(got))
			}
			if info != (wav.StreamInfo{}) {
				t.Errorf("a rejected stream still described itself: %+v", info)
			}
		})
	}
}

// TestDecodeInterleavedRejectsBadOption checks that an unusable conversion
// width is reported rather than ignored, matching NewDecoder.
func TestDecodeInterleavedRejectsBadOption(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	file := encodeFixture(t, cfg, pattern(64))

	for _, to := range []int{0, 1, 12, 64} {
		t.Run(fmt.Sprintf("convert to %d", to), func(t *testing.T) {
			_, got, err := pcm.DecodeInterleaved(file, pcm.WithConvertTo(to))
			if !errors.Is(err, wav.ErrUnsupported) {
				t.Fatalf("error: got %v want one wrapping %v", err, wav.ErrUnsupported)
			}
			if got != nil {
				t.Errorf("a rejected option still yielded %d bytes", len(got))
			}
		})
	}
}

// TestDecodeInterleavedConcurrent checks that the one-shot path is safe for
// concurrent use, with configurations that differ so that shared state would
// show up in the output rather than only in the race detector.
func TestDecodeInterleavedConcurrent(t *testing.T) {
	configs := []pcm.Config{
		{SampleRate: 48000, BitDepth: 16, Channels: 1},
		{SampleRate: 44100, BitDepth: 16, Channels: 2},
		{SampleRate: 96000, BitDepth: 24, Channels: 2},
		{SampleRate: 48000, BitDepth: 32, Channels: 6},
		{SampleRate: 8000, BitDepth: 8, Channels: 1},
		{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat},
	}

	const goroutines = 50
	type result struct {
		cfg  pcm.Config
		want []byte
		info wav.StreamInfo
		got  []byte
		err  error
	}
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := configs[i%len(configs)]
			samples := (10 + i) * cfg.Channels
			payload := pattern(samples * (cfg.BitDepth / 8))
			if cfg.Format == wav.SampleFormatFloat {
				payload = floatPattern(samples, cfg.BitDepth)
			}
			var buf bytes.Buffer
			if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
				results[i] = result{err: err}
				return
			}
			info, got, err := pcm.DecodeInterleaved(buf.Bytes())
			results[i] = result{cfg: cfg, want: payload, info: info, got: got, err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: %v", i, r.err)
			continue
		}
		if r.info.BitDepth != r.cfg.BitDepth || r.info.Channels != r.cfg.Channels ||
			r.info.SampleRate != r.cfg.SampleRate || r.info.Format != r.cfg.Format {
			t.Errorf("goroutine %d: StreamInfo %+v does not match config %+v", i, r.info, r.cfg)
		}
		if !bytes.Equal(r.got, r.want) {
			t.Errorf("goroutine %d: audio: got %d bytes want %d", i, len(r.got), len(r.want))
		}
	}
}

// TestDecodeInterleavedAfterFailure checks that a rejected stream does not
// leave the next call decoding against stale state, which is the hazard of
// recycling anything between calls.
func TestDecodeInterleavedAfterFailure(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)
	good := encodeFixture(t, cfg, src)

	for range 20 {
		if _, _, err := pcm.DecodeInterleaved([]byte("RIFFnonsenseWAVE")); err == nil {
			t.Fatal("a malformed stream was accepted")
		}
		info, got, err := pcm.DecodeInterleaved(good)
		if err != nil {
			t.Fatalf("a good stream after a rejected one: %v", err)
		}
		if info.SampleRate != cfg.SampleRate || !bytes.Equal(got, src) {
			t.Fatalf("a good stream after a rejected one decoded as %+v with %d bytes",
				info, len(got))
		}
	}
}

// TestDecodeInterleavedResultsSurviveLaterCalls checks that a result handed
// back by one call is left alone by the calls that follow it, which is what the
// recycling has to guarantee: the decoder and its reader are pooled, the window
// onto the caller's buffer is not.
//
// It does not check that the pool stops retaining the input, and deliberately
// does not try to: a sync.Pool is drained by the GC, so a decoder that kept its
// last buffer would be collected along with it rather than held. Retention is
// handled by resetting the reader before pooling, which no test here can see.
func TestDecodeInterleavedResultsSurviveLaterCalls(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	const runs = 50
	files := make([][]byte, runs)
	payloads := make([][]byte, runs)
	slices := make([][]byte, runs)

	for i := range runs {
		payloads[i] = pattern(2 * (i + 1))
		for j := range payloads[i] {
			payloads[i][j] = byte(i*13 + j)
		}
		files[i] = encodeFixture(t, cfg, payloads[i])
		_, got, err := pcm.DecodeInterleaved(files[i])
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		slices[i] = got
	}

	// Every earlier result must still be a window onto its own file, unchanged
	// by the runs that followed it.
	for i := range runs {
		if !bytes.Equal(slices[i], payloads[i]) {
			t.Errorf("run %d: audio changed after later calls", i)
		}
	}
}

// TestDecodeInterleavedDoesNotCopyAtAnyLength pins the property the entry point
// exists for: the work it does is the same whatever the length of the clip,
// because it slices instead of copying. A result that still begins at the data
// chunk of the caller's own buffer is a result that was not copied, by
// definition, and a megabyte of audio has to come back the same way sixty-four
// bytes does.
//
// The tempting way to state this is an allocation count, and it is the wrong
// way. testing.AllocsPerRun reads the process-global malloc counter, so a GC
// that drains the decoder pool part way through a measurement shifts the
// average by one, in either direction; measured here, the short clip reported
// nine allocations against the long clip's eight about as often as the reverse.
// A copy is a make plus a copy, which is also exactly one allocation. The noise
// and the regression are the same size, so no threshold separates them.
// Comparing addresses has neither problem and needs no threshold.
func TestDecodeInterleavedDoesNotCopyAtAnyLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}

	for _, n := range []int{64, 1 << 20} {
		t.Run(fmt.Sprintf("%d bytes of audio", n), func(t *testing.T) {
			src := pattern(n)
			file := encodeFixture(t, cfg, src)
			start := dataOffset(t, file)

			_, got, err := pcm.DecodeInterleaved(file)
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if !bytes.Equal(got, src) {
				t.Fatalf("audio: got %d bytes want %d%s", len(got), len(src), describeDifference(got, src))
			}
			if &got[0] != &file[start] {
				t.Error("the returned slice does not begin at the data chunk of the input, so the audio was copied")
			}
			if cap(got) != len(got) {
				t.Errorf("returned slice has capacity %d for a length of %d, so it is not bounded by the audio",
					cap(got), len(got))
			}
		})
	}
}
