package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// bextFixtureV2 is a version 2 descriptor carrying a UMID and all five loudness
// fields, so a round trip exercises the version-gated read path. TimeReference
// spans both 32-bit halves.
func bextFixtureV2() *pcm.Bext {
	var umid [64]byte
	for i := range umid {
		umid[i] = byte(i + 1)
	}
	return &pcm.Bext{
		Description:         "version 2 fixture",
		Originator:          "go-wav test rig",
		OriginatorReference: "GOWAV0000000002",
		OriginationDate:     "2026-08-05",
		OriginationTime:     "09:15:30",
		// Distinct 32-bit halves, so a low/high transposition in the read path
		// changes the value and this fixture alone catches it.
		TimeReference:        0xDEADBEEF_01234567,
		Version:              2,
		UMID:                 umid,
		LoudnessValue:        -2301,
		LoudnessRange:        750,
		MaxTruePeakLevel:     -145,
		MaxMomentaryLoudness: -1999,
		MaxShortTermLoudness: 1234,
		CodingHistory:        "A=PCM,F=48000,W=24,M=stereo\r\n",
	}
}

// bextFixtureV0 is a minimal version 0 descriptor: no UMID, no loudness, and an
// empty CodingHistory, so the chunk body is exactly the fixed 602 bytes.
func bextFixtureV0() *pcm.Bext {
	return &pcm.Bext{Description: "minimal", Version: 0}
}

// insertChunkAfter splices a chunk carrying body, tagged chunkID, into b right
// after the afterID chunk (and its pad byte), fixing the RIFF file-size field so
// the result stays well formed. It builds malformed-metadata fixtures without a
// hand-assembled header.
func insertChunkAfter(tb testing.TB, b []byte, afterID, chunkID string, body []byte) []byte {
	tb.Helper()
	span := requireChunk(tb, b, afterID)
	adv := span.size
	if adv%2 != 0 {
		adv++
	}
	insertAt := span.payload + adv

	chunk := make([]byte, 0, 8+len(body)+1)
	chunk = append(chunk, chunkID...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)))
	chunk = append(chunk, size[:]...)
	chunk = append(chunk, body...)
	if len(body)%2 != 0 {
		chunk = append(chunk, 0)
	}

	out := make([]byte, 0, len(b)+len(chunk))
	out = append(out, b[:insertAt]...)
	out = append(out, chunk...)
	out = append(out, b[insertAt:]...)
	if magic(tb, out) == magicRIFF {
		binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	}
	return out
}

// TestDecoderBextRoundTrip encodes a stream with a Bext, decodes it, and checks
// that Decoder.Bext hands back an identical descriptor, so a decoded chunk can
// be re-encoded losslessly. The audio must still decode unchanged.
func TestDecoderBextRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		bext *pcm.Bext
	}{
		{"version1", bextFixture()},
		{"version2_umid_loudness", bextFixtureV2()},
		{"version0_empty_history", bextFixtureV0()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := pattern(256)
			b := encodeFixture(t, pcm.Config{
				SampleRate: 48000, BitDepth: 16, Channels: 1, Bext: tc.bext,
			}, src)

			d, err := pcm.NewDecoder(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			got, err := d.Bext()
			if err != nil {
				t.Fatalf("Bext: %v", err)
			}
			if got == nil {
				t.Fatal("Bext returned nil for a stream that carries one")
			}
			if !reflect.DeepEqual(got, tc.bext) {
				t.Errorf("round-tripped Bext mismatch\n got %+v\nwant %+v", got, tc.bext)
			}

			audio, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(audio, src) {
				t.Errorf("audio: got %d bytes, want %d", len(audio), len(src))
			}
		})
	}
}

// TestDecoderBextAbsent checks that a stream carrying no bext chunk reports its
// absence as (nil, nil), not as an error.
func TestDecoderBextAbsent(t *testing.T) {
	b := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64))
	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := d.Bext()
	if err != nil {
		t.Fatalf("Bext returned an error for a stream with no bext: %v", err)
	}
	if got != nil {
		t.Errorf("Bext = %+v, want nil for a stream with no bext chunk", got)
	}
}

// TestDecoderBextMalformedTooShort checks that a bext chunk shorter than its
// fixed body surfaces as an error from Bext wrapping wav.ErrCorruptStream, that
// NewDecoder does not fail on it, and that the audio still decodes. Metadata
// damage must never fail the stream.
func TestDecoderBextMalformedTooShort(t *testing.T) {
	src := pattern(64)
	clean := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, src)
	bad := insertChunkAfter(t, clean, "fmt ", "bext", make([]byte, 40))

	d, err := pcm.NewDecoder(bytes.NewReader(bad))
	if err != nil {
		t.Fatalf("NewDecoder must not fail on a malformed bext: %v", err)
	}
	got, err := d.Bext()
	if got != nil {
		t.Errorf("Bext = %+v, want nil for a malformed chunk", got)
	}
	if !errors.Is(err, wav.ErrCorruptStream) {
		t.Fatalf("Bext error = %v, want one wrapping wav.ErrCorruptStream", err)
	}

	audio, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(audio, src) {
		t.Errorf("audio: got %d bytes, want %d", len(audio), len(src))
	}
}

// TestDecoderBextMemoizedAndClearedOnReset checks that Bext parses once and
// caches the result, and that Reset clears the cache so a reused decoder does
// not report the previous stream's metadata.
func TestDecoderBextMemoizedAndClearedOnReset(t *testing.T) {
	withBext := encodeFixture(t, pcm.Config{
		SampleRate: 48000, BitDepth: 16, Channels: 1, Bext: bextFixture(),
	}, pattern(64))

	d, err := pcm.NewDecoder(bytes.NewReader(withBext))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	first, err := d.Bext()
	if err != nil || first == nil {
		t.Fatalf("Bext: got (%v, %v)", first, err)
	}
	second, _ := d.Bext()
	if first != second {
		t.Error("Bext returned a different pointer on the second call; the parse is not memoized")
	}

	noBext := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64))
	if err := d.Reset(bytes.NewReader(noBext)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	got, err := d.Bext()
	if err != nil {
		t.Fatalf("Bext after Reset: %v", err)
	}
	if got != nil {
		t.Errorf("Bext = %+v after Reset onto a bext-less stream, want nil", got)
	}
}

// TestDecoderBextFirstWins checks that when a malformed stream carries more than
// one bext chunk, the first is exposed and the rest are ignored, as EBU Tech
// 3285 (one bext per file) implies.
func TestDecoderBextFirstWins(t *testing.T) {
	src := pattern(64)
	clean := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, src)
	// Insert the second chunk after fmt, then the first ahead of it, so the
	// stream order is fmt, first (v1), second (v2), data.
	withSecond := insertChunkAfter(t, clean, "fmt ", "bext", bextFixtureV2().Serialize())
	both := insertChunkAfter(t, withSecond, "fmt ", "bext", bextFixture().Serialize())

	d, err := pcm.NewDecoder(bytes.NewReader(both))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := d.Bext()
	if err != nil {
		t.Fatalf("Bext: %v", err)
	}
	if !reflect.DeepEqual(got, bextFixture()) {
		t.Errorf("Bext returned a later chunk; want the first\n got %+v\nwant %+v", got, bextFixture())
	}
	if audio, err := io.ReadAll(d); err != nil {
		t.Fatalf("ReadAll: %v", err)
	} else if !bytes.Equal(audio, src) {
		t.Errorf("audio: got %d bytes, want %d", len(audio), len(src))
	}
}

// TestDecoderBextOversizedIsAbsentAndLocksOut checks that a bext chunk larger
// than the reader's in-memory cap reads as absent, and that being the first
// bext it locks out a smaller valid chunk that follows. This is exactly why the
// walk uses a haveBext flag rather than a nil check: an oversized first body is
// captured as nil, and that "absent" verdict must still win.
func TestDecoderBextOversizedIsAbsentAndLocksOut(t *testing.T) {
	src := pattern(64)
	clean := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, src)
	// A valid bext second, then an oversized (> 1 MiB) bext first.
	withValid := insertChunkAfter(t, clean, "fmt ", "bext", bextFixture().Serialize())
	both := insertChunkAfter(t, withValid, "fmt ", "bext", make([]byte, (1<<20)+2))

	d, err := pcm.NewDecoder(bytes.NewReader(both))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	got, err := d.Bext()
	if err != nil {
		t.Fatalf("Bext returned an error for an oversized bext, want it treated as absent: %v", err)
	}
	if got != nil {
		t.Errorf("Bext = %+v, want nil: an oversized first chunk must read as absent and lock out the later valid one", got)
	}
	if audio, err := io.ReadAll(d); err != nil {
		t.Fatalf("ReadAll: %v", err)
	} else if !bytes.Equal(audio, src) {
		t.Errorf("audio: got %d bytes, want %d", len(audio), len(src))
	}
}

// TestParseBextTooShort checks that a body shorter than the fixed 602-byte block
// is refused with wav.ErrCorruptStream, and that exactly the fixed size parses.
func TestParseBextTooShort(t *testing.T) {
	for _, n := range []int{0, 1, pcm.BextFixedSize - 1} {
		if _, err := pcm.ParseBext(make([]byte, n)); !errors.Is(err, wav.ErrCorruptStream) {
			t.Errorf("ParseBext(%d bytes) error = %v, want one wrapping wav.ErrCorruptStream", n, err)
		}
	}
	if _, err := pcm.ParseBext(make([]byte, pcm.BextFixedSize)); err != nil {
		t.Errorf("ParseBext(%d bytes) = %v, want a parsed empty descriptor", pcm.BextFixedSize, err)
	}
}

// TestParseBextRoundTripsSerialize checks that parseBext is the exact inverse of
// serialize for every descriptor the encoder accepts.
func TestParseBextRoundTripsSerialize(t *testing.T) {
	cases := []struct {
		name string
		bext *pcm.Bext
	}{
		{"version1", bextFixture()},
		{"version2", bextFixtureV2()},
		{"version0_empty", bextFixtureV0()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pcm.ParseBext(tc.bext.Serialize())
			if err != nil {
				t.Fatalf("ParseBext: %v", err)
			}
			if !reflect.DeepEqual(got, tc.bext) {
				t.Errorf("ParseBext(Serialize()) mismatch\n got %+v\nwant %+v", got, tc.bext)
			}
		})
	}
}

// TestParseBextGatesFieldsOnVersion checks that a reader honours the Version
// gate serialize and validate honour: with Version forced to 0 on the wire, the
// UMID (version 1) and loudness (version 2) bytes are treated as Reserved and
// read back zero rather than inventing metadata the chunk does not assert.
func TestParseBextGatesFieldsOnVersion(t *testing.T) {
	body := bextFixtureV2().Serialize()
	body[pcm.BextOffVersion] = 0
	body[pcm.BextOffVersion+1] = 0

	got, err := pcm.ParseBext(body)
	if err != nil {
		t.Fatalf("ParseBext: %v", err)
	}
	if got.Version != 0 {
		t.Fatalf("Version = %d, want 0", got.Version)
	}
	if got.UMID != ([64]byte{}) {
		t.Errorf("UMID = %x, want all zero: version 0 must not read the UMID bytes", got.UMID)
	}
	if got.LoudnessValue != 0 || got.LoudnessRange != 0 || got.MaxTruePeakLevel != 0 ||
		got.MaxMomentaryLoudness != 0 || got.MaxShortTermLoudness != 0 {
		t.Errorf("loudness fields not zero for version 0: %+v", got)
	}
}

// TestParseBextCutsTextAtNUL checks that a fixed-width text field ends at its
// first NUL, so padding (or junk written past the value) is not read back.
func TestParseBextCutsTextAtNUL(t *testing.T) {
	b := &pcm.Bext{Description: "abc", Version: 0}
	body := b.Serialize()
	// "abc" ends at the NUL at index 3; these bytes sit later in the same
	// 256-byte Description field.
	body[10] = 'X'
	body[20] = 'Y'

	got, err := pcm.ParseBext(body)
	if err != nil {
		t.Fatalf("ParseBext: %v", err)
	}
	if got.Description != "abc" {
		t.Errorf("Description = %q, want %q (junk past the NUL must be cut)", got.Description, "abc")
	}
}
