package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// frameShape names a stream layout for the whole-frame tests. channels, when
// set, is patched into the fmt chunk after encoding: the writer cannot emit a
// frame wider than its 16-bit nBlockAlign field, the reader derives the real
// width from the channel count and accepts it. law, when set, builds a
// companded stream with two channels instead.
type frameShape struct {
	cfg      pcm.Config
	channels int
	law      uint16
}

// frameBytes is the stored width of one inter-channel frame.
func (s frameShape) frameBytes() int {
	if s.law != 0 {
		return 2
	}
	ch := s.cfg.Channels
	if s.channels != 0 {
		ch = s.channels
	}
	return s.cfg.BitDepth / 8 * ch
}

// build returns a stream holding exactly present bytes of audio whose data
// chunk size field says declared (or the 0xFFFFFFFF sentinel when declared is
// negative). An odd known-length payload gets its pad byte, as a writer would.
func (s frameShape) build(tb testing.TB, declared, present int) []byte {
	tb.Helper()
	var header []byte
	if s.law != 0 {
		header = compandedFile(tb, s.law, 2, 8000, nil)
	} else {
		one := encodeFixture(tb, s.cfg, pattern(s.cfg.BitDepth/8*s.cfg.Channels))
		header = one[:dataOffset(tb, one)]
	}
	out := bytes.Clone(header)
	out = append(out, pattern(present)...)
	if declared == present && present%2 != 0 {
		out = append(out, 0)
	}
	span := requireChunk(tb, out, "data")
	size := uint32(0xFFFFFFFF)
	if declared >= 0 {
		size = uint32(declared) //nolint:gosec // G115: test sizes are small.
	}
	binary.LittleEndian.PutUint32(out[span.payload-4:], size)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8)) //nolint:gosec // G115: test sizes are small.
	if s.channels != 0 {
		binary.LittleEndian.PutUint16(out[requireChunk(tb, out, "fmt ").payload+2:], uint16(s.channels)) //nolint:gosec // G115: test channel counts are small.
	}
	return out
}

// readInCalls drains d through a fixed buffer size.
func readInCalls(d io.Reader, size int) ([]byte, error) {
	buf := make([]byte, size)
	var out []byte
	for {
		n, err := d.Read(buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// frameRow is one stream shape of TestDecodePathsYieldWholeFrames.
type frameRow struct {
	name     string
	shape    frameShape
	declared int // data size field; negative for unknown
	present  int // audio bytes actually in the stream
	frames   int // whole frames expected out
	complete bool
	opts     []pcm.Option
	variants []int // conversion depths to run each path with (0 is none)
}

// pathOutput is what one decode path returned for a stream.
type pathOutput struct {
	name string
	got  []byte
}

// decodeEveryPath runs every decode path over file and returns what each
// returned. A path that fails is a fatal test error, since every stream here
// decodes cleanly.
func decodeEveryPath(t *testing.T, file []byte, opts []pcm.Option, wantLen int) []pathOutput {
	t.Helper()
	fresh := func() *pcm.Decoder {
		d, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		return d
	}
	var outs []pathOutput
	add := func(name string, got []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		outs = append(outs, pathOutput{name, got})
	}

	got, err := io.ReadAll(fresh())
	add("io.ReadAll", got, err)

	var sink bytes.Buffer
	_, err = fresh().WriteTo(&sink)
	add("WriteTo", sink.Bytes(), err)

	for _, size := range []int{3, 64 << 10} {
		got, err := readInCalls(fresh(), size)
		add(fmt.Sprintf("Read in %d byte calls", size), got, err)
	}

	got, _, err = pcm.DecodeInterleaved(bytes.NewReader(file), opts...)
	add("DecodeInterleaved", got, err)

	// The limit counts output bytes: a ceiling equal to the whole frames must
	// succeed, and the fragment must not count against it.
	got, _, err = pcm.DecodeInterleavedLimit(bytes.NewReader(file), wantLen, opts...)
	add("DecodeInterleavedLimit at the exact length", got, err)

	got, _, err = pcm.DecodeInterleavedBytes(file, opts...)
	add("DecodeInterleavedBytes", got, err)
	return outs
}

// checkFrameRow decodes one row's stream through every path with one
// conversion depth and compares each result with a clean stream holding only
// the whole frames expected.
func checkFrameRow(t *testing.T, row *frameRow, depth int) {
	t.Helper()
	opts := slices.Clone(row.opts)
	if depth != 0 {
		opts = append(opts, pcm.WithConvertTo(depth))
	}
	file := row.shape.build(t, row.declared, row.present)

	// The reference is a clean stream holding exactly the whole frames
	// expected, decoded with the same options.
	cleanLen := row.frames * row.shape.frameBytes()
	rd, err := pcm.NewDecoder(bytes.NewReader(row.shape.build(t, cleanLen, cleanLen)), opts...)
	if err != nil {
		t.Fatalf("NewDecoder(clean): %v", err)
	}
	want, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll(clean): %v", err)
	}
	probe, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	info := probe.Info()
	outFrame := info.BytesPerFrame()
	if len(want) != row.frames*outFrame {
		t.Fatalf("reference holds %d bytes, want %d frames of %d", len(want), row.frames, outFrame)
	}

	for _, out := range decodeEveryPath(t, file, opts, len(want)) {
		if len(out.got)%outFrame != 0 {
			t.Errorf("%s: %d bytes is not a whole number of %d byte frames", out.name, len(out.got), outFrame)
		}
		if row.complete && uint64(len(out.got)/outFrame) != info.TotalFrames {
			t.Errorf("%s: %d frames, Info().TotalFrames is %d", out.name, len(out.got)/outFrame, info.TotalFrames)
		}
		if !bytes.Equal(out.got, want) {
			t.Errorf("%s: %d bytes differ from the %d byte whole-frame reference", out.name, len(out.got), len(want))
		}
	}

	// One less than the whole frames must not fit.
	if len(want) > 0 {
		_, _, err = pcm.DecodeInterleavedLimit(bytes.NewReader(file), len(want)-1, opts...)
		if !errors.Is(err, pcm.ErrDecodeLimit) {
			t.Errorf("DecodeInterleavedLimit(%d) error = %v, want ErrDecodeLimit", len(want)-1, err)
		}
	}

	// The pass-through result is still a window onto the input that ends at
	// the last whole frame.
	got, _, err := pcm.DecodeInterleavedBytes(file, opts...)
	if err != nil {
		t.Fatalf("DecodeInterleavedBytes: %v", err)
	}
	if cap(got) != len(got) {
		t.Errorf("DecodeInterleavedBytes: cap %d, want it to end at the length %d", cap(got), len(got))
	}
	if depth == 0 && row.shape.law == 0 && len(got) > 0 && &got[0] != &file[dataOffset(t, file)] {
		t.Error("DecodeInterleavedBytes: pass-through result no longer aliases the input")
	}
}

// TestDecodePathsYieldWholeFrames pins that no decode path returns part of an
// inter-channel frame, whatever ends the audio mid-frame (a data chunk size
// that is not a multiple of the frame, a source that runs out) and whichever
// path and option reads it. A ragged stream must decode to the whole-frame
// prefix a clean stream of the same audio gives. On the unfixed code every
// ragged row fails.
func TestDecodePathsYieldWholeFrames(t *testing.T) {
	t.Parallel()

	const n = 30000 // enough that 64 KiB reads cross several windows
	s16 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}}
	s24 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}}
	s24x6 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 6}}
	u8mono := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 1}}
	u8st := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 2}}
	f32 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}}
	wide := frameShape{
		cfg:      pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1, Format: wav.SampleFormatFloat},
		channels: 8193, // a 65544 byte frame, wider than the window and the conversion batch
	}
	mulaw := frameShape{law: 0x0007}

	raw, to16 := []int{0}, []int{16}
	both := []int{0, 16}
	ignoreLength := []pcm.Option{pcm.WithIgnoreLength()}

	rows := []frameRow{
		{"s16 stereo known, 2 bytes over", s16, n*4 + 2, n*4 + 2, n, true, nil, both},
		{"s16 stereo unknown, cut 2 short", s16, -1, n*4 - 2, n - 1, false, nil, both},
		{"s24 stereo known, 3 bytes over", s24, n*6 + 3, n*6 + 3, n, true, nil, both},
		{"s16 stereo declares all, source 2 short", s16, n * 4, n*4 - 2, n - 1, false, nil, both},
		{"s24 6ch known, 7 bytes over", s24x6, n*18 + 7, n*18 + 7, n, true, nil, both},
		{"s24 6ch ignore length", s24x6, n*18 + 7, n*18 + 7, n, false, ignoreLength, both},
		{"u8 mono odd frame count", u8mono, n + 1, n + 1, n + 1, true, nil, both},
		{"f32 stereo whole frames only", f32, n * 8, n * 8, n, true, nil, both},
		{"f64 8193ch, 2 frames and 16 bytes", wide, 2*65544 + 16, 2*65544 + 16, 2, true, nil, both},
		{"s16 stereo known, under one frame", s16, 2, 2, 0, true, nil, both},
		{"s16 stereo unknown, under one frame", s16, -1, 2, 0, false, nil, both},
		{"u8 stereo known, 1 byte over", u8st, n*2 + 1, n*2 + 1, n, true, nil, to16},
		{"f32 stereo known, 4 bytes over", f32, n*8 + 4, n*8 + 4, n, true, nil, to16},
		{"mu-law stereo, odd code count", mulaw, 2*1000 + 1, 2*1000 + 1, 1000, true, nil, raw},
	}

	for _, row := range rows {
		for _, depth := range row.variants {
			t.Run(fmt.Sprintf("%s/convert%d", row.name, depth), func(t *testing.T) {
				t.Parallel()
				checkFrameRow(t, &row, depth)
			})
		}
	}
}

// convertOpts is the option list for one conversion depth (0 is none).
func convertOpts(depth int) []pcm.Option {
	if depth == 0 {
		return nil
	}
	return []pcm.Option{pcm.WithConvertTo(depth)}
}

// seekThenReadAll opens file, seeks to frame and returns the frame reached and
// everything Read yields after it.
func seekThenReadAll(t *testing.T, file []byte, frame int64, opts []pcm.Option) (reached int64, rest []byte) {
	t.Helper()
	d, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
	if err != nil {
		t.Fatal(err)
	}
	reached, err = d.SeekToFrame(frame)
	if err != nil {
		t.Fatalf("SeekToFrame(%d): %v", frame, err)
	}
	rest, err = io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	return reached, rest
}

// TestSeekToFrameThenReadYieldsWholeFrames pins that a seek does not restore
// the trailing fragment the decoder dropped at reset, and leaves no staged
// frame or held frame head from before the seek in front of the new position.
func TestSeekToFrameThenReadYieldsWholeFrames(t *testing.T) {
	t.Parallel()

	s16 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}}
	const frames = 1000
	payload := pattern(frames*4 + 2)

	t.Run("known length with a fragment", func(t *testing.T) {
		t.Parallel()
		file := s16.build(t, len(payload), len(payload))
		for _, depth := range []int{0, 16} {
			for _, c := range []struct {
				seek, reached int64
				frames        int
			}{{frames - 1, frames - 1, 1}, {frames, frames, 0}, {frames + 10, frames, 0}} {
				reached, rest := seekThenReadAll(t, file, c.seek, convertOpts(depth))
				want := payload[c.reached*4 : (c.reached+int64(c.frames))*4]
				if reached != c.reached || !bytes.Equal(rest, want) {
					t.Errorf("convert %d, seek %d: reached %d and read %d bytes, want frame %d and the %d byte whole-frame tail",
						depth, c.seek, reached, len(rest), c.reached, len(want))
				}
			}
		}
	})

	t.Run("unknown length cut short", func(t *testing.T) {
		t.Parallel()
		present := frames*4 - 2
		file := s16.build(t, -1, present)
		want := pattern(present)[(frames-2)*4 : (frames-1)*4]
		for _, depth := range []int{0, 16} {
			if _, rest := seekThenReadAll(t, file, frames-2, convertOpts(depth)); !bytes.Equal(rest, want) {
				t.Errorf("convert %d: read %v, want the one whole frame %v", depth, rest, want)
			}
		}
	})

	const n = 30000
	for _, c := range []struct {
		name  string
		shape frameShape
		buf   int
	}{
		// A 3 byte read of a 6 byte frame leaves a part-served staged frame.
		{"part-served staged frame", frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}}, 3},
		// The 58 byte float32 header leaves a header window remainder that is
		// not a frame multiple, so the first 64 KiB read holds a head.
		{"held frame head", frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}}, 64 << 10},
	} {
		t.Run(c.name+" is discarded", func(t *testing.T) {
			t.Parallel()
			fb := c.shape.frameBytes()
			file := c.shape.build(t, n*fb, n*fb)
			d, err := pcm.NewDecoder(bytes.NewReader(file))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Read(make([]byte, c.buf)); err != nil {
				t.Fatal(err)
			}
			if _, err := d.SeekToFrame(10); err != nil {
				t.Fatal(err)
			}
			rest, err := io.ReadAll(d)
			if err != nil {
				t.Fatal(err)
			}
			if want := pattern(n * fb)[10*fb:]; !bytes.Equal(rest, want) {
				t.Errorf("after the seek: %d bytes, want frames 10 onward (%d bytes)", len(rest), len(want))
			}
		})
	}
}

// TestDecoderReadRetryAfterErrorStaysAligned pins that a raw Read which fails
// on the source and is then retried loses no byte and stays frame-aligned: the
// head of a frame cut short by the error must be kept, not dropped. The source
// returns its bytes together with the error in one call, which is what reaches
// the bulk path's keep-on-error branch; small buffers reach the staged one.
func TestDecoderReadRetryAfterErrorStaysAligned(t *testing.T) {
	t.Parallel()

	s24 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}}
	const n = 30000
	file := s24.build(t, n*6, n*6)
	want := pattern(n * 6)
	dataStart := dataOffset(t, file)
	sentinel := errors.New("transient source error")

	for _, bufSize := range []int{3, 4096, 65536} {
		for _, after := range []int{7, 72005} { // data offsets past the header window, both mid-frame
			t.Run(fmt.Sprintf("buf%d/error at or after data offset %d", bufSize, after), func(t *testing.T) {
				t.Parallel()
				src := &errOnceReader{data: file, at: dataStart + after, err: sentinel}
				d, err := pcm.NewDecoder(src)
				if err != nil {
					t.Fatal(err)
				}
				// The header parse buffers ahead of the audio. Place the
				// error mid-frame relative to the data start, past that.
				for src.at <= src.pos || (src.at-dataStart)%6 == 0 {
					src.at++
				}
				src.armed = true
				var out []byte
				buf := make([]byte, bufSize)
				errs := 0
				for {
					m, err := d.Read(buf)
					out = append(out, buf[:m]...)
					if errors.Is(err, io.EOF) {
						break
					}
					if errors.Is(err, sentinel) {
						errs++
						continue
					}
					if err != nil {
						t.Fatalf("Read: %v", err)
					}
				}
				if errs != 1 {
					t.Fatalf("saw the source error %d times, want once", errs)
				}
				if !bytes.Equal(out, want) {
					t.Errorf("retry lost or shifted bytes: got %d, want %d", len(out), len(want))
				}
			})
		}
	}
}

// TestDecoderReadDoesNotAllocate pins that steady-state Read allocates
// nothing on either path, including where a frame head is held across reads or
// a frame is staged for a buffer smaller than it. testing.AllocsPerRun divides
// by its run count as integers, so each run drains the whole stream: an
// allocation per 64 KiB window then still shows as at least one per run.
func TestDecoderReadDoesNotAllocate(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in a parallel test.
	s24 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}}
	const n = 30000
	file := s24.build(t, n*6, n*6)

	for _, depth := range []int{0, 16} {
		for _, size := range []int{3, 4000} {
			t.Run(fmt.Sprintf("convert%d/buf%d", depth, size), func(t *testing.T) {
				var opts []pcm.Option
				if depth != 0 {
					opts = append(opts, pcm.WithConvertTo(depth))
				}
				d, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
				if err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, size)
				allocs := testing.AllocsPerRun(10, func() {
					if _, err := d.SeekToFrame(0); err != nil {
						t.Fatal(err)
					}
					for {
						if _, err := d.Read(buf); err != nil {
							return
						}
					}
				})
				if allocs != 0 {
					t.Errorf("a full read allocated %v times per run, want 0", allocs)
				}
			})
		}
	}
}

// TestDecoderReadDoesNotWaitOrSkipErrors pins two properties of the raw path
// that are easy to break while making it frame-whole. A Read that got whole
// frames from the source must return them without a second source read to
// finish a cut frame (a live source would block), and an error the buffered
// window deferred must still be reported, in order, when the next Read looks
// beneath the empty window.
func TestDecoderReadDoesNotWaitOrSkipErrors(t *testing.T) {
	t.Parallel()

	s16 := frameShape{cfg: pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}}
	const n = 30000
	file := s16.build(t, n*4, n*4)
	header := dataOffset(t, file)

	t.Run("a cut frame is not completed in the same call", func(t *testing.T) {
		t.Parallel()
		wouldBlock := errors.New("would block")
		for _, size := range []int{4096, 65536} {
			src := &scriptedReader{data: file, header: header, steps: []scriptStep{{10, nil}, {0, wouldBlock}}}
			d, err := pcm.NewDecoder(src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Read(make([]byte, size))
			if err != nil || got != 8 || src.calls != 1 {
				t.Errorf("Read(%d) = %d, %v after %d source reads; want 8 bytes, nil, 1 read", size, got, err, src.calls)
			}
		}
	})

	t.Run("a deferred source error is still reported", func(t *testing.T) {
		t.Parallel()
		errX := errors.New("deferred error")
		src := &scriptedReader{data: file, header: header, steps: []scriptStep{{4096, errX}}}
		d, err := pcm.NewDecoder(src)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := d.Read(make([]byte, 4096)); err != nil || got != 4096 {
			t.Fatalf("first Read = %d, %v; want 4096, nil", got, err)
		}
		if got, err := d.Read(make([]byte, 65536)); !errors.Is(err, errX) || got != 0 {
			t.Errorf("second Read = %d, %v; want 0 and the deferred error", got, err)
		}
	})
}
