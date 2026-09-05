package pcm_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// sampleIXML is a small but realistic iXML body, odd-length so a round trip
// also exercises the writer's pad byte.
const sampleIXML = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<BWFXML><IXML_VERSION>1.62</IXML_VERSION><PROJECT>demo</PROJECT>` +
	`<SCENE>210725</SCENE><TAKE>001</TAKE></BWFXML>`

// TestDecoderIXMLRoundTrip encodes a stream carrying an iXML chunk, decodes it,
// and checks that Decoder.IXML hands back the identical text, so a decoded
// chunk can be re-encoded losslessly. The audio must still decode unchanged.
func TestDecoderIXMLRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cfg  pcm.Config
	}{
		{"ixml_only", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: sampleIXML}},
		{"even_body", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: sampleIXML[:len(sampleIXML)-1]}},
		{"bext_and_ixml", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bext: bextFixture(), IXML: sampleIXML}},
		{"float_stream", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat, IXML: sampleIXML}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := pattern(256)
			b := encodeFixture(t, tc.cfg, src)

			d, err := pcm.NewDecoder(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if got := d.IXML(); got != tc.cfg.IXML {
				t.Errorf("round-tripped IXML = %q, want %q", got, tc.cfg.IXML)
			}

			// When a bext was also written, it must still round-trip alongside.
			if tc.cfg.Bext != nil {
				bx, err := d.Bext()
				if err != nil || bx == nil {
					t.Fatalf("Bext alongside iXML: got (%v, %v)", bx, err)
				}
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

// TestDecoderIXMLAbsent checks that a stream carrying no iXML chunk reports its
// absence as the empty string.
func TestDecoderIXMLAbsent(t *testing.T) {
	b := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64))
	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.IXML(); got != "" {
		t.Errorf("IXML = %q, want the empty string for a stream with no iXML chunk", got)
	}
}

// TestDecoderIXMLClearedOnReset checks that Reset onto an iXML-less stream stops
// reporting the previous stream's chunk.
func TestDecoderIXMLClearedOnReset(t *testing.T) {
	withIXML := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: sampleIXML}, pattern(64))
	d, err := pcm.NewDecoder(bytes.NewReader(withIXML))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.IXML(); got == "" {
		t.Fatal("IXML returned empty for a stream that carries one")
	}

	noIXML := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64))
	if err := d.Reset(bytes.NewReader(noIXML)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := d.IXML(); got != "" {
		t.Errorf("IXML = %q after Reset onto an iXML-less stream, want the empty string", got)
	}
}

// TestDecoderIXMLNilAfterFailedReset checks that a Reset that fails invalidates
// the decoder, so IXML reports the empty string rather than reaching into the
// previous stream's header.
func TestDecoderIXMLNilAfterFailedReset(t *testing.T) {
	good := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: sampleIXML}, pattern(64))
	d, err := pcm.NewDecoder(bytes.NewReader(good))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if d.IXML() == "" {
		t.Fatal("precondition: IXML empty for a stream that carries one")
	}
	if err := d.Reset(bytes.NewReader([]byte("not a wav file"))); err == nil {
		t.Fatal("Reset onto a non-WAV stream should have failed")
	}
	if got := d.IXML(); got != "" {
		t.Errorf("IXML = %q after a failed Reset, want the empty string", got)
	}
}

// TestDecoderIXMLFirstWins checks that when a stream carries more than one iXML
// chunk, the first is exposed and the rest ignored, matching the bext rule.
func TestDecoderIXMLFirstWins(t *testing.T) {
	src := pattern(64)
	clean := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, src)
	first := "<BWFXML>first</BWFXML>"
	second := "<BWFXML>second</BWFXML>"
	// Insert the second after fmt, then the first ahead of it, so the stream
	// order is fmt, first, second, data.
	withSecond := insertChunkAfter(t, clean, "fmt ", "iXML", []byte(second))
	both := insertChunkAfter(t, withSecond, "fmt ", "iXML", []byte(first))

	d, err := pcm.NewDecoder(bytes.NewReader(both))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.IXML(); got != first {
		t.Errorf("IXML = %q, want the first chunk %q", got, first)
	}
	if audio, err := io.ReadAll(d); err != nil {
		t.Fatalf("ReadAll: %v", err)
	} else if !bytes.Equal(audio, src) {
		t.Errorf("audio: got %d bytes, want %d", len(audio), len(src))
	}
}

// TestEncoderRejectsOversizeIXML checks that a Config whose iXML exceeds the
// bytes the reader will hold is refused rather than written to a file this
// package could not read back.
func TestEncoderRejectsOversizeIXML(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: strings.Repeat("x", (1<<20)+1)}
	if _, err := pcm.NewEncoder(io.Discard, cfg); !errors.Is(err, wav.ErrTooLarge) {
		t.Fatalf("NewEncoder with oversize iXML = %v, want an error wrapping wav.ErrTooLarge", err)
	}
}

// TestEncoderAcceptsMaxIXML checks the boundary: an iXML chunk exactly at the
// cap is accepted and round-trips, so the ceiling is inclusive.
func TestEncoderAcceptsMaxIXML(t *testing.T) {
	body := strings.Repeat(" ", 1<<20)
	src := pattern(64)
	b := encodeFixture(t, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, IXML: body}, src)

	d, err := pcm.NewDecoder(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.IXML(); got != body {
		t.Errorf("round-tripped max-size IXML = %d bytes, want %d", len(got), len(body))
	}
}
