package pcm_test

import (
	"bytes"
	"strings"
	"testing"

	pcm "github.com/tphakala/go-wav/pcm"
)

// bextFixture returns a Bext descriptor exercising every field, shared by the
// encoder-level tests in this file.
func bextFixture() *pcm.Bext {
	return &pcm.Bext{
		Description:         "field recording, site 4",
		Originator:          "go-wav test rig",
		OriginatorReference: "GOWAV0000000001",
		OriginationDate:     "2026-08-05",
		OriginationTime:     "09:15:30",
		TimeReference:       123456789,
		Version:             1,
		CodingHistory:       "A=PCM,F=48000,W=16,M=mono,T=go-wav",
	}
}

// TestEncoderWritesBextChunk checks that a configured Bext lands in the
// stream as a bext chunk right after fmt, carrying the fields given, and that
// the audio still decodes byte for byte.
func TestEncoderWritesBextChunk(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bext: bextFixture()}
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	src := pattern(256)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b := sink.b
	span := requireChunk(t, b, "bext")
	// The fixed bext body is 602 bytes wide regardless of field content, so a
	// chunk carrying a non-empty CodingHistory is always at least that wide.
	if span.size < 602 {
		t.Fatalf("bext chunk size = %d, want at least 602", span.size)
	}
	if got := string(b[span.payload : span.payload+len(cfg.Bext.Description)]); got != cfg.Bext.Description {
		t.Errorf("Description at the start of the bext chunk: got %q want %q", got, cfg.Bext.Description)
	}
	wantHistory := cfg.Bext.CodingHistory
	gotHistory := string(b[span.payload+span.size-len(wantHistory) : span.payload+span.size])
	if gotHistory != wantHistory {
		t.Errorf("CodingHistory at the end of the bext chunk: got %q want %q", gotHistory, wantHistory)
	}

	assertDecodes(t, b, cfg, src)
}

// TestEncoderNilBextWritesNoChunk checks that leaving Config.Bext at its zero
// value writes no bext chunk at all, which is the regression this feature
// must not introduce for every caller who does not use it.
func TestEncoderNilBextWritesNoChunk(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	src := pattern(64)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bytes.Contains(sink.b, []byte("bext")) {
		t.Errorf("a Config with Bext left nil still wrote a bext chunk")
	}
	assertDecodes(t, sink.b, cfg, src)
}

// TestEncoderRejectsInvalidBext checks that an invalid Bext, on an otherwise
// valid Config, is refused by NewEncoder and Reset rather than silently
// accepted. Config.validate delegates to Bext.validate, and this is the only
// test that exercises that delegation through the public API rather than
// calling Bext.validate directly: without it, that single wiring line could
// be deleted and the rest of the suite would stay green.
func TestEncoderRejectsInvalidBext(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
		Bext: &pcm.Bext{Description: strings.Repeat("x", 257)}} // one byte over the 256 byte field

	t.Run("NewEncoder", func(t *testing.T) {
		sink := &memSeeker{}
		e, err := pcm.NewEncoder(sink, cfg)
		if err == nil {
			t.Fatal("NewEncoder accepted a Bext.Description one byte over its 256 byte field")
		}
		if e != nil {
			t.Error("NewEncoder returned a non-nil Encoder alongside its error")
		}
		if len(sink.b) != 0 {
			t.Errorf("a rejected configuration still wrote %d bytes", len(sink.b))
		}
	})

	t.Run("Reset", func(t *testing.T) {
		var e pcm.Encoder
		sink := &memSeeker{}
		if err := e.Reset(sink, cfg); err == nil {
			t.Fatal("Reset accepted a Bext.Description one byte over its 256 byte field")
		}
	})
}

// TestEncoderBextPushesRF64AutoBoundary checks that a bext chunk's extra
// header bytes are counted in the RF64Auto decision. A declared data size
// that would just fit a plain RIFF header without a bext chunk must not fit
// once one is added, tipping the choice to RF64 sooner than it otherwise
// would. This is the exact invariant plan_consistency_test.go pins for the
// encoder generally: plan's up-front probe and the header actually written
// must agree, and a bext chunk is what could make them disagree if its bytes
// were counted in one place and not the other.
func TestEncoderBextPushesRF64AutoBoundary(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	plain := headerLen(t, cfg)
	cfg.Bext = bextFixture()
	withBext := headerLen(t, cfg)
	if withBext <= plain {
		t.Fatalf("a header carrying a bext chunk is not longer than one without: %d vs %d", withBext, plain)
	}

	const maxU32 = int64(1)<<32 - 1
	// A declared size that fits exactly under the 32-bit RIFF limit for the
	// shorter, bext-less header, but overflows it once the real, longer
	// header is the one actually counted.
	declaredBytes := maxU32 - plain + 8
	declaredBytes -= declaredBytes % 2  // a whole frame at 16-bit mono
	frames := uint64(declaredBytes / 2) //nolint:gosec // G115: declaredBytes is bounded well under maxU32.

	cfg.TotalFrames = frames
	var buf bytes.Buffer
	e, err := pcm.NewEncoder(&buf, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got := magic(t, buf.Bytes()); got != magicRF64 {
		t.Errorf("magic for a declared size that only overflows once the bext chunk is counted: got %q want %q",
			got, magicRF64)
	}
	requireChunk(t, buf.Bytes(), "bext")
	// The header is what is under test; Close then reports the frame count
	// the caller never delivered, the same way the sibling boundary test in
	// rf64_test.go does without streaming gigabytes of audio.
	if err := e.Close(); err == nil {
		t.Fatal("Close accepted zero frames against a huge declared count")
	}
}

// TestEncoderBextWithRF64Always checks that a bext chunk and RF64Always
// coexist: the stream is RF64 from byte zero, carries a well-formed bext
// chunk, and still decodes byte for byte.
func TestEncoderBextWithRF64Always(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, RF64: pcm.RF64Always, Bext: bextFixture()}
	sink := &memSeeker{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	src := pattern(400)
	if _, err := e.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b := sink.b
	assertRF64Shape(t, b, int64(len(src)), uint64(len(src))/2) //nolint:gosec // G115: len(src) is small and positive.
	requireChunk(t, b, "bext")
	assertDecodes(t, b, cfg, src)
}

// TestEncoderBextSurvivesRF64AutoUpgradePastFourGiB streams more than 4 GiB
// through a seekable sink with a bext chunk configured, and checks that the
// in-place upgrade from a JUNK reservation to a real ds64 leaves the bext
// chunk, which sits ahead of it in the header, untouched and still
// well-formed.
func TestEncoderBextSurvivesRF64AutoUpgradePastFourGiB(t *testing.T) {
	if testing.Short() {
		t.Skip("streams over 4 GiB; skipped under -short")
	}

	cfg := baseCfg
	cfg.Bext = bextFixture()

	sink := &countingSink{}
	e, err := pcm.NewEncoder(sink, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got := string(sink.head[fileHeaderSize : fileHeaderSize+4]); got != idJUNKChunk {
		t.Fatalf("expected a JUNK reservation, got %q", got)
	}
	requireChunk(t, sink.head[:], "bext")

	written := streamPast(t, e, overLimit)
	if err := e.Close(); err != nil {
		t.Fatalf("Close after %d bytes: %v", written, err)
	}

	b := sink.bytes()
	if got := magic(t, b); got != magicRF64 {
		t.Fatalf("magic after Close: got %q want %q; the in place upgrade did not happen", got, magicRF64)
	}
	span := requireChunk(t, b, "bext")
	if span.size < 602 {
		t.Errorf("bext chunk size = %d, want at least 602", span.size)
	}
	assertRF64Shape(t, b, written, uint64(written/2)) //nolint:gosec // G115: written is non-negative.
}
