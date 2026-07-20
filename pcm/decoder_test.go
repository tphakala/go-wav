package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// encodeFixture builds a stream holding payload and returns its bytes.
func encodeFixture(tb testing.TB, cfg pcm.Config, payload []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
		tb.Fatalf("EncodeInterleaved: %v", err)
	}
	return buf.Bytes()
}

// TestDecoderInfoDescribesRead is the central decoder invariant: Info must
// describe the bytes Read yields, never the bytes on disk.
func TestDecoderInfoDescribesRead(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	file := encodeFixture(t, cfg, pattern(24*6))

	t.Run("pass-through reports the stored width", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatal(err)
		}
		info := d.Info()
		if info.BitDepth != 24 || info.SourceBitDepth != 24 {
			t.Errorf("BitDepth %d, SourceBitDepth %d, want 24 and 24",
				info.BitDepth, info.SourceBitDepth)
		}
		if info.Format != wav.SampleFormatPCM || info.SourceFormat != wav.SampleFormatPCM {
			t.Errorf("Format %v, SourceFormat %v, want pcm and pcm", info.Format, info.SourceFormat)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if want := int(info.TotalFrames) * info.BytesPerFrame(); len(got) != want {
			t.Errorf("Read yielded %d bytes, Info describes %d", len(got), want)
		}
	})

	t.Run("converting reports the converted width", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(32))
		if err != nil {
			t.Fatal(err)
		}
		info := d.Info()
		if info.BitDepth != 32 {
			t.Errorf("BitDepth: got %d want 32 (the converted width)", info.BitDepth)
		}
		if info.SourceBitDepth != 24 {
			t.Errorf("SourceBitDepth: got %d want 24 (the stored width)", info.SourceBitDepth)
		}
		if info.Format != wav.SampleFormatPCM {
			t.Errorf("Format: got %v want pcm", info.Format)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if want := int(info.TotalFrames) * info.BytesPerFrame(); len(got) != want {
			t.Errorf("Read yielded %d bytes, Info describes %d", len(got), want)
		}
	})

	t.Run("converting a float source reports pcm", func(t *testing.T) {
		fcfg := pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1,
			Format: wav.SampleFormatFloat}
		ffile := encodeFixture(t, fcfg, floatPattern(64, 32))
		d, err := pcm.NewDecoder(bytes.NewReader(ffile), pcm.WithConvertTo(16))
		if err != nil {
			t.Fatal(err)
		}
		info := d.Info()
		if info.Format != wav.SampleFormatPCM {
			t.Errorf("Format: got %v want pcm", info.Format)
		}
		if info.SourceFormat != wav.SampleFormatFloat {
			t.Errorf("SourceFormat: got %v want float", info.SourceFormat)
		}
		if info.BitDepth != 16 || info.SourceBitDepth != 32 {
			t.Errorf("BitDepth %d SourceBitDepth %d, want 16 and 32",
				info.BitDepth, info.SourceBitDepth)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if want := int(info.TotalFrames) * info.BytesPerFrame(); len(got) != want {
			t.Errorf("Read yielded %d bytes, Info describes %d", len(got), want)
		}
	})
}

// TestDecoderConvertToEveryPair checks the output length of every supported
// source and target combination.
func TestDecoderConvertToEveryPair(t *testing.T) {
	sources := []struct {
		name string
		cfg  pcm.Config
	}{
		{"u8", pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 2}},
		{"s16", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}},
		{"s24", pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}},
		{"s32", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2}},
		{"f32", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2,
			Format: wav.SampleFormatFloat}},
		{"f64", pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 2,
			Format: wav.SampleFormatFloat}},
	}
	targets := []int{8, 16, 24, 32}
	const frames = 40

	for _, src := range sources {
		for _, target := range targets {
			t.Run(fmt.Sprintf("%s to s%d", src.name, target), func(t *testing.T) {
				width := (src.cfg.BitDepth + 7) / 8
				payload := pattern(frames * src.cfg.Channels * width)
				if src.cfg.Format == wav.SampleFormatFloat {
					payload = floatPattern(frames*src.cfg.Channels, src.cfg.BitDepth)
				}
				file := encodeFixture(t, src.cfg, payload)

				d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(target))
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				want := frames * src.cfg.Channels * ((target + 7) / 8)
				if len(got) != want {
					t.Errorf("converted length: got %d want %d", len(got), want)
				}
				if info := d.Info(); info.BitDepth != target {
					t.Errorf("Info().BitDepth: got %d want %d", info.BitDepth, target)
				}
			})
		}
	}
}

// TestDecoderConvertKnownValues spot-checks conversions whose results are fixed
// by the format rather than by taste.
func TestDecoderConvertKnownValues(t *testing.T) {
	t.Run("u8 to s16", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
		file := encodeFixture(t, cfg, []byte{128, 255, 0, 129, 127})
		got := readAllConverted(t, file, 16)
		want := []int16{0, 32512, -32768, 256, -256}
		assertInt16(t, got, want)
	})

	t.Run("s16 to u8", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 1}
		in := make([]byte, 8)
		for i, v := range []int16{0, 32767, -32768, 256} {
			binary.LittleEndian.PutUint16(in[i*2:], uint16(v))
		}
		file := encodeFixture(t, cfg, in)
		got := readAllConverted(t, file, 8)
		want := []byte{128, 255, 0, 129}
		if !bytes.Equal(got, want) {
			t.Errorf("got %v want %v", got, want)
		}
	})

	t.Run("s24 to s32", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 24, Channels: 1}
		// 1, -1 and full scale, packed little-endian in three bytes.
		in := []byte{
			0x01, 0x00, 0x00,
			0xFF, 0xFF, 0xFF,
			0xFF, 0xFF, 0x7F,
			0x00, 0x00, 0x80,
		}
		file := encodeFixture(t, cfg, in)
		got := readAllConverted(t, file, 32)
		want := []int32{1 << 8, -1 << 8, 0x7FFFFF << 8, -0x800000 << 8}
		assertInt32(t, got, want)
	})

	t.Run("s32 to s32 is a copy", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 32, Channels: 1}
		in := pattern(32)
		file := encodeFixture(t, cfg, in)
		got := readAllConverted(t, file, 32)
		if !bytes.Equal(got, in) {
			t.Errorf("an identity conversion changed the bytes")
		}
	})

	t.Run("f32 to s16", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 32, Channels: 1,
			Format: wav.SampleFormatFloat}
		vals := []float32{0, 1, -1, 0.5, -0.5, 2, float32(math.NaN())}
		in := make([]byte, len(vals)*4)
		for i, v := range vals {
			binary.LittleEndian.PutUint32(in[i*4:], math.Float32bits(v))
		}
		file := encodeFixture(t, cfg, in)
		got := readAllConverted(t, file, 16)
		want := []int16{0, 32767, -32768, 16384, -16384, 32767, 0}
		assertInt16(t, got, want)
	})

	t.Run("f64 to s32", func(t *testing.T) {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 64, Channels: 1,
			Format: wav.SampleFormatFloat}
		vals := []float64{0, 0.25, -0.25, math.Inf(1), math.Inf(-1)}
		in := make([]byte, len(vals)*8)
		for i, v := range vals {
			binary.LittleEndian.PutUint64(in[i*8:], math.Float64bits(v))
		}
		file := encodeFixture(t, cfg, in)
		got := readAllConverted(t, file, 32)
		want := []int32{0, 1 << 29, -(1 << 29), math.MaxInt32, math.MinInt32}
		assertInt32(t, got, want)
	})
}

func readAllConverted(tb testing.TB, file []byte, depth int) []byte {
	tb.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(depth))
	if err != nil {
		tb.Fatalf("NewDecoder: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		tb.Fatalf("ReadAll: %v", err)
	}
	return got
}

func assertInt16(tb testing.TB, got []byte, want []int16) {
	tb.Helper()
	if len(got) != len(want)*2 {
		tb.Fatalf("got %d bytes, want %d", len(got), len(want)*2)
	}
	for i, w := range want {
		g := int16(binary.LittleEndian.Uint16(got[i*2:]))
		if g != w {
			tb.Errorf("sample %d: got %d want %d", i, g, w)
		}
	}
}

func assertInt32(tb testing.TB, got []byte, want []int32) {
	tb.Helper()
	if len(got) != len(want)*4 {
		tb.Fatalf("got %d bytes, want %d", len(got), len(want)*4)
	}
	for i, w := range want {
		g := int32(binary.LittleEndian.Uint32(got[i*4:]))
		if g != w {
			tb.Errorf("sample %d: got %d want %d", i, g, w)
		}
	}
}

// TestDecoderConvertToInvalidDepth checks that an unsupported target width is
// rejected at construction rather than at the first Read.
func TestDecoderConvertToInvalidDepth(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 1}
	file := encodeFixture(t, cfg, pattern(16))

	for _, depth := range []int{0, 1, 12, 20, 64, -8} {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			_, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(depth))
			if err == nil {
				t.Fatalf("NewDecoder accepted WithConvertTo(%d)", depth)
			}
		})
	}
}

// TestDecoderPassThroughEightBitIsUnsigned pins the documented trap: without a
// conversion, 8-bit data comes back exactly as stored, which is unsigned.
func TestDecoderPassThroughEightBitIsUnsigned(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
	src := []byte{0, 1, 127, 128, 129, 254, 255}
	file := encodeFixture(t, cfg, src)

	d, err := pcm.NewDecoder(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("pass-through 8-bit: got %v want the unsigned originals %v", got, src)
	}
}

// TestDecoderIgnoreLength checks the recovery path for a header whose data size
// field understates the audio actually present.
func TestDecoderIgnoreLength(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 1}
	src := pattern(200)
	file := encodeFixture(t, cfg, src)

	span := requireChunk(t, file, "data")
	if span.size != len(src) {
		t.Fatalf("fixture data size field is %d, want %d", span.size, len(src))
	}
	// Lie: claim only the first 40 bytes are audio.
	lying := bytes.Clone(file)
	binary.LittleEndian.PutUint32(lying[span.payload-4:], 40)

	t.Run("without the option only the declared bytes come back", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(lying))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 40 {
			t.Errorf("got %d bytes, want the declared 40", len(got))
		}
		if !bytes.Equal(got, src[:40]) {
			t.Error("the declared prefix does not match the source")
		}
		if frames := d.Info().TotalFrames; frames != 20 {
			t.Errorf("TotalFrames: got %d want 20", frames)
		}
	})

	t.Run("with the option everything to EOF comes back", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(lying), pcm.WithIgnoreLength())
		if err != nil {
			t.Fatal(err)
		}
		if frames := d.Info().TotalFrames; frames != 0 {
			t.Errorf("TotalFrames under WithIgnoreLength: got %d want 0 (unknown)", frames)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, src) {
			t.Errorf("got %d bytes, want the whole %d byte payload", len(got), len(src))
		}
	})

	t.Run("with the option and a conversion", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(lying), pcm.WithIgnoreLength(),
			pcm.WithConvertTo(32))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(src)*2 {
			t.Errorf("got %d bytes, want %d", len(got), len(src)*2)
		}
	})
}

// TestDecoderReadZeroLengthBuffer checks the degenerate Read.
func TestDecoderReadZeroLengthBuffer(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 1}
	file := encodeFixture(t, cfg, pattern(64))

	for _, opts := range [][]pcm.Option{nil, {pcm.WithConvertTo(32)}} {
		d, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
		if err != nil {
			t.Fatal(err)
		}
		n, err := d.Read(nil)
		if n != 0 || err != nil {
			t.Errorf("Read(nil): got (%d, %v), want (0, nil)", n, err)
		}
		n, err = d.Read([]byte{})
		if n != 0 || err != nil {
			t.Errorf("Read(empty): got (%d, %v), want (0, nil)", n, err)
		}
	}
}

// TestDecoderWriteTo checks that the io.WriterTo path yields the same bytes as
// a plain drain.
func TestDecoderWriteTo(t *testing.T) {
	cases := []struct {
		name string
		cfg  pcm.Config
		opts []pcm.Option
	}{
		{"pass-through", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}, nil},
		{"converted", pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2},
			[]pcm.Option{pcm.WithConvertTo(16)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			width := (tc.cfg.BitDepth + 7) / 8
			file := encodeFixture(t, tc.cfg, pattern(1000*tc.cfg.Channels*width))

			fresh, err := pcm.NewDecoder(bytes.NewReader(file), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			want, err := io.ReadAll(fresh)
			if err != nil {
				t.Fatal(err)
			}

			d, err := pcm.NewDecoder(bytes.NewReader(file), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			n, err := d.WriteTo(&buf)
			if err != nil {
				t.Fatalf("WriteTo: %v", err)
			}
			if n != int64(buf.Len()) {
				t.Errorf("WriteTo reported %d bytes but wrote %d", n, buf.Len())
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("WriteTo produced %d bytes, ReadAll produced %d", buf.Len(), len(want))
			}

			// io.Copy must take the same path and reach the same result.
			c, err := pcm.NewDecoder(bytes.NewReader(file), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			var copied bytes.Buffer
			cn, err := io.Copy(&copied, c)
			if err != nil {
				t.Fatalf("io.Copy: %v", err)
			}
			if cn != int64(len(want)) || !bytes.Equal(copied.Bytes(), want) {
				t.Errorf("io.Copy produced %d bytes, want %d", cn, len(want))
			}
		})
	}
}

// TestDecoderSeekToFrame exercises seeking on a seekable source.
func TestDecoderSeekToFrame(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}
	const frames = 500
	perFrame := 4
	src := pattern(frames * perFrame)
	file := encodeFixture(t, cfg, src)

	cases := []struct {
		name      string
		frame     int64
		wantFrame int64
	}{
		{"first frame", 0, 0},
		{"second frame", 1, 1},
		{"middle", frames / 2, frames / 2},
		{"last frame", frames - 1, frames - 1},
		{"exactly the end", frames, frames},
		{"past the end", frames * 10, frames},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := pcm.NewDecoder(bytes.NewReader(file))
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.SeekToFrame(tc.frame)
			if err != nil {
				t.Fatalf("SeekToFrame(%d): %v", tc.frame, err)
			}
			if got != tc.wantFrame {
				t.Errorf("SeekToFrame(%d) reached frame %d, want %d", tc.frame, got, tc.wantFrame)
			}
			rest, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll after seek: %v", err)
			}
			want := src[tc.wantFrame*int64(perFrame):]
			if !bytes.Equal(rest, want) {
				t.Errorf("after seeking to %d: got %d bytes want %d", tc.wantFrame, len(rest), len(want))
			}
		})
	}

	t.Run("seek after partial read", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 137)
		if _, err := io.ReadFull(d, buf); err != nil {
			t.Fatal(err)
		}
		if _, err := d.SeekToFrame(10); err != nil {
			t.Fatalf("SeekToFrame after a partial read: %v", err)
		}
		got := make([]byte, perFrame)
		if _, err := io.ReadFull(d, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, src[10*perFrame:11*perFrame]) {
			t.Errorf("frame 10: got %v want %v", got, src[10*perFrame:11*perFrame])
		}
	})

	t.Run("negative frame is an error", func(t *testing.T) {
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.SeekToFrame(-1); err == nil {
			t.Error("SeekToFrame(-1) returned no error")
		}
	})

	t.Run("a source that cannot seek", func(t *testing.T) {
		d, err := pcm.NewDecoder(nonSeekReader{r: bytes.NewReader(file)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = d.SeekToFrame(10)
		if !errors.Is(err, wav.ErrSeekUnsupported) {
			t.Errorf("got %v, want wav.ErrSeekUnsupported", err)
		}
		// The decoder must still be usable for sequential reading.
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll after a refused seek: %v", err)
		}
		if !bytes.Equal(got, src) {
			t.Errorf("a refused seek disturbed the stream: got %d bytes want %d", len(got), len(src))
		}
	})
}

// TestDecoderTruncatedFile checks that a file claiming more audio than it holds
// ends cleanly rather than as damage.
func TestDecoderTruncatedFile(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(400)
	file := encodeFixture(t, cfg, src)
	span := requireChunk(t, file, "data")

	for _, keep := range []int{0, 1, 7, 100, 399} {
		t.Run(fmt.Sprintf("%d audio bytes present", keep), func(t *testing.T) {
			truncated := file[:span.payload+keep]

			for _, opts := range [][]pcm.Option{nil, {pcm.WithConvertTo(32)}} {
				d, err := pcm.NewDecoder(bytes.NewReader(truncated), opts...)
				if err != nil {
					t.Fatalf("NewDecoder on a truncated file: %v", err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				if errors.Is(err, io.ErrUnexpectedEOF) {
					t.Error("a truncated file reported io.ErrUnexpectedEOF")
				}
				// A trailing partial sample is dropped, so compare on whole
				// samples only.
				if len(opts) == 0 && !bytes.HasPrefix(src[:keep], got) &&
					!bytes.Equal(got, src[:keep]) {
					t.Errorf("got %d bytes that are not a prefix of the %d present", len(got), keep)
				}
				// A second Read must keep reporting the end of the stream.
				n, rerr := d.Read(make([]byte, 8))
				if n != 0 || !errors.Is(rerr, io.EOF) {
					t.Errorf("Read after the end: got (%d, %v), want (0, io.EOF)", n, rerr)
				}
			}
		})
	}
}

// TestDecoderNilReader checks that a nil source is reported, not dereferenced.
func TestDecoderNilReader(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a nil reader panicked: %v", r)
		}
	}()
	if _, err := pcm.NewDecoder(nil); err == nil {
		t.Error("NewDecoder accepted a nil reader")
	}
	var d pcm.Decoder
	if err := d.Reset(nil); err == nil {
		t.Error("Reset accepted a nil reader")
	}
}

// TestDecoderZeroValue checks that an uninitialised Decoder reports rather than
// panics.
func TestDecoderZeroValue(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("zero-value Decoder panicked: %v", r)
		}
	}()

	var d pcm.Decoder
	n, err := d.Read(make([]byte, 16))
	if n != 0 {
		t.Errorf("Read on a zero-value Decoder returned %d bytes", n)
	}
	if err == nil {
		t.Error("Read on a zero-value Decoder returned no error")
	}
	if info := d.Info(); info.SampleRate != 0 {
		t.Errorf("Info on a zero-value Decoder: %+v", info)
	}
	if _, err := d.SeekToFrame(0); err == nil {
		t.Error("SeekToFrame on a zero-value Decoder returned no error")
	}
	if _, err := d.WriteTo(io.Discard); err != nil && !errors.Is(err, io.EOF) {
		t.Logf("WriteTo on a zero-value Decoder: %v", err)
	}
}

// TestDecoderResetAfterFailedParse checks that a decoder whose Reset failed does
// not stay usable against the stale stream.
func TestDecoderResetAfterFailedParse(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := pattern(64)
	good := encodeFixture(t, cfg, src)

	d, err := pcm.NewDecoder(bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 8)
	if _, err := io.ReadFull(d, first); err != nil {
		t.Fatal(err)
	}

	if err := d.Reset(bytes.NewReader([]byte("this is not a wav file at all"))); err == nil {
		t.Fatal("Reset accepted a stream that is not RIFF")
	} else if !errors.Is(err, wav.ErrNotRIFF) {
		t.Errorf("Reset error: got %v, want wav.ErrNotRIFF", err)
	}

	n, rerr := d.Read(make([]byte, 8))
	if rerr == nil {
		t.Errorf("Read after a failed Reset succeeded with %d bytes from the stale stream", n)
	}
	if _, serr := d.SeekToFrame(0); serr == nil {
		t.Error("SeekToFrame after a failed Reset returned no error")
	}
	if _, werr := d.WriteTo(io.Discard); werr == nil {
		t.Error("WriteTo after a failed Reset returned no error")
	}

	// A subsequent successful Reset must fully recover the decoder.
	if err := d.Reset(bytes.NewReader(good)); err != nil {
		t.Fatalf("Reset back onto a good stream: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("after recovery: got %d bytes want %d", len(got), len(src))
	}
}

// TestDecoderRejectsMalformedStreams checks the sentinels for streams that are
// not WAVE at all or are structurally broken.
func TestDecoderRejectsMalformedStreams(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	good := encodeFixture(t, cfg, pattern(64))

	noFmt := bytes.Clone(good[:12])
	noFmt = append(noFmt, 'd', 'a', 't', 'a', 0, 0, 0, 0)

	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, wav.ErrNotRIFF},
		{"short of a file header", good[:8], wav.ErrNotRIFF},
		{"wrong magic", append([]byte("XXXX"), good[4:]...), wav.ErrNotRIFF},
		{"wrong form type", append(bytes.Clone(good[:8]),
			append([]byte("AVI "), good[12:]...)...), wav.ErrNotRIFF},
		{"no fmt chunk", noFmt, wav.ErrCorruptStream},
		{"header only", good[:12], wav.ErrCorruptStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pcm.NewDecoder(bytes.NewReader(tc.in))
			if err == nil {
				t.Fatal("NewDecoder accepted it")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestDecoderRF64WithoutDS64 checks that an RF64 stream missing its ds64 chunk
// is reported rather than silently read with sentinel sizes.
func TestDecoderRF64WithoutDS64(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	good := encodeFixture(t, cfg, pattern(64))
	broken := bytes.Clone(good)
	copy(broken[0:4], "RF64")

	_, err := pcm.NewDecoder(bytes.NewReader(broken))
	if !errors.Is(err, wav.ErrCorruptStream) {
		t.Errorf("got %v, want wav.ErrCorruptStream", err)
	}
}

// TestDecoderReset reuses a decoder across streams.
func TestDecoderReset(t *testing.T) {
	first := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	second := pcm.Config{SampleRate: 8000, BitDepth: 24, Channels: 2}
	srcA := pattern(64)
	srcB := pattern(120)

	d, err := pcm.NewDecoder(bytes.NewReader(encodeFixture(t, first, srcA)))
	if err != nil {
		t.Fatal(err)
	}
	gotA, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotA, srcA) {
		t.Errorf("first stream: got %d bytes want %d", len(gotA), len(srcA))
	}

	if err := d.Reset(bytes.NewReader(encodeFixture(t, second, srcB))); err != nil {
		t.Fatal(err)
	}
	if info := d.Info(); info.BitDepth != 24 || info.Channels != 2 || info.SampleRate != 8000 {
		t.Errorf("Info after Reset: %+v", info)
	}
	gotB, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotB, srcB) {
		t.Errorf("second stream: got %d bytes want %d", len(gotB), len(srcB))
	}

	// Options must not survive a Reset that does not repeat them.
	if err := d.Reset(bytes.NewReader(encodeFixture(t, first, srcA)), pcm.WithConvertTo(32)); err != nil {
		t.Fatal(err)
	}
	if got := d.Info().BitDepth; got != 32 {
		t.Errorf("BitDepth under a converting Reset: got %d want 32", got)
	}
	if err := d.Reset(bytes.NewReader(encodeFixture(t, first, srcA))); err != nil {
		t.Fatal(err)
	}
	if got := d.Info().BitDepth; got != 16 {
		t.Errorf("BitDepth after a plain Reset: got %d want 16, the option leaked", got)
	}
}

// TestDecoderSmallReads checks that a caller reading a byte at a time gets the
// same stream as one reading in bulk.
func TestDecoderSmallReads(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 1}
	src := pattern(90)
	file := encodeFixture(t, cfg, src)

	d, err := pcm.NewDecoder(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 1)
	for {
		n, rerr := d.Read(buf)
		got = append(got, buf[:n]...)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			t.Fatalf("Read: %v", rerr)
		}
	}
	if !bytes.Equal(got, src) {
		t.Errorf("one byte at a time: got %d bytes want %d", len(got), len(src))
	}
}
