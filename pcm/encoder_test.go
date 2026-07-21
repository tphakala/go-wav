package pcm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// testEncoderChunkSizeCase is the body of TestEncoderAwkwardChunkSizes.
func testEncoderChunkSizeCase(t *testing.T, cc struct {
	name string
	cfg  pcm.Config
}, chunk int) {
	t.Helper()
	const frames = 97
	width := (cc.cfg.BitDepth + 7) / 8
	src := pattern(frames * cc.cfg.Channels * width)
	if cc.cfg.Format == wav.SampleFormatFloat {
		src = floatPattern(frames*cc.cfg.Channels, cc.cfg.BitDepth)
	}

	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cc.cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	for i := 0; i < len(src); i += chunk {
		end := min(i+chunk, len(src))
		n, werr := e.Write(src[i:end])
		if werr != nil {
			t.Fatalf("Write at %d: %v", i, werr)
		}
		if n != end-i {
			t.Fatalf("Write at %d returned %d, want %d", i, n, end-i)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d, err := pcm.NewDecoder(bytes.NewReader(sink.b))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("payload differs: got %d bytes, want %d", len(got), len(src))
		for i := range min(len(got), len(src)) {
			if got[i] != src[i] {
				t.Errorf("first difference at byte %d: got %#02x want %#02x",
					i, got[i], src[i])
				break
			}
		}
	}
	if d.Info().TotalFrames != frames {
		t.Errorf("TotalFrames: got %d want %d", d.Info().TotalFrames, frames)
	}
}

// TestEncoderAwkwardChunkSizes drives Write with buffer lengths that split a
// sample in half, which is where the carry path either works or corrupts the
// payload.
func TestEncoderAwkwardChunkSizes(t *testing.T) {
	configs := []struct {
		name string
		cfg  pcm.Config
	}{
		{"u8 mono", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}},
		{"s16 stereo", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}},
		{"s24 stereo", pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 2}},
		{"s24 mono", pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 1}},
		{"s32 mono", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1}},
		{"f32 stereo", pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2,
			Format: wav.SampleFormatFloat}},
		{"f64 mono", pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1,
			Format: wav.SampleFormatFloat}},
	}
	chunkSizes := []int{1, 2, 3, 5, 7, 13, 4096}

	for _, cc := range configs {
		for _, chunk := range chunkSizes {
			t.Run(fmt.Sprintf("%s/chunk %d", cc.name, chunk), func(t *testing.T) {
				testEncoderChunkSizeCase(t, cc, chunk)
			})
		}
	}
}

// TestEncoderCloseTrailingPartialSample checks that a sub-sample remainder left
// at Close is reported rather than silently padded or dropped.
func TestEncoderCloseTrailingPartialSample(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = e.Close()
	if err == nil {
		t.Fatal("Close accepted a trailing partial sample")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Errorf("Close error does not mention the trailing bytes: %v", err)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("Close error does not name the byte count: %v", err)
	}
}

// TestEncoderCloseIdempotent checks that Close stores its result.
func TestEncoderCloseIdempotent(t *testing.T) {
	t.Run("successful close", func(t *testing.T) {
		sink := &memSeeker{}
		e, err := pcm.NewEncoder(sink, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(pattern(64)); err != nil {
			t.Fatal(err)
		}
		first := e.Close()
		if first != nil {
			t.Fatalf("Close: %v", first)
		}
		for i := 2; i <= 3; i++ {
			if again := e.Close(); !errors.Is(again, first) {
				t.Errorf("Close call %d returned %v, want %v", i, again, first)
			}
		}
		if n := len(sink.b); n == 0 {
			t.Error("repeated Close wrote nothing at all")
		}
	})

	t.Run("failed close", func(t *testing.T) {
		sink := &memSeeker{}
		e, err := pcm.NewEncoder(sink, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		first := e.Close()
		if first == nil {
			t.Fatal("Close accepted a trailing partial sample")
		}
		for i := 2; i <= 3; i++ {
			again := e.Close()
			if again == nil || again.Error() != first.Error() {
				t.Errorf("Close call %d returned %v, want %v", i, again, first)
			}
		}
	})
}

// TestEncoderWriteAfterClose checks the closed sentinel.
func TestEncoderWriteAfterClose(t *testing.T) {
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write(pattern(16)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := e.Write(pattern(16))
	if !errors.Is(err, wav.ErrEncoderClosed) {
		t.Errorf("Write after Close: got %v, want wav.ErrEncoderClosed", err)
	}
	if n != 0 {
		t.Errorf("Write after Close reported %d bytes accepted, want 0", n)
	}
}

// TestEncoderErrorLatching checks that the first sink failure is the value every
// later call reports.
func TestEncoderErrorLatching(t *testing.T) {
	boom := errors.New("sink exploded")
	// A limit well past the header so that construction succeeds and the
	// failure lands in the middle of the audio.
	w := &failWriter{limit: 128, err: boom}
	e, err := pcm.NewEncoder(w, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}

	var firstErr error
	for i := 0; i < 10 && firstErr == nil; i++ {
		if _, werr := e.Write(pattern(64)); werr != nil {
			firstErr = werr
		}
	}
	if firstErr == nil {
		t.Fatal("no Write ever failed against a writer that stops at 128 bytes")
	}
	if !errors.Is(firstErr, boom) {
		t.Fatalf("first failure: got %v, want %v", firstErr, boom)
	}
	for i := range 3 {
		if _, werr := e.Write(pattern(64)); !errors.Is(werr, boom) {
			t.Errorf("Write %d after failure: got %v, want the latched %v", i, werr, boom)
		}
	}
	if cerr := e.Close(); !errors.Is(cerr, boom) {
		t.Errorf("Close after failure: got %v, want the latched %v", cerr, boom)
	}
	if cerr := e.Close(); !errors.Is(cerr, boom) {
		t.Errorf("second Close after failure: got %v, want the latched %v", cerr, boom)
	}
}

// TestEncoderOddLengthDataChunk checks that an odd data chunk is padded on disk
// while its size field keeps the odd value, and that the result still parses.
func TestEncoderOddLengthDataChunk(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
	src := pattern(5)

	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	span := requireChunk(t, sink.b, "data")
	if span.size != len(src) {
		t.Errorf("data chunk size field: got %d want %d (it must stay odd)", span.size, len(src))
	}
	wantLen := span.payload + len(src) + 1
	if len(sink.b) != wantLen {
		t.Errorf("file length: got %d want %d (payload %d, %d audio bytes, one pad byte)",
			len(sink.b), wantLen, span.payload, len(src))
	}
	if pad := sink.b[len(sink.b)-1]; pad != 0 {
		t.Errorf("pad byte is %#02x, want 0x00", pad)
	}

	d, err := pcm.NewDecoder(bytes.NewReader(sink.b))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("payload: got %v want %v", got, src)
	}
}

// TestEncoderZeroValue checks that an uninitialised Encoder reports rather than
// panics.
func TestEncoderZeroValue(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("zero-value Encoder panicked: %v", r)
		}
	}()

	var e pcm.Encoder
	n, err := e.Write(pattern(16))
	if err == nil {
		t.Error("Write on a zero-value Encoder returned no error")
	}
	if n != 0 {
		t.Errorf("Write on a zero-value Encoder accepted %d bytes", n)
	}
	if !errors.Is(err, wav.ErrEncoderClosed) {
		t.Errorf("Write on a zero-value Encoder: got %v, want wav.ErrEncoderClosed", err)
	}
	if err := e.Close(); !errors.Is(err, wav.ErrEncoderClosed) {
		t.Errorf("Close on a zero-value Encoder: got %v, want wav.ErrEncoderClosed", err)
	}
	info := e.StreamInfo()
	if info.SampleRate != 0 || info.TotalFrames != 0 {
		t.Errorf("StreamInfo on a zero-value Encoder: got %+v", info)
	}
}

// TestEncoderReset checks that an encoder can be reused across streams.
func TestEncoderReset(t *testing.T) {
	cfgA := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	cfgB := pcm.Config{SampleRate: 8000, BitDepth: 24, Channels: 2}

	t.Run("after a clean close", func(t *testing.T) {
		first := &memSeeker{}
		e, err := pcm.NewEncoder(first, cfgA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(pattern(64)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}

		second := &memSeeker{}
		if err := e.Reset(second, cfgB); err != nil {
			t.Fatalf("Reset after Close: %v", err)
		}
		payload := pattern(60)
		if _, err := e.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close of the second stream: %v", err)
		}
		assertDecodes(t, second.b, cfgB, payload)
	})

	t.Run("after a failed stream", func(t *testing.T) {
		boom := errors.New("sink exploded")
		w := &failWriter{limit: 64, err: boom}
		e, err := pcm.NewEncoder(w, cfgA)
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			if _, werr := e.Write(pattern(64)); werr != nil {
				break
			}
		}
		_ = e.Close()

		second := &memSeeker{}
		if err := e.Reset(second, cfgA); err != nil {
			t.Fatalf("Reset after a failed stream: %v", err)
		}
		payload := pattern(64)
		if _, err := e.Write(payload); err != nil {
			t.Fatalf("Write after Reset: %v", err)
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close after Reset: %v", err)
		}
		assertDecodes(t, second.b, cfgA, payload)
	})
}

// TestEncoderResetRejectedConfigLeavesNoLiveEncoder covers the state an encoder
// is left in when Reset rejects a configuration after the previous stream has
// already been discarded.
func TestEncoderResetRejectedConfigLeavesNoLiveEncoder(t *testing.T) {
	// A channel count of 65535 passes Config.validate, since the fmt field is
	// 16 bits wide, but the derived block align of 4 * 65535 does not fit the
	// 16-bit nBlockAlign field, so header construction fails afterwards.
	bad := pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 65535}

	t.Run("NewEncoder rejects it", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := pcm.NewEncoder(&buf, bad); err == nil {
			t.Fatal("NewEncoder accepted a block align that cannot be expressed")
		}
	})

	t.Run("Write after the failed Reset does not panic", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Write after a failed Reset panicked: %v", r)
			}
		}()

		good := &memSeeker{}
		e, err := pcm.NewEncoder(good, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
		if err != nil {
			t.Fatal(err)
		}
		if rerr := e.Reset(io.Discard, bad); rerr == nil {
			t.Fatal("Reset accepted a block align that cannot be expressed")
		}
		// The encoder must now be inert: either it reports an error or it is
		// still bound to the previous stream. What it must not do is fault.
		if _, werr := e.Write(pattern(64)); werr == nil {
			t.Error("Write succeeded on an encoder whose Reset failed")
		}
	})
}

// TestEncoderConfigValidation checks every rejected configuration.
func TestEncoderConfigValidation(t *testing.T) {
	cases := []struct {
		name         string
		cfg          pcm.Config
		unsupported  bool
		wantAccepted bool
	}{
		{name: "sample rate zero", cfg: pcm.Config{SampleRate: 0, BitDepth: 16, Channels: 1}},
		{name: "sample rate negative", cfg: pcm.Config{SampleRate: -1, BitDepth: 16, Channels: 1}},
		{name: "channels zero", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 0}},
		{name: "channels negative", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: -2}},
		{name: "channels 65536", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 65536}},
		{name: "bit depth zero", cfg: pcm.Config{SampleRate: 48000, BitDepth: 0, Channels: 1},
			unsupported: true},
		{name: "bit depth 12", cfg: pcm.Config{SampleRate: 48000, BitDepth: 12, Channels: 1},
			unsupported: true},
		{name: "bit depth 20", cfg: pcm.Config{SampleRate: 48000, BitDepth: 20, Channels: 1},
			unsupported: true},
		{name: "integer at 64 bits", cfg: pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1},
			unsupported: true},
		{name: "float at 8 bits", cfg: pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 1,
			Format: wav.SampleFormatFloat}, unsupported: true},
		{name: "float at 16 bits", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
			Format: wav.SampleFormatFloat}, unsupported: true},
		{name: "float at 24 bits", cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1,
			Format: wav.SampleFormatFloat}, unsupported: true},
		{name: "unknown sample format", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
			Format: wav.SampleFormat(42)}, unsupported: true},
		{name: "unknown rf64 mode", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
			RF64: pcm.RF64Mode(9)}},
		{name: "negative rf64 mode", cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
			RF64: pcm.RF64Mode(-1)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := pcm.NewEncoder(&buf, tc.cfg)
			if err == nil {
				t.Fatalf("NewEncoder accepted %+v", tc.cfg)
			}
			if buf.Len() != 0 {
				t.Errorf("a rejected configuration still wrote %d bytes", buf.Len())
			}
			if tc.unsupported && !errors.Is(err, wav.ErrUnsupported) {
				t.Errorf("error %v does not wrap wav.ErrUnsupported", err)
			}

			// Reset must reject exactly what NewEncoder rejects.
			var e pcm.Encoder
			if rerr := e.Reset(&buf, tc.cfg); rerr == nil {
				t.Errorf("Reset accepted %+v that NewEncoder rejected", tc.cfg)
			}

			// So must the one-shot path.
			if oerr := pcm.EncodeInterleaved(&buf, tc.cfg, nil); oerr == nil {
				t.Errorf("EncodeInterleaved accepted %+v that NewEncoder rejected", tc.cfg)
			}
		})
	}
}

// TestEncoderNilWriter checks that a nil sink is reported, not dereferenced.
func TestEncoderNilWriter(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a nil writer panicked: %v", r)
		}
	}()

	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	if _, err := pcm.NewEncoder(nil, cfg); err == nil {
		t.Error("NewEncoder accepted a nil writer")
	}
	var e pcm.Encoder
	if err := e.Reset(nil, cfg); err == nil {
		t.Error("Reset accepted a nil writer")
	}
	if err := pcm.EncodeInterleaved(nil, cfg, nil); err == nil {
		t.Error("EncodeInterleaved accepted a nil writer")
	}
}

// guidPCM is KSDATAFORMAT_SUBTYPE_PCM on the wire.
var guidPCM = []byte{
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// guidFloat is KSDATAFORMAT_SUBTYPE_IEEE_FLOAT on the wire.
var guidFloat = []byte{
	0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// TestEncoderFmtChunkShape checks the automatic promotion to the extensible fmt
// form and the sizes of the three valid fmt payloads.
func TestEncoderFmtChunkShape(t *testing.T) {
	const (
		tagPCM        = 0x0001
		tagIEEEFloat  = 0x0003
		tagExtensible = 0xFFFE
	)
	cases := []struct {
		name        string
		cfg         pcm.Config
		wantPayload int
		wantTag     uint16
		wantCBSize  int // -1 when the field is absent
		wantGUID    []byte
		wantFact    bool
	}{
		{
			name:        "s16 mono stays bare",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1},
			wantPayload: 16, wantTag: tagPCM, wantCBSize: -1,
		},
		{
			name:        "s16 stereo stays bare",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2},
			wantPayload: 16, wantTag: tagPCM, wantCBSize: -1,
		},
		{
			name:        "u8 mono stays bare",
			cfg:         pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1},
			wantPayload: 16, wantTag: tagPCM, wantCBSize: -1,
		},
		{
			name:        "s24 promotes on depth",
			cfg:         pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 2},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidPCM,
		},
		{
			name:        "s32 promotes on depth",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidPCM,
		},
		{
			name:        "six channels promote on width",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 6},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidPCM,
		},
		{
			name:        "explicit Extensible promotes",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Extensible: true},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidPCM,
		},
		{
			name:        "a channel mask promotes",
			cfg:         pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2, ChannelMask: 0x3},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidPCM,
		},
		{
			name: "f32 stereo takes the 18 byte form",
			cfg: pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2,
				Format: wav.SampleFormatFloat},
			wantPayload: 18, wantTag: tagIEEEFloat, wantCBSize: 0, wantFact: true,
		},
		{
			name: "f64 mono takes the 18 byte form",
			cfg: pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1,
				Format: wav.SampleFormatFloat},
			wantPayload: 18, wantTag: tagIEEEFloat, wantCBSize: 0, wantFact: true,
		},
		{
			name: "f32 six channels promotes and keeps the float guid",
			cfg: pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 6,
				Format: wav.SampleFormatFloat},
			wantPayload: 40, wantTag: tagExtensible, wantCBSize: 22, wantGUID: guidFloat,
			wantFact: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			width := (tc.cfg.BitDepth + 7) / 8
			payload := make([]byte, 4*tc.cfg.Channels*width)
			if err := pcm.EncodeInterleaved(&buf, tc.cfg, payload); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}
			b := buf.Bytes()

			span := requireChunk(t, b, "fmt ")
			if span.size != tc.wantPayload {
				t.Errorf("fmt payload size: got %d want %d", span.size, tc.wantPayload)
			}
			if tag := uint16(u32At(t, b, span.payload) & 0xFFFF); tag != tc.wantTag {
				t.Errorf("format tag: got %#04x want %#04x", tag, tc.wantTag)
			}
			if tc.wantCBSize >= 0 {
				if span.size < 18 {
					t.Fatalf("fmt payload of %d bytes has no cbSize field", span.size)
				}
				cb := int(u32At(t, b, span.payload+16) & 0xFFFF)
				if cb != tc.wantCBSize {
					t.Errorf("cbSize: got %d want %d", cb, tc.wantCBSize)
				}
			}
			if tc.wantGUID != nil {
				got := b[span.payload+24 : span.payload+40]
				if !bytes.Equal(got, tc.wantGUID) {
					t.Errorf("SubFormat GUID: got % x want % x", got, tc.wantGUID)
				}
			}

			spans := walkChunks(t, b)
			_, haveFact := spans["fact"]
			if haveFact != tc.wantFact {
				t.Errorf("fact chunk present = %v, want %v", haveFact, tc.wantFact)
			}

			// A well-formed header must round trip through our own parser.
			d, err := pcm.NewDecoder(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("NewDecoder on our own output: %v", err)
			}
			info := d.Info()
			if info.Channels != tc.cfg.Channels || info.BitDepth != tc.cfg.BitDepth {
				t.Errorf("parsed info %+v does not match config %+v", info, tc.cfg)
			}
			wantExtensible := tc.wantPayload == 40
			if info.Extensible != wantExtensible {
				t.Errorf("Extensible: got %v want %v", info.Extensible, wantExtensible)
			}
		})
	}
}

// TestEncoderTotalFramesMismatch checks the promise a declared frame count makes.
func TestEncoderTotalFramesMismatch(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, TotalFrames: 100}
	var buf bytes.Buffer
	e, err := pcm.NewEncoder(&buf, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write(pattern(50 * 2)); err != nil {
		t.Fatal(err)
	}
	err = e.Close()
	if err == nil {
		t.Fatal("Close accepted 50 frames against a declared 100")
	}
	msg := err.Error()
	if !strings.Contains(msg, "50") || !strings.Contains(msg, "100") {
		t.Errorf("Close error names neither count: %v", err)
	}

	t.Run("an exact match closes cleanly", func(t *testing.T) {
		var ok bytes.Buffer
		e, err := pcm.NewEncoder(&ok, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(pattern(100 * 2)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if span := requireChunk(t, ok.Bytes(), "data"); span.size != 200 {
			t.Errorf("data size field: got %d want 200", span.size)
		}
	})
}

// TestEncoderStreamInfo checks that the encoder describes what it has written.
func TestEncoderStreamInfo(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info := e.StreamInfo(); info.TotalFrames != 0 {
		t.Errorf("TotalFrames before any Write: got %d want 0", info.TotalFrames)
	}
	if _, err := e.Write(pattern(60)); err != nil {
		t.Fatal(err)
	}
	info := e.StreamInfo()
	if info.TotalFrames != 10 {
		t.Errorf("TotalFrames: got %d want 10", info.TotalFrames)
	}
	if info.SampleRate != 48000 || info.Channels != 2 || info.BitDepth != 24 {
		t.Errorf("StreamInfo: got %+v", info)
	}
	if info.SourceBitDepth != 24 || info.SourceFormat != wav.SampleFormatPCM {
		t.Errorf("source fields: got %+v", info)
	}
	if info.Container != wav.ContainerRIFF {
		t.Errorf("Container: got %v want RIFF", info.Container)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestEncoderEmptyWrite checks that a zero-length Write is a no-op.
func TestEncoderEmptyWrite(t *testing.T) {
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	headerLen := len(sink.b)
	n, err := e.Write(nil)
	if n != 0 || err != nil {
		t.Errorf("Write(nil): got (%d, %v), want (0, nil)", n, err)
	}
	n, err = e.Write([]byte{})
	if n != 0 || err != nil {
		t.Errorf("Write(empty): got (%d, %v), want (0, nil)", n, err)
	}
	if len(sink.b) != headerLen {
		t.Errorf("an empty Write emitted %d bytes", len(sink.b)-headerLen)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if span := requireChunk(t, sink.b, "data"); span.size != 0 {
		t.Errorf("data size field: got %d want 0", span.size)
	}
}

// assertDecodes checks that b is a stream matching cfg and holding want.
func assertDecodes(tb testing.TB, b []byte, cfg pcm.Config, want []byte) {
	tb.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		tb.Fatalf("NewDecoder: %v", err)
	}
	info := d.Info()
	if info.SampleRate != cfg.SampleRate || info.Channels != cfg.Channels ||
		info.BitDepth != cfg.BitDepth || info.Format != cfg.Format {
		tb.Errorf("stream info %+v does not match config %+v", info, cfg)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		tb.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		tb.Errorf("payload: got %d bytes want %d", len(got), len(want))
	}
}

// TestEncoderRefusesRatesTheDecoderRefuses pins the two halves of the sample
// rate ceiling together at the level a caller sees them. The reader bounds a
// declared rate so that StreamInfo.SampleRate can promise to be positive on a
// 32-bit target; without the matching bound here the encoder would happily
// write a file this package then refuses to read.
//
// Eight-bit mono is the geometry that reaches the gap: a frame is one byte, so
// the fmt chunk's derived byte rate equals the sample rate and does not
// overflow its own 32-bit field before the rate overflows the ceiling. At any
// wider frame the byte rate overflows first and hides the problem.
func TestEncoderRefusesRatesTheDecoderRefuses(t *testing.T) {
	t.Parallel()

	const ceiling = int64(math.MaxInt32)
	for _, rate := range []int64{48000, ceiling, ceiling + 1, math.MaxUint32} {
		// A rate past the ceiling is not expressible as an int on a 32-bit
		// target, so there it cannot be asked for at all.
		if int64(int(rate)) != rate {
			continue
		}
		cfg := pcm.Config{SampleRate: int(rate), BitDepth: 8, Channels: 1}

		var buf bytes.Buffer
		err := pcm.EncodeInterleaved(&buf, cfg, []byte{1, 2, 3, 4})
		if rate > ceiling {
			if err == nil {
				t.Errorf("rate %d: encoder wrote a file the decoder refuses", rate)
			}
			continue
		}
		if err != nil {
			t.Fatalf("rate %d: encoder refused a rate the decoder accepts: %v", rate, err)
		}

		info, _, derr := pcm.DecodeInterleaved(buf.Bytes())
		if derr != nil {
			t.Fatalf("rate %d: encoder wrote a file the decoder refuses: %v", rate, derr)
		}
		if int64(info.SampleRate) != rate {
			t.Errorf("rate %d round tripped as %d", rate, info.SampleRate)
		}
	}
}
