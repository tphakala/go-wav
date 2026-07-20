package pcm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// TestEncodeInterleavedRejectsPartialFrame checks the one way the one-shot path
// is stricter than a streaming Encoder.
func TestEncodeInterleavedRejectsPartialFrame(t *testing.T) {
	cases := []struct {
		name    string
		cfg     pcm.Config
		payload int
	}{
		{"s16 stereo, one byte short", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}, 7},
		{"s24 stereo, half a frame", pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}, 9},
		{"s32 6ch, one sample short", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 6}, 44},
		{"u8 stereo, odd byte", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 2}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := pcm.EncodeInterleaved(&buf, tc.cfg, pattern(tc.payload))
			if err == nil {
				t.Fatalf("accepted %d bytes that are not whole frames", tc.payload)
			}
			perFrame := tc.cfg.Channels * ((tc.cfg.BitDepth + 7) / 8)
			msg := err.Error()
			if !strings.Contains(msg, fmt.Sprint(tc.payload)) {
				t.Errorf("error does not name the payload size %d: %v", tc.payload, err)
			}
			if !strings.Contains(msg, fmt.Sprint(perFrame)) {
				t.Errorf("error does not name the frame size %d: %v", perFrame, err)
			}
		})
	}
}

// TestEncodeInterleavedIgnoresTotalFrames checks that the declared count is
// overridden by the length actually supplied.
func TestEncodeInterleavedIgnoresTotalFrames(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(200) // 100 frames

	for _, declared := range []uint64{0, 1, 99, 100, 101, 1 << 40} {
		t.Run(fmt.Sprintf("TotalFrames %d", declared), func(t *testing.T) {
			c := cfg
			c.TotalFrames = declared
			var buf bytes.Buffer
			if err := pcm.EncodeInterleaved(&buf, c, src); err != nil {
				t.Fatalf("EncodeInterleaved with TotalFrames %d: %v", declared, err)
			}
			assertDecodes(t, buf.Bytes(), cfg, src)
			if got := magic(t, buf.Bytes()); got != "RIFF" {
				t.Errorf("magic: got %q want %q", got, "RIFF")
			}
			if span := requireChunk(t, buf.Bytes(), "data"); span.size != len(src) {
				t.Errorf("data size field: got %d want %d", span.size, len(src))
			}
		})
	}
}

// TestEncodeInterleavedEmpty checks that a zero-length payload still produces a
// parseable stream.
func TestEncodeInterleavedEmpty(t *testing.T) {
	configs := []pcm.Config{
		{SampleRate: 48000, BitDepth: 16, Channels: 1},
		{SampleRate: 48000, BitDepth: 24, Channels: 2},
		{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat},
	}
	for _, cfg := range configs {
		t.Run(fmt.Sprintf("%dbit %dch", cfg.BitDepth, cfg.Channels), func(t *testing.T) {
			for _, payload := range [][]byte{nil, {}} {
				var buf bytes.Buffer
				if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
					t.Fatalf("EncodeInterleaved: %v", err)
				}
				if !wav.Sniff(buf.Bytes()) {
					t.Error("Sniff rejected a zero-length stream")
				}
				d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				if got := d.Info().TotalFrames; got != 0 {
					t.Errorf("TotalFrames: got %d want 0", got)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				if len(got) != 0 {
					t.Errorf("a zero-length stream yielded %d bytes", len(got))
				}
			}
		})
	}
}

// TestEncodeInterleavedConcurrent checks the only concurrency-safe entry point,
// with configurations that differ so that a shared pooled encoder would be
// visible in the output rather than merely in the race detector.
func TestEncodeInterleavedConcurrent(t *testing.T) {
	configs := []pcm.Config{
		{SampleRate: 48000, BitDepth: 16, Channels: 1},
		{SampleRate: 44100, BitDepth: 16, Channels: 2},
		{SampleRate: 96000, BitDepth: 24, Channels: 2},
		{SampleRate: 48000, BitDepth: 32, Channels: 6},
		{SampleRate: 8000, BitDepth: 8, Channels: 1},
		{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat},
		{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat},
		{SampleRate: 384000, BitDepth: 16, Channels: 1},
	}

	const goroutines = 50
	type result struct {
		cfg     pcm.Config
		payload []byte
		out     []byte
		err     error
	}
	results := make([]result, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := configs[i%len(configs)]
			width := (cfg.BitDepth + 7) / 8
			frames := 10 + i
			payload := make([]byte, frames*cfg.Channels*width)
			for j := range payload {
				payload[j] = byte(i*17 + j*5)
			}
			if cfg.Format == wav.SampleFormatFloat {
				payload = floatPattern(frames*cfg.Channels, cfg.BitDepth)
			}
			var buf bytes.Buffer
			err := pcm.EncodeInterleaved(&buf, cfg, payload)
			results[i] = result{cfg: cfg, payload: payload, out: buf.Bytes(), err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: %v", i, r.err)
			continue
		}
		assertDecodes(t, r.out, r.cfg, r.payload)
	}
}

// TestEncodeInterleavedDoesNotPinSink checks that the pooled encoder drops the
// caller's sink, so a later call cannot write into an earlier buffer.
func TestEncodeInterleavedDoesNotPinSink(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}

	const runs = 200
	buffers := make([]*bytes.Buffer, runs)
	payloads := make([][]byte, runs)
	lengths := make([]int, runs)

	for i := range runs {
		payloads[i] = pattern(2 * (i + 1))
		for j := range payloads[i] {
			payloads[i][j] = byte(i*13 + j)
		}
		buffers[i] = &bytes.Buffer{}
		if err := pcm.EncodeInterleaved(buffers[i], cfg, payloads[i]); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		lengths[i] = buffers[i].Len()

		// Every earlier buffer must still hold exactly what it held before.
		for j := range i {
			if buffers[j].Len() != lengths[j] {
				t.Fatalf("run %d changed buffer %d from %d to %d bytes",
					i, j, lengths[j], buffers[j].Len())
			}
		}
	}

	for i := range runs {
		assertDecodes(t, buffers[i].Bytes(), cfg, payloads[i])
	}
}

// TestEncodeInterleavedAfterFailure checks that a failed call does not poison
// the pool for the calls that follow it.
func TestEncodeInterleavedAfterFailure(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	boom := errors.New("sink exploded")

	for range 20 {
		w := &failWriter{limit: 8, err: boom}
		if err := pcm.EncodeInterleaved(w, cfg, pattern(64)); !errors.Is(err, boom) {
			t.Fatalf("expected the sink failure, got %v", err)
		}
		var buf bytes.Buffer
		src := pattern(64)
		if err := pcm.EncodeInterleaved(&buf, cfg, src); err != nil {
			t.Fatalf("a good call after a failed one: %v", err)
		}
		assertDecodes(t, buf.Bytes(), cfg, src)
	}
}

// TestEncodeInterleavedSeekableSink checks that the one-shot path works on a
// sink that can seek, where the header could also have been patched.
func TestEncodeInterleavedSeekableSink(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	src := pattern(60)
	sink := &memSeeker{}
	if err := pcm.EncodeInterleaved(sink, cfg, src); err != nil {
		t.Fatal(err)
	}
	assertDecodes(t, sink.b, cfg, src)
	if got := magic(t, sink.b); got != "RIFF" {
		t.Errorf("magic: got %q want %q", got, "RIFF")
	}
	// The length is known, so nothing needed reserving.
	if got := string(sink.b[fileHeaderSize : fileHeaderSize+4]); got == "JUNK" {
		t.Error("the one-shot path reserved ds64 space for a length it already knows")
	}
}

// TestEncodeInterleavedRF64Always covers the mode on both kinds of sink through
// the one-shot path, where TotalFrames is always supplied internally.
func TestEncodeInterleavedRF64Always(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, RF64: pcm.RF64Always}
	src := pattern(256)

	t.Run("plain writer", func(t *testing.T) {
		var buf bytes.Buffer
		if err := pcm.EncodeInterleaved(&buf, cfg, src); err != nil {
			t.Fatalf("EncodeInterleaved: %v", err)
		}
		assertRF64Shape(t, buf.Bytes(), int64(len(src)), uint64(len(src)/2))
		assertDecodes(t, buf.Bytes(), cfg, src)
	})

	t.Run("seekable sink", func(t *testing.T) {
		sink := &memSeeker{}
		if err := pcm.EncodeInterleaved(sink, cfg, src); err != nil {
			t.Fatalf("EncodeInterleaved: %v", err)
		}
		assertRF64Shape(t, sink.b, int64(len(src)), uint64(len(src)/2))
	})

	t.Run("empty payload on a plain writer", func(t *testing.T) {
		// A zero-length payload means zero declared frames, which is the one
		// combination RF64Always cannot describe on a sink that cannot seek.
		var buf bytes.Buffer
		err := pcm.EncodeInterleaved(&buf, cfg, nil)
		if err == nil {
			t.Log("an empty RF64Always stream on a plain writer was accepted")
			assertRF64Shape(t, buf.Bytes(), 0, 0)
		} else {
			t.Logf("an empty RF64Always stream on a plain writer was refused: %v", err)
		}
	})
}

// TestEncodeInterleavedOddLength checks the pad byte through the one-shot path.
func TestEncodeInterleavedOddLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
	src := pattern(7)
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, src); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	span := requireChunk(t, b, "data")
	if span.size != 7 {
		t.Errorf("data size field: got %d want 7", span.size)
	}
	if len(b) != span.payload+8 {
		t.Errorf("file length: got %d want %d", len(b), span.payload+8)
	}
	assertDecodes(t, b, cfg, src)
}
