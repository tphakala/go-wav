package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	pcm "github.com/tphakala/go-wav/pcm"
)

// seekCfg is the layout every fixture in this file shares: two-channel 16-bit
// audio, which keeps perFrame a round 4 bytes so the frame arithmetic in the
// test cases below stays readable.
var seekCfg = pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 2}

// seekPerFrame is seekCfg's bytes per frame: (16/8) bytes per sample times 2
// channels.
const seekPerFrame = 4

// seekFrames is how many frames of audio the fixtures in this file hold.
const seekFrames = 100

// unknownLengthRoute is one way a decoder can end up with an undeterminable
// data chunk length, as issue #8 lists: an explicit option, a data chunk size
// field of zero or the sentinel, or an RF64 stream whose ds64 was never
// patched.
type unknownLengthRoute struct {
	name string
	file func(tb testing.TB, src []byte) []byte
	opts []pcm.Option
}

// unknownLengthRoutes builds one fixture per route, each holding the same
// seekFrames frames of audio so every test in this file can share one set of
// expectations across all four.
func unknownLengthRoutes() []unknownLengthRoute {
	return []unknownLengthRoute{
		{
			name: "WithIgnoreLength",
			file: func(tb testing.TB, src []byte) []byte {
				tb.Helper()
				return encodeFixture(tb, seekCfg, src)
			},
			opts: []pcm.Option{pcm.WithIgnoreLength()},
		},
		{
			name: "data chunk size is zero",
			file: func(tb testing.TB, src []byte) []byte {
				tb.Helper()
				return patchDataSize(tb, encodeFixture(tb, seekCfg, src), 0)
			},
		},
		{
			name: "data chunk size is the sentinel",
			file: func(tb testing.TB, src []byte) []byte {
				tb.Helper()
				return patchDataSize(tb, encodeFixture(tb, seekCfg, src), 0xFFFFFFFF)
			},
		},
		{
			name: "RF64 ds64 never patched",
			file: unpatchedRF64,
		},
	}
}

// patchDataSize overwrites a stream's data chunk size field, producing the
// writer-crashed-before-patching case that the parser treats as unknown
// length even without WithIgnoreLength.
func patchDataSize(tb testing.TB, file []byte, size uint32) []byte {
	tb.Helper()
	span := requireChunk(tb, file, "data")
	binary.LittleEndian.PutUint32(file[span.payload-4:], size)
	return file
}

// unpatchedRF64 returns an RF64 stream whose ds64 chunk still holds the
// placeholder zero sizes a writer that crashed before Close would leave
// behind. A ds64 dataSize of zero is what the parser reads as "never
// stamped", not as a genuinely empty stream.
func unpatchedRF64(tb testing.TB, src []byte) []byte {
	tb.Helper()
	cfg := seekCfg
	cfg.RF64 = pcm.RF64Always
	file := encodeFixture(tb, cfg, src)
	span := requireChunk(tb, file, "ds64")
	binary.LittleEndian.PutUint64(file[span.payload+8:], 0)  // dataSize
	binary.LittleEndian.PutUint64(file[span.payload+16:], 0) // sampleCount
	return file
}

// rf64DeclaringOnlyACount returns an RF64 stream whose ds64 dataSize was never
// stamped but whose sampleCount was, which is the one shape where the decoder
// has a frame count and no byte length to corroborate it.
//
// unpatchedRF64 above zeroes both fields and so lands in the ordinary
// unknown-everything case. This one keeps the count, which is what
// riff.resolveFrames falls back to precisely because the size is missing.
func rf64DeclaringOnlyACount(tb testing.TB, src []byte, declared uint64) []byte {
	tb.Helper()
	cfg := seekCfg
	cfg.RF64 = pcm.RF64Always
	file := encodeFixture(tb, cfg, src)
	span := requireChunk(tb, file, "ds64")
	binary.LittleEndian.PutUint64(file[span.payload+8:], 0)         // dataSize: never stamped
	binary.LittleEndian.PutUint64(file[span.payload+16:], declared) // sampleCount: stamped
	return file
}

// TestDeclaredCountDoesNotBoundSeeks pins both halves of the disagreement issue
// #18 is about, so that neither can drift without the other being reconsidered.
//
// A stream can carry a frame count while carrying no byte length: the count
// comes from a ds64 sampleCount or a fact chunk, which resolveFrames consults
// *because* the size is missing. Info then reports a definite count and a
// definite duration, while SeekToFrame has no boundary to clamp against and so
// honours a request far beyond them.
//
// Both behaviours are deliberate and neither is being changed here. A declared
// count is a claim, so bounding seeks by it would refuse to reach real audio in
// a file that declares less than it holds, which is the interrupted-writer case
// the fallback exists to serve. What this test exists for is to make the
// combination a stated property rather than an accident, since the two are
// governed by one fact and read as contradictory without it.
func TestDeclaredCountDoesNotBoundSeeks(t *testing.T) {
	t.Parallel()

	const declared = seekFrames
	src := pattern(seekFrames * seekPerFrame)
	file := rf64DeclaringOnlyACount(t, src, declared)

	d, err := pcm.NewDecoder(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	// The count survives the missing length, and the duration derived from it
	// does too. Zeroing either was tried in #12 and reverted, because it makes
	// the fallback dead code on this path.
	info := d.Info()
	if info.DataSizeKnown {
		t.Error("DataSizeKnown = true for a stream whose ds64 dataSize was never stamped")
	}
	if info.TotalFrames != declared {
		t.Fatalf("TotalFrames = %d, want the declared %d: the fallback to a ds64 sampleCount is what this fixture exercises",
			info.TotalFrames, declared)
	}
	if info.Duration() == 0 {
		t.Error("Duration = 0 despite a declared count and a known sample rate")
	}

	// And the seek is not bounded by it.
	const past = declared * 10
	got, err := d.SeekToFrame(past)
	if err != nil {
		t.Fatalf("SeekToFrame(%d): %v", past, err)
	}
	if got != past {
		t.Errorf("SeekToFrame(%d) = %d, want %d unclamped: a declared count must not bound a seek",
			past, got, past)
	}

	// The only end-of-stream signal available on this path.
	if _, rerr := d.Read(make([]byte, seekPerFrame)); !errors.Is(rerr, io.EOF) {
		t.Errorf("Read past the audio = %v, want io.EOF", rerr)
	}
}

// TestUnknownLengthReportsNoTotalFrames checks the premise every other test in
// this file relies on: each of the four fixtures below reports both
// TotalFrames and Duration as zero, so the decoder genuinely does not know how
// long the stream is while the tests that follow seek around in it.
//
// It pins these fixtures, not a general rule that an unknown length always
// zeroes the frame count. Only the WithIgnoreLength route is zeroed by the
// decoder itself; the other three report zero because nothing else in them
// declares a count. A stream whose byte length is unknown may still carry one,
// in a fact chunk or in a ds64 sampleCount, and the parser deliberately falls
// back to it. Such a stream reports a non-zero TotalFrames while its seeks stay
// unclamped, since the clamp is bounded by the data chunk length rather than by
// the frame count.
func TestUnknownLengthReportsNoTotalFrames(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			d, err := pcm.NewDecoder(bytes.NewReader(route.file(t, src)), route.opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			info := d.Info()
			if info.TotalFrames != 0 {
				t.Errorf("TotalFrames: got %d want 0", info.TotalFrames)
			}
			if info.Duration() != 0 {
				t.Errorf("Duration: got %v want 0", info.Duration())
			}
		})
	}
}

// TestSeekUnknownLengthMatchesLinearDecode checks that seeking forward on a
// stream whose length cannot be determined up front yields exactly the bytes
// a straight, unseeked decode of the same stream holds from that offset. This
// is the central behaviour issue #8 asks to be pinned.
func TestSeekUnknownLengthMatchesLinearDecode(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			file := route.file(t, src)

			for _, frame := range []int64{0, seekFrames / 2, seekFrames - 1} {
				d, err := pcm.NewDecoder(bytes.NewReader(file), route.opts...)
				if err != nil {
					t.Fatalf("NewDecoder: %v", err)
				}
				got, err := d.SeekToFrame(frame)
				if err != nil {
					t.Fatalf("SeekToFrame(%d): %v", frame, err)
				}
				if got != frame {
					t.Errorf("SeekToFrame(%d) reached frame %d", frame, got)
				}
				rest, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll after seeking to frame %d: %v", frame, err)
				}
				want := src[frame*seekPerFrame:]
				if !bytes.Equal(rest, want) {
					t.Errorf("frame %d: got %d bytes, want the %d bytes a linear decode holds from there",
						frame, len(rest), len(want))
				}
			}
		})
	}
}

// TestSeekUnknownLengthBackward checks that a backward seek is exact after
// audio has already been read forward. It pins that dataStart is recorded
// once at parse time rather than derived from bytes already consumed, which
// is what a previous bug got wrong for the known-length case; nothing
// exercised that logic on an unknown-length stream before this test.
func TestSeekUnknownLengthBackward(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)
	const backTo = 10

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			d, err := pcm.NewDecoder(bytes.NewReader(route.file(t, src)), route.opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}

			// Read past where the backward seek will land, so a dataStart
			// bug that tracks bytes consumed rather than a fixed offset
			// would show up as the wrong frame instead of frame 10.
			primed := make([]byte, (seekFrames-backTo)*seekPerFrame)
			if _, err := io.ReadFull(d, primed); err != nil {
				t.Fatalf("priming read: %v", err)
			}

			got, err := d.SeekToFrame(backTo)
			if err != nil {
				t.Fatalf("SeekToFrame(%d): %v", backTo, err)
			}
			if got != backTo {
				t.Fatalf("SeekToFrame(%d) reached frame %d", backTo, got)
			}
			rest, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll after seeking back to frame %d: %v", backTo, err)
			}
			want := src[backTo*seekPerFrame:]
			if !bytes.Equal(rest, want) {
				t.Errorf("after seeking back to frame %d: got %d bytes want %d",
					backTo, len(rest), len(want))
			}
		})
	}
}

// TestSeekUnknownLengthAfterEOF is a regression test for the second half of the
// defect this file's fixes address, the one that needs no option to reach.
//
// Reading an unknown-length stream to its end leaves the decoder's remaining
// count at zero, which is how a Read reports io.EOF without a declared length
// to count down from. A seek away from that position has to restore the count
// to "unknown" so reads resume; SeekToFrame only ever rewrote remaining when
// the header declared a size, so on these routes it left the zero in place. The
// seek itself worked, and the position was correct, but every subsequent Read
// returned io.EOF immediately: a silent no-op that looked like an empty stream
// rather than an error.
func TestSeekUnknownLengthAfterEOF(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			d, err := pcm.NewDecoder(bytes.NewReader(route.file(t, src)), route.opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}

			// Drain the stream so the decoder has actually observed the end,
			// which is the state that used to poison the seek.
			all, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("draining the stream: %v", err)
			}
			if !bytes.Equal(all, src) {
				t.Fatalf("draining the stream: got %d bytes want %d", len(all), len(src))
			}

			if got, err := d.SeekToFrame(0); err != nil {
				t.Fatalf("SeekToFrame(0) after reaching EOF: %v", err)
			} else if got != 0 {
				t.Fatalf("SeekToFrame(0) after reaching EOF reached frame %d", got)
			}

			again, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll after seeking back from EOF: %v", err)
			}
			if !bytes.Equal(again, src) {
				t.Errorf("re-reading after seeking back from EOF: got %d bytes want the whole %d-byte stream; "+
					"a seek away from the end must restore the decoder's ability to read",
					len(again), len(src))
			}
		})
	}
}

// TestSeekUnknownLengthPastTheEnd checks the documented contract for a seek
// past the end: "the next Read reports io.EOF". An unknown-length stream has
// no declared boundary to clamp to, so unlike the known-length case
// SeekToFrame reports the frame it was asked for rather than one bounded by
// the real content; the raw seek is performed and the physical end of the
// source produces a clean io.EOF on the next Read.
//
// The decoder could in principle find a boundary anyway, since it holds an
// io.Seeker and the physical end is one Seek(0, io.SeekEnd) away. It
// deliberately does not measure the source: that would cost a round trip to
// the end on every seek the caller never asked for, and it would be wrong for
// a file still being appended to, which is precisely the recovery case
// WithIgnoreLength exists for. A boundary measured now would be stale by the
// next write.
func TestSeekUnknownLengthPastTheEnd(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)
	farFrame := int64(seekFrames * 10)

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			d, err := pcm.NewDecoder(bytes.NewReader(route.file(t, src)), route.opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			got, err := d.SeekToFrame(farFrame)
			if err != nil {
				t.Fatalf("SeekToFrame(%d): %v", farFrame, err)
			}
			if got != farFrame {
				t.Errorf("SeekToFrame(%d) on an unknown-length stream reached %d; "+
					"there is no known boundary to clamp to, so it must report the frame it was asked to seek to",
					farFrame, got)
			}
			n, rerr := d.Read(make([]byte, 16))
			if n != 0 || !errors.Is(rerr, io.EOF) {
				t.Errorf("Read after seeking past the end: got (%d, %v), want (0, io.EOF)", n, rerr)
			}
		})
	}
}

// TestSeekUnknownLengthWithConversion checks that seeking still lines up with
// a linear decode when the decoder is also converting samples, since the
// converting Read path stages its own output buffer independently of the raw
// path the tests above exercise.
func TestSeekUnknownLengthWithConversion(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)
	const frame = seekFrames / 3
	// s16 converted to s32 doubles the bytes per sample, so the converted
	// offset scales the same way the frame offset does.
	const convertedPerFrame = seekPerFrame * 2

	for _, route := range unknownLengthRoutes() {
		t.Run(route.name, func(t *testing.T) {
			file := route.file(t, src)
			opts := append(slices.Clone(route.opts), pcm.WithConvertTo(32))

			reference, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			wantAll, err := io.ReadAll(reference)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}

			d, err := pcm.NewDecoder(bytes.NewReader(file), opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if _, err := d.SeekToFrame(frame); err != nil {
				t.Fatalf("SeekToFrame(%d): %v", frame, err)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll after seeking under conversion: %v", err)
			}
			want := wantAll[frame*convertedPerFrame:]
			if !bytes.Equal(got, want) {
				t.Errorf("converted read after seeking to frame %d: got %d bytes want %d",
					frame, len(got), len(want))
			}
		})
	}
}

// TestSeekWithIgnoreLengthPastDeclaredSize is a regression test for a defect
// found while writing the tests above. WithIgnoreLength does not change
// hdr.DataSize, the header field parsed at construction; it only overrides
// the decoder's own remaining count. SeekToFrame consulted hdr.DataSize
// directly, so when the header declared a plausible (but understated) size,
// a seek past that declared size was clamped to it and remaining was rebounded
// to it too, silently reimposing the exact length the caller asked to ignore.
func TestSeekWithIgnoreLengthPastDeclaredSize(t *testing.T) {
	src := pattern(seekFrames * seekPerFrame)
	file := encodeFixture(t, seekCfg, src)

	// Lie: claim only the first 10 frames are audio, though the source in
	// fact holds all of seekFrames.
	const declaredFrames = 10
	span := requireChunk(t, file, "data")
	binary.LittleEndian.PutUint32(file[span.payload-4:], declaredFrames*seekPerFrame)

	d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithIgnoreLength())
	if err != nil {
		t.Fatal(err)
	}

	// Seek well past the declared (but ignored) size, to a frame that exists
	// only because the true stream is longer than the header admits.
	const frame = seekFrames - 5
	got, err := d.SeekToFrame(frame)
	if err != nil {
		t.Fatalf("SeekToFrame(%d): %v", frame, err)
	}
	if got != frame {
		t.Errorf("SeekToFrame(%d) reached %d; WithIgnoreLength must not clamp to the declared size",
			frame, got)
	}
	rest, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll after seeking past the declared size: %v", err)
	}
	want := src[frame*seekPerFrame:]
	if !bytes.Equal(rest, want) {
		t.Errorf("after seeking to frame %d under WithIgnoreLength: got %d bytes want %d",
			frame, len(rest), len(want))
	}
}

// TestDataSizeKnownPredictsClamping is the point of the flag: it is the one
// value a caller can read to know which of SeekToFrame's two rules applies,
// and it must agree with what the decoder actually does.
//
// Asserting the flag alone would be worth little, since a constant false would
// satisfy it on every fixture in this file. Each case therefore performs the
// seek as well, so the flag is checked against the behaviour it predicts
// rather than against itself.
func TestDataSizeKnownPredictsClamping(t *testing.T) {
	t.Parallel()

	src := pattern(seekFrames * seekPerFrame)
	const past = seekFrames * 10

	t.Run("declared size bounds the seek", func(t *testing.T) {
		t.Parallel()
		d, err := pcm.NewDecoder(bytes.NewReader(encodeFixture(t, seekCfg, src)))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		if !d.Info().DataSizeKnown {
			t.Fatal("DataSizeKnown = false for an ordinary stream with a stamped size")
		}
		got, serr := d.SeekToFrame(past)
		if serr != nil {
			t.Fatalf("SeekToFrame: %v", serr)
		}
		if got != seekFrames {
			t.Errorf("SeekToFrame(%d) = %d, want it clamped to %d: DataSizeKnown promised a boundary",
				past, got, seekFrames)
		}
	})

	for _, route := range unknownLengthRoutes() {
		t.Run("no boundary: "+route.name, func(t *testing.T) {
			t.Parallel()
			d, err := pcm.NewDecoder(bytes.NewReader(route.file(t, src)), route.opts...)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if d.Info().DataSizeKnown {
				t.Fatalf("DataSizeKnown = true on the %q route", route.name)
			}
			got, serr := d.SeekToFrame(past)
			if serr != nil {
				t.Fatalf("SeekToFrame: %v", serr)
			}
			if got != past {
				t.Errorf("SeekToFrame(%d) = %d, want it unclamped: DataSizeKnown promised no boundary",
					past, got)
			}
		})
	}
}

// TestDataSizeKnownIsNotAPromiseTheAudioIsThere pins the limit of what the flag
// says, which is the half most likely to be misread.
//
// A declared size is a claim like any other. A file truncated after it was
// written still carries the header it was given, so the decoder reports the
// original count, bounds seeks by the original size, and lands a seek well past
// the bytes that survive without complaint. Only the Read afterwards can say
// so. Anyone reading DataSizeKnown as "the audio is all there" would get this
// case wrong, and the field's doc says as much.
func TestDataSizeKnownIsNotAPromiseTheAudioIsThere(t *testing.T) {
	t.Parallel()

	full := encodeFixture(t, seekCfg, pattern(seekFrames*seekPerFrame))
	// Drop the second half of the audio and leave the header untouched, which
	// is what a truncated copy of a complete file looks like.
	const surviving = seekFrames / 2
	cut := full[:len(full)-surviving*seekPerFrame]

	d, err := pcm.NewDecoder(bytes.NewReader(cut))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	info := d.Info()
	if !info.DataSizeKnown {
		t.Fatal("DataSizeKnown = false: truncation does not change the header")
	}
	if info.TotalFrames != seekFrames {
		t.Errorf("TotalFrames = %d, want the declared %d: the count comes from the header, not the bytes",
			info.TotalFrames, seekFrames)
	}

	// Inside the declared range, past the surviving audio.
	const target = seekFrames - 10
	got, serr := d.SeekToFrame(target)
	if serr != nil {
		t.Fatalf("SeekToFrame(%d): %v", target, serr)
	}
	if got != target {
		t.Errorf("SeekToFrame(%d) = %d, want %d: the clamp respects the declared size, not the surviving bytes",
			target, got, target)
	}
	if _, rerr := d.Read(make([]byte, seekPerFrame)); !errors.Is(rerr, io.EOF) {
		t.Errorf("Read past the surviving audio = %v, want io.EOF: it is the only signal that the file is short", rerr)
	}
}
