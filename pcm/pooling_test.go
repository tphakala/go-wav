package pcm_test

import (
	"bytes"
	"io"
	"runtime"
	"testing"

	pcm "github.com/tphakala/go-wav/pcm"
)

// poolFixture encodes a stream whose every payload byte is fill, so audio from
// one stream is immediately recognisable if it leaks into another.
func poolFixture(tb testing.TB, fill byte, frames int) []byte {
	tb.Helper()
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := bytes.Repeat([]byte{fill}, frames*2)
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// TestResetDiscardsPreviousStreamAudio is the correctness half of carrying the
// streaming buffer across Reset.
//
// The buffer is reused rather than reallocated, which means it can still hold
// bytes from the stream before it. If Reset carries it without emptying it, a
// pooled Decoder rebound to a new source hands back the OLD stream's audio: no
// error, no short read, just the wrong sound. That is the worst shape a bug in
// this package can take, and it is invisible to every test that decodes only
// one stream per Decoder.
func TestResetDiscardsPreviousStreamAudio(t *testing.T) {
	const frames = 40000 // comfortably more than one streaming buffer
	first := poolFixture(t, 0xAA, frames)
	second := poolFixture(t, 0xBB, frames)

	var d pcm.Decoder
	if err := d.Reset(bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}
	// Read only a little, so the streaming buffer is left holding the rest of
	// the first stream.
	partial := make([]byte, 64)
	if _, err := io.ReadFull(&d, partial); err != nil {
		t.Fatal(err)
	}
	for _, b := range partial {
		if b != 0xAA {
			t.Fatalf("first stream returned %#x, want 0xAA", b)
		}
	}

	if err := d.Reset(bytes.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(&d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != frames*2 {
		t.Errorf("second stream yielded %d bytes, want %d", len(got), frames*2)
	}
	for i, b := range got {
		if b != 0xBB {
			t.Fatalf("byte %d of the second stream is %#x, want 0xBB: audio from the "+
				"previous stream leaked through the carried buffer", i, b)
		}
	}
}

// TestSeekToFrameDiscardsBufferedAudio is the same hazard on the seek path: the
// buffer must be emptied, because it holds bytes from before the seek.
func TestSeekToFrameDiscardsBufferedAudio(t *testing.T) {
	const frames = 40000
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := make([]byte, frames*2)
	for i := range payload {
		payload[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
		t.Fatal(err)
	}

	d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// Fill the streaming buffer, then seek somewhere else entirely.
	if _, err := io.ReadFull(d, make([]byte, 128)); err != nil {
		t.Fatal(err)
	}
	const target = 30000
	if _, err := d.SeekToFrame(target); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 256)
	if _, err := io.ReadFull(d, got); err != nil {
		t.Fatal(err)
	}
	want := payload[target*2 : target*2+256]
	if !bytes.Equal(got, want) {
		t.Errorf("after seeking to frame %d the decoder returned stale buffered audio:\n got %x\nwant %x",
			target, got[:16], want[:16])
	}
}

// bytesPerRun reports the average heap bytes allocated by one call to f.
//
// Allocation COUNT is the wrong measure for the hazard here: dropping the
// streaming buffer changes the count by one and the volume by 64 KiB, so bytes
// are what separates a reused buffer from a rebuilt one.
func bytesPerRun(tb testing.TB, runs int, f func()) uint64 {
	tb.Helper()
	f() // warm: the first pass legitimately builds the buffers
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// TestPooledDecoderReusesItsBuffers is the allocation half of carrying the
// streaming buffer across Reset.
//
// It is a test rather than a benchmark on purpose. A benchmark only runs under
// -bench, so a regression guarded by one is not guarded during ordinary CI,
// and this exact regression reached review once already.
func TestPooledDecoderReusesItsBuffers(t *testing.T) {
	file := poolFixture(t, 0xAA, 20000)
	sink := make([]byte, 4096)

	var d pcm.Decoder
	drain := func() {
		if err := d.Reset(bytes.NewReader(file)); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := d.Read(sink); err != nil {
				return
			}
		}
	}

	got := bytesPerRun(t, 50, drain)
	// A rebind costs a few hundred bytes of small objects. Rebuilding the
	// streaming window would cost 64 KiB on top.
	const limit = 8 << 10
	if got > limit {
		t.Errorf("a pooled Decoder allocates %d bytes per stream, want under %d; "+
			"the streaming buffer is being rebuilt instead of reused", got, limit)
	}
}

// TestSeekReusesItsBuffers is the same guard for the seek path, where random
// access makes a per-seek reallocation especially expensive.
func TestSeekReusesItsBuffers(t *testing.T) {
	file := poolFixture(t, 0xAA, 40000)
	d, err := pcm.NewDecoder(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	sink := make([]byte, 4096)
	seekRead := func() {
		if _, err := d.SeekToFrame(1000); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Read(sink); err != nil {
			t.Fatal(err)
		}
	}

	got := bytesPerRun(t, 50, seekRead)
	const limit = 8 << 10
	if got > limit {
		t.Errorf("a seek allocates %d bytes, want under %d; the streaming buffer is "+
			"being dropped and rebuilt on every seek", got, limit)
	}
}
