package riff

import (
	"errors"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// TestParseCompandedFormatTags checks that the two G.711 format tags parse to
// the sample format they name, in both the bare-tag and the extensible form.
//
// A real A-law file carries the 18-byte fmt chunk, because the format requires
// a cbSize field on every non-PCM encoding, so that is the form the bare case
// uses. The extensible form is rarer but legal, and the GUIDs it carries are
// the format tag padded into the KSDATAFORMAT namespace.
func TestParseCompandedFormatTags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    wav.SampleFormat
	}{
		{"alaw_bare_tag", fmtPayload18(tagALaw, 1, 8000, 8), wav.SampleFormatALaw},
		{"mulaw_bare_tag", fmtPayload18(tagMuLaw, 1, 8000, 8), wav.SampleFormatMuLaw},
		{"alaw_16_byte_fmt", fmtPayload16(tagALaw, 2, 44100, 8), wav.SampleFormatALaw},
		{"mulaw_16_byte_fmt", fmtPayload16(tagMuLaw, 2, 44100, 8), wav.SampleFormatMuLaw},
		{"alaw_extensible", fmtPayload40(1, 8000, 8, 8, 0x4, guidALaw), wav.SampleFormatALaw},
		{"mulaw_extensible", fmtPayload40(1, 8000, 8, 8, 0x4, guidMuLaw), wav.SampleFormatMuLaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseBytes(cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, tc.payload),
				chunk(idData, make([]byte, 8))))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.Info.SourceFormat != tc.want {
				t.Errorf("SourceFormat = %v, want %v", h.Info.SourceFormat, tc.want)
			}
			if h.Info.SourceBitDepth != 8 {
				t.Errorf("SourceBitDepth = %d, want 8", h.Info.SourceBitDepth)
			}
		})
	}
}

// TestCompandedBlockAlignAndFrames checks the frame arithmetic of a companded
// stream, which is the one place its 8-bit storage and its 16-bit output could
// be confused. A companded frame is one byte per channel on disk, exactly as
// the fmt chunk of a real A-law file records it, so the frame count follows
// from the data chunk size divided by the channel count alone.
func TestCompandedBlockAlignAndFrames(t *testing.T) {
	const frames = 40
	h, err := parseBytes(cat(fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, fmtPayload18(tagALaw, 2, 8000, 8)),
		chunk(idData, make([]byte, frames*2))))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.BlockAlign != 2 {
		t.Errorf("BlockAlign = %d, want 2 (one byte per channel)", h.BlockAlign)
	}
	if h.Info.TotalFrames != frames {
		t.Errorf("TotalFrames = %d, want %d", h.Info.TotalFrames, frames)
	}
}

// TestParseCompandedRejectsOtherDepths checks that a fmt chunk claiming a
// companding law at a width G.711 does not define is refused. Such a file
// describes nothing that exists, and guessing that the depth field is wrong
// while the tag is right would decode noise.
func TestParseCompandedRejectsOtherDepths(t *testing.T) {
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		for _, bits := range []int{4, 12, 16, 24, 32} {
			_, err := parseBytes(cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload18(tag, 1, 8000, bits)),
				chunk(idData, make([]byte, 8))))
			if !errors.Is(err, wav.ErrUnsupported) {
				t.Errorf("tag 0x%04X at %d bits: error = %v, want wav.ErrUnsupported", tag, bits, err)
			}
		}
	}
}
