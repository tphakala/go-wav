package pcm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// The RF64 policy is a three by three matrix over the kind of sink and the
// chosen mode. The spec fixes every cell, so every cell is checked here.
//
//	| sink                     | RF64Auto            | RF64Never | RF64Always |
//	| io.WriteSeeker           | JUNK, upgrade       | ErrTooLarge | RF64 at zero |
//	| io.Writer, frames known  | RIFF or RF64 upfront| ErrTooLarge | RF64 at zero |
//	| io.Writer, frames zero   | ErrTooLarge         | ErrTooLarge | rejected     |

const (
	fileHeaderSize  = 12
	chunkHeaderSize = 8
	ds64PayloadSize = 28
	ds64ChunkSize   = chunkHeaderSize + ds64PayloadSize
	sentinel32      = uint32(0xFFFFFFFF)
	magicRIFF       = "RIFF"
	magicRF64       = "RF64"
	magicBW64       = "BW64"
	idDS64Chunk     = "ds64"
	idJUNKChunk     = "JUNK"
)

var baseCfg = pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}

// TestRF64AlwaysRejectedWithoutSeekOrFrames is the "rejected at construction"
// rule: the one cell of the matrix that cannot produce a correct file.
func TestRF64AlwaysRejectedWithoutSeekOrFrames(t *testing.T) {
	cfg := baseCfg
	cfg.RF64 = pcm.RF64Always

	var buf bytes.Buffer
	e, err := pcm.NewEncoder(&buf, cfg)
	if err == nil {
		t.Fatal("NewEncoder accepted RF64Always on a plain io.Writer with no TotalFrames")
	}
	if e != nil {
		t.Error("NewEncoder returned a non-nil Encoder alongside its error")
	}
	if buf.Len() != 0 {
		t.Errorf("a rejected configuration still emitted %d bytes", buf.Len())
	}

	// A writer that merely wraps a seekable sink is still not seekable, and
	// must be rejected the same way.
	sink := &memSeeker{}
	if _, err := pcm.NewEncoder(nonSeekWriter{w: sink}, cfg); err == nil {
		t.Error("NewEncoder accepted RF64Always behind a non-seekable wrapper")
	}

	// Reset must apply the same rule.
	var e2 pcm.Encoder
	if err := e2.Reset(&buf, cfg); err == nil {
		t.Error("Reset accepted RF64Always on a plain io.Writer with no TotalFrames")
	}
}

// TestRF64AlwaysWithTotalFrames is the same combination made legal by declaring
// the length up front.
func TestRF64AlwaysWithTotalFrames(t *testing.T) {
	const frames = 128
	cfg := baseCfg
	cfg.RF64 = pcm.RF64Always
	cfg.TotalFrames = frames

	var buf bytes.Buffer
	e, err := pcm.NewEncoder(&buf, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got := magic(t, buf.Bytes()); got != magicRF64 {
		t.Errorf("magic before any audio: got %q want %q", got, magicRF64)
	}

	src := pattern(frames * 2)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b := buf.Bytes()
	assertRF64Shape(t, b, int64(len(src)), frames)

	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewDecoder on our own RF64: %v", err)
	}
	if got := d.Info().Container; got != wav.ContainerRF64 {
		t.Errorf("Container: got %v want RF64", got)
	}
	if got := d.Info().TotalFrames; got != frames {
		t.Errorf("TotalFrames: got %d want %d", got, frames)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("payload: got %d bytes want %d", len(got), len(src))
	}
}

// TestRF64AlwaysSeekable checks that a seekable sink also gets RF64 from byte
// zero, with the ds64 patched at Close.
func TestRF64AlwaysSeekable(t *testing.T) {
	cfg := baseCfg
	cfg.RF64 = pcm.RF64Always

	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got := magic(t, sink.b); got != magicRF64 {
		t.Errorf("magic before any audio: got %q want %q", got, magicRF64)
	}
	if got := string(sink.b[fileHeaderSize : fileHeaderSize+4]); got != idDS64Chunk {
		t.Errorf("chunk after the file header: got %q want %q", got, idDS64Chunk)
	}

	src := pattern(200)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	assertRF64Shape(t, sink.b, int64(len(src)), 100)
	assertDecodes(t, sink.b, cfg, src)
}

// TestRF64AutoSeekableReservesJUNK checks the reservation: RIFF magic plus a
// JUNK chunk of exactly 36 wire bytes right after the 12-byte file header.
func TestRF64AutoSeekableReservesJUNK(t *testing.T) {
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, baseCfg) // RF64Auto is the zero value
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	b := sink.b
	if got := magic(t, b); got != magicRIFF {
		t.Errorf("magic: got %q want %q", got, magicRIFF)
	}
	if got := string(b[fileHeaderSize : fileHeaderSize+4]); got != idJUNKChunk {
		t.Fatalf("chunk after the file header: got %q want %q", got, idJUNKChunk)
	}
	if got := u32At(t, b, fileHeaderSize+4); got != ds64PayloadSize {
		t.Errorf("JUNK payload size: got %d want %d", got, ds64PayloadSize)
	}
	// The reserved region must be exactly a ds64 chunk wide, so that the
	// upgrade is a pure in-place rewrite.
	next := fileHeaderSize + ds64ChunkSize
	if got := string(b[next : next+4]); got != "fmt " {
		t.Errorf("chunk at offset %d: got %q, want %q, so JUNK is not %d wire bytes",
			next, got, "fmt ", ds64ChunkSize)
	}

	src := pattern(64)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// A small stream stays plain RIFF; the JUNK chunk simply remains.
	if got := magic(t, sink.b); got != magicRIFF {
		t.Errorf("magic after a small stream: got %q want %q", got, magicRIFF)
	}
	if got := string(sink.b[fileHeaderSize : fileHeaderSize+4]); got != idJUNKChunk {
		t.Errorf("reserved chunk after a small stream: got %q want %q", got, idJUNKChunk)
	}
	assertDecodes(t, sink.b, baseCfg, src)
	d, err := pcm.NewDecoder(bytes.NewReader(sink.b))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Info().Container; got != wav.ContainerRIFF {
		t.Errorf("Container: got %v want RIFF", got)
	}
}

// TestRF64AutoNonSeekableReservesNothing checks that no space is reserved when
// it could never be used.
func TestRF64AutoNonSeekableReservesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		totalFrames uint64
	}{
		{"frames unknown", 0},
		{"frames declared", 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.TotalFrames = tc.totalFrames

			var buf bytes.Buffer
			e, err := pcm.NewEncoder(&buf, cfg)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			b := buf.Bytes()
			if got := magic(t, b); got != magicRIFF {
				t.Errorf("magic: got %q want %q", got, magicRIFF)
			}
			if got := string(b[fileHeaderSize : fileHeaderSize+4]); got == idJUNKChunk {
				t.Error("a non-seekable sink got a JUNK reservation it can never use")
			} else if got != "fmt " {
				t.Errorf("chunk after the file header: got %q want %q", got, "fmt ")
			}
			if spans := walkChunks(t, b); len(spans) != 2 {
				t.Errorf("expected only fmt and data, got %v", chunkIDs(spans))
			}

			src := pattern(int(tc.totalFrames) * 2)
			if _, err := e.Write(src); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

// TestRF64NeverEmitsNoRF64 checks that the mode is absolute.
func TestRF64NeverEmitsNoRF64(t *testing.T) {
	cases := []struct {
		name        string
		seekable    bool
		totalFrames uint64
	}{
		{"seekable, frames unknown", true, 0},
		{"seekable, frames declared", true, 64},
		{"plain writer, frames unknown", false, 0},
		{"plain writer, frames declared", false, 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.RF64 = pcm.RF64Never
			cfg.TotalFrames = tc.totalFrames

			var head func() []byte
			var w io.Writer
			if tc.seekable {
				sink := &memSeeker{}
				w = sink
				head = func() []byte { return sink.b }
			} else {
				buf := &bytes.Buffer{}
				w = buf
				head = buf.Bytes
			}

			e, err := pcm.NewEncoder(w, cfg)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			if got := magic(t, head()); got != magicRIFF {
				t.Errorf("magic: got %q want %q", got, magicRIFF)
			}
			if got := string(head()[fileHeaderSize : fileHeaderSize+4]); got == idJUNKChunk || got == idDS64Chunk {
				t.Errorf("RF64Never reserved or wrote a %q chunk", got)
			}

			frames := tc.totalFrames
			if frames == 0 {
				frames = 64
			}
			if _, err := e.Write(pattern(int(frames) * 2)); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := magic(t, head()); got != magicRIFF {
				t.Errorf("magic after Close: got %q want %q", got, magicRIFF)
			}
		})
	}
}

// TestRF64AutoDeclaredSizeBoundary checks the cell where a declared frame count
// settles the container up front. Only the header changes, so no 4 GiB of audio
// is needed: the header is inspected before Close.
func TestRF64AutoDeclaredSizeBoundary(t *testing.T) {
	// The header this configuration produces, and therefore the point past
	// which a 32-bit file size field overflows.
	dataOffset := headerLen(t, baseCfg)
	const maxU32 = int64(1)<<32 - 1
	// riffSize is dataOffset + padded(dataSize) - 8, and both it and dataSize
	// must fit 32 bits.
	maxData := maxU32 - dataOffset + 8
	if maxData > maxU32 {
		maxData = maxU32
	}
	perFrame := int64(2)
	fitting := maxData / perFrame
	if fitting*perFrame > maxData {
		fitting--
	}

	cases := []struct {
		name          string
		frames        uint64
		wantContainer string
	}{
		{"just under the boundary", uint64(fitting), magicRIFF},
		{"just over the boundary", uint64(fitting) + 1, magicRF64},
		{"far over the boundary", uint64(fitting) * 4, magicRF64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.TotalFrames = tc.frames

			var buf bytes.Buffer
			e, err := pcm.NewEncoder(&buf, cfg)
			if err != nil {
				t.Fatalf("NewEncoder with TotalFrames %d: %v", tc.frames, err)
			}
			b := buf.Bytes()
			if got := magic(t, b); got != tc.wantContainer {
				t.Errorf("magic for %d declared frames: got %q want %q",
					tc.frames, got, tc.wantContainer)
			}
			if tc.wantContainer == magicRF64 {
				if got := string(b[fileHeaderSize : fileHeaderSize+4]); got != idDS64Chunk {
					t.Errorf("chunk after the file header: got %q want %q", got, idDS64Chunk)
				}
				if got := u32At(t, b, 4); got != sentinel32 {
					t.Errorf("file header size field: got %#08x want the sentinel", got)
				}
				p := fileHeaderSize + chunkHeaderSize
				if got := u64At(t, b, p+8); got != tc.frames*2 {
					t.Errorf("ds64 dataSize: got %d want %d", got, tc.frames*2)
				}
				if got := u64At(t, b, p+16); got != tc.frames {
					t.Errorf("ds64 sampleCount: got %d want %d", got, tc.frames)
				}
			}
			// The header is what is under test; Close then reports the frame
			// count the caller never delivered.
			cerr := e.Close()
			if cerr == nil {
				t.Fatal("Close accepted zero frames against a huge declared count")
			}
		})
	}

	t.Run("RF64Never refuses a declared size that will not fit", func(t *testing.T) {
		cfg := baseCfg
		cfg.RF64 = pcm.RF64Never
		cfg.TotalFrames = uint64(fitting) + 1

		var buf bytes.Buffer
		_, err := pcm.NewEncoder(&buf, cfg)
		if err == nil {
			t.Fatal("NewEncoder accepted a declared size RIFF cannot express")
		}
		if !errors.Is(err, wav.ErrTooLarge) {
			t.Errorf("got %v, want wav.ErrTooLarge", err)
		}
	})

	t.Run("RF64Never accepts a declared size that fits", func(t *testing.T) {
		cfg := baseCfg
		cfg.RF64 = pcm.RF64Never
		cfg.TotalFrames = uint64(fitting)

		var buf bytes.Buffer
		e, err := pcm.NewEncoder(&buf, cfg)
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		if got := magic(t, buf.Bytes()); got != magicRIFF {
			t.Errorf("magic: got %q want %q", got, magicRIFF)
		}
		_ = e.Close()
	})

	t.Run("a declared frame count that overflows a byte count is rejected", func(t *testing.T) {
		cfg := baseCfg
		cfg.TotalFrames = ^uint64(0)

		var buf bytes.Buffer
		_, err := pcm.NewEncoder(&buf, cfg)
		if err == nil {
			t.Fatal("NewEncoder accepted a frame count that overflows int64")
		}
		if !errors.Is(err, wav.ErrTooLarge) {
			t.Errorf("got %v, want wav.ErrTooLarge", err)
		}
	})
}

// headerLen returns the number of header bytes a configuration emits before the
// first audio byte, on a sink that reserves nothing.
func headerLen(tb testing.TB, cfg pcm.Config) int64 {
	tb.Helper()
	var buf bytes.Buffer
	if _, err := pcm.NewEncoder(&buf, cfg); err != nil {
		tb.Fatalf("NewEncoder: %v", err)
	}
	return int64(buf.Len())
}

// assertRF64Shape checks the fields an RF64 header must carry.
func assertRF64Shape(tb testing.TB, b []byte, dataSize int64, frames uint64) {
	tb.Helper()
	if got := magic(tb, b); got != magicRF64 {
		tb.Fatalf("magic: got %q want %q", got, magicRF64)
	}
	if got := u32At(tb, b, 4); got != sentinel32 {
		tb.Errorf("file header size field: got %#08x want %#08x", got, sentinel32)
	}
	if got := string(b[fileHeaderSize : fileHeaderSize+4]); got != idDS64Chunk {
		tb.Fatalf("chunk after the file header: got %q want %q", got, idDS64Chunk)
	}
	if got := u32At(tb, b, fileHeaderSize+4); got != ds64PayloadSize {
		tb.Errorf("ds64 payload size: got %d want %d", got, ds64PayloadSize)
	}
	p := fileHeaderSize + chunkHeaderSize
	if got := u64At(tb, b, p+8); got != uint64(dataSize) {
		tb.Errorf("ds64 dataSize: got %d want %d", got, dataSize)
	}
	if got := u64At(tb, b, p+16); got != frames {
		tb.Errorf("ds64 sampleCount: got %d want %d", got, frames)
	}
	// The data chunk's own 32-bit size is superseded by the sentinel.
	span := requireChunk(tb, b, "data")
	if got := u32At(tb, b, span.payload-4); got != sentinel32 {
		tb.Errorf("data chunk size field: got %#08x want %#08x", got, sentinel32)
	}
	// riffSize is the whole file less the eight bytes ahead of it, counting the
	// pad byte an odd data chunk carries.
	padded := dataSize
	if padded%2 != 0 {
		padded++
	}
	wantRIFF := uint64(int64(span.payload) + padded - 8)
	if got := u64At(tb, b, p); got != wantRIFF {
		tb.Errorf("ds64 riffSize: got %d want %d", got, wantRIFF)
	}
}

// bigChunk is the buffer streamed repeatedly by the 4 GiB tests. It is large so
// that crossing the limit costs few iterations.
const bigChunk = 64 << 20

// overLimit is comfortably past the 4 GiB a 32-bit size field can describe.
const overLimit = int64(1)<<32 + int64(128<<20)

// TestRF64AutoSeekableUpgradesPastFourGiB streams more than 4 GiB through a
// sink that counts rather than stores, and checks that the reserved JUNK chunk
// became a correct ds64.
func TestRF64AutoSeekableUpgradesPastFourGiB(t *testing.T) {
	if testing.Short() {
		t.Skip("streams over 4 GiB; skipped under -short")
	}

	sink := &countingSink{}
	e, err := pcm.NewEncoder(sink, baseCfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	headerBytes := sink.size
	if got := string(sink.head[fileHeaderSize : fileHeaderSize+4]); got != idJUNKChunk {
		t.Fatalf("expected a JUNK reservation, got %q", got)
	}

	written := streamPast(t, e, overLimit)
	if err := e.Close(); err != nil {
		t.Fatalf("Close after %d bytes: %v", written, err)
	}

	b := sink.bytes()
	if got := magic(t, b); got != magicRF64 {
		t.Fatalf("magic after Close: got %q want %q; the in place upgrade did not happen", got, magicRF64)
	}
	if got := u32At(t, b, 4); got != sentinel32 {
		t.Errorf("file header size field: got %#08x want the sentinel", got)
	}
	if got := string(b[fileHeaderSize : fileHeaderSize+4]); got != idDS64Chunk {
		t.Errorf("reserved chunk after Close: got %q want %q", got, idDS64Chunk)
	}
	p := fileHeaderSize + chunkHeaderSize
	frames := uint64(written / 2)
	if got := u64At(t, b, p+8); got != uint64(written) {
		t.Errorf("ds64 dataSize: got %d want %d", got, written)
	}
	if got := u64At(t, b, p+16); got != frames {
		t.Errorf("ds64 sampleCount: got %d want %d", got, frames)
	}
	if got := u64At(t, b, p); got != uint64(headerBytes+written-8) {
		t.Errorf("ds64 riffSize: got %d want %d", got, headerBytes+written-8)
	}
	span := requireChunk(t, b, "data")
	if got := u32At(t, b, span.payload-4); got != sentinel32 {
		t.Errorf("data chunk size field: got %#08x want the sentinel", got)
	}
	if sink.size != headerBytes+written {
		t.Errorf("sink holds %d bytes, want %d", sink.size, headerBytes+written)
	}

	// The head alone is enough to re-parse, since a decoder never reads past
	// the data chunk header to describe a stream.
	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewDecoder on the upgraded head: %v", err)
	}
	if got := d.Info().Container; got != wav.ContainerRF64 {
		t.Errorf("Container: got %v want RF64", got)
	}
	if got := d.Info().TotalFrames; got != frames {
		t.Errorf("TotalFrames: got %d want %d", got, frames)
	}
}

// TestRF64PastFourGiBWithoutRescue checks the cells that must report
// wav.ErrTooLarge rather than write a size they know to be wrong.
func TestRF64PastFourGiBWithoutRescue(t *testing.T) {
	if testing.Short() {
		t.Skip("streams over 4 GiB; skipped under -short")
	}

	cases := []struct {
		name     string
		mode     pcm.RF64Mode
		seekable bool
	}{
		{"RF64Never on a seekable sink", pcm.RF64Never, true},
		{"RF64Never on a plain writer", pcm.RF64Never, false},
		{"RF64Auto on a plain writer with no declared frames", pcm.RF64Auto, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.RF64 = tc.mode

			var w io.Writer
			var head func() []byte
			var total func() int64
			if tc.seekable {
				sink := &countingSink{}
				w, head, total = sink, sink.bytes, func() int64 { return sink.size }
			} else {
				sink := &countingWriter{}
				w, head, total = sink, func() []byte { return sink.head }, func() int64 { return sink.n }
			}

			e, err := pcm.NewEncoder(w, cfg)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}

			buf := make([]byte, bigChunk)
			var written int64
			var got error
			for written < overLimit {
				n, werr := e.Write(buf)
				written += int64(n)
				if werr != nil {
					got = werr
					break
				}
			}
			if got == nil {
				got = e.Close()
			}
			if got == nil {
				t.Fatalf("%d bytes went through with no error at all", written)
			}
			if !errors.Is(got, wav.ErrTooLarge) {
				t.Fatalf("got %v, want wav.ErrTooLarge", got)
			}
			// Nothing may have been written past what a 32-bit field can
			// describe, and the magic must still be plain RIFF.
			if m := magic(t, head()); m != magicRIFF {
				t.Errorf("magic: got %q want %q", m, magicRIFF)
			}
			if total() > int64(1)<<32 {
				t.Errorf("the sink received %d bytes, past the 4 GiB the header can describe", total())
			}
			// The error must latch.
			if _, werr := e.Write(make([]byte, 16)); !errors.Is(werr, wav.ErrTooLarge) {
				t.Errorf("Write after the refusal: got %v, want the latched wav.ErrTooLarge", werr)
			}
			if cerr := e.Close(); !errors.Is(cerr, wav.ErrTooLarge) {
				t.Errorf("Close after the refusal: got %v, want the latched wav.ErrTooLarge", cerr)
			}
		})
	}
}

// TestRF64AlwaysPastFourGiBNonSeekable checks that a declared frame count lets a
// plain writer receive a correct RF64 header for a stream over 4 GiB, and that
// the audio then flows without complaint.
func TestRF64AlwaysPastFourGiBNonSeekable(t *testing.T) {
	if testing.Short() {
		t.Skip("streams over 4 GiB; skipped under -short")
	}

	chunks := int(overLimit/bigChunk) + 1
	total := int64(chunks) * bigChunk
	frames := uint64(total / 2)

	cfg := baseCfg
	cfg.RF64 = pcm.RF64Always
	cfg.TotalFrames = frames

	sink := &countingWriter{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	headerBytes := sink.n
	if got := magic(t, sink.head); got != magicRF64 {
		t.Fatalf("magic: got %q want %q", got, magicRF64)
	}

	buf := make([]byte, bigChunk)
	for range chunks {
		if _, err := e.Write(buf); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sink.n != headerBytes+total {
		t.Errorf("sink holds %d bytes, want %d", sink.n, headerBytes+total)
	}
	assertRF64Shape(t, sink.head, total, frames)
}

// streamPast writes zero audio until at least want bytes have gone through, and
// returns the exact number written.
func streamPast(tb testing.TB, e *pcm.Encoder, want int64) int64 {
	tb.Helper()
	buf := make([]byte, bigChunk)
	var written int64
	for written < want {
		n, err := e.Write(buf)
		written += int64(n)
		if err != nil {
			tb.Fatalf("Write at %d bytes: %v", written, err)
		}
	}
	return written
}

// TestRF64ModeString pins the names, since they are part of error messages.
func TestRF64ModeString(t *testing.T) {
	cases := map[pcm.RF64Mode]string{
		pcm.RF64Auto:     "auto",
		pcm.RF64Never:    "never",
		pcm.RF64Always:   "always",
		pcm.RF64Mode(99): "unknown",
	}
	for mode, want := range cases {
		if got := mode.String(); got != want {
			t.Errorf("RF64Mode(%d).String(): got %q want %q", int(mode), got, want)
		}
	}
}

// TestRF64MatrixSmallStreams walks every cell of the matrix with a small stream,
// so that the container each combination chooses is pinned even where no limit
// is approached.
func TestRF64MatrixSmallStreams(t *testing.T) {
	const frames = 64
	src := pattern(frames * 2)

	sinks := []struct {
		name string
		make func() (io.Writer, func() []byte)
	}{
		{"io.WriteSeeker", func() (io.Writer, func() []byte) {
			s := &memSeeker{}
			return s, func() []byte { return s.b }
		}},
		{"io.Writer", func() (io.Writer, func() []byte) {
			b := &bytes.Buffer{}
			return b, b.Bytes
		}},
	}
	modes := []struct {
		name string
		mode pcm.RF64Mode
	}{
		{"RF64Auto", pcm.RF64Auto},
		{"RF64Never", pcm.RF64Never},
		{"RF64Always", pcm.RF64Always},
	}
	declared := []uint64{0, frames}

	for _, sk := range sinks {
		for _, md := range modes {
			for _, tf := range declared {
				name := fmt.Sprintf("%s/%s/TotalFrames %d", sk.name, md.name, tf)
				t.Run(name, func(t *testing.T) {
					cfg := baseCfg
					cfg.RF64 = md.mode
					cfg.TotalFrames = tf

					w, head := sk.make()
					_, seekable := w.(io.WriteSeeker)
					e, err := pcm.NewEncoder(w, cfg)

					mustReject := md.mode == pcm.RF64Always && !seekable && tf == 0
					if mustReject {
						if err == nil {
							t.Fatal("this cell must be rejected at construction")
						}
						return
					}
					if err != nil {
						t.Fatalf("NewEncoder: %v", err)
					}
					if _, err := e.Write(src); err != nil {
						t.Fatal(err)
					}
					if err := e.Close(); err != nil {
						t.Fatalf("Close: %v", err)
					}

					want := magicRIFF
					if md.mode == pcm.RF64Always {
						want = magicRF64
					}
					if got := magic(t, head()); got != want {
						t.Errorf("magic: got %q want %q", got, want)
					}
					assertDecodes(t, head(), cfg, src)
				})
			}
		}
	}
}
