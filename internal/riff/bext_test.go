package riff

import (
	"bytes"
	"encoding/binary"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// TestBuildHeaderBextPlacement checks that a bext chunk, when present, is
// written immediately after fmt and before fact or data, with a single zero
// pad byte only when its length is odd.
func TestBuildHeaderBextPlacement(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		body   []byte
	}{
		{
			name:   "pcm_even_bext",
			format: Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			body:   bytes.Repeat([]byte{0xAB}, 602),
		},
		{
			name:   "pcm_odd_bext",
			format: Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			body:   bytes.Repeat([]byte{0xCD}, 603),
		},
		{
			name:   "float_with_fact_and_bext",
			format: Format{SampleRate: 44100, Channels: 1, BitDepth: 32, Format: wav.SampleFormatFloat},
			body:   bytes.Repeat([]byte{0xEF}, 602),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := HeaderConfig{
				Format:    tc.format,
				Container: wav.ContainerRIFF,
				Bext:      tc.body,
			}
			lay, err := BuildHeader(cfg)
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}

			fmtAt := bytes.Index(lay.Bytes, []byte(idFmt))
			if fmtAt < 0 {
				t.Fatalf("no fmt chunk in header")
			}
			fmtLen := int(binary.LittleEndian.Uint32(lay.Bytes[fmtAt+4:]))
			bextAt := fmtAt + ChunkHeaderSize + fmtLen

			if got := string(lay.Bytes[bextAt : bextAt+4]); got != idBext {
				t.Fatalf("chunk right after fmt = %q, want %q", got, idBext)
			}
			gotSize := binary.LittleEndian.Uint32(lay.Bytes[bextAt+4:])
			if int(gotSize) != len(tc.body) {
				t.Errorf("bext chunk size = %d, want %d", gotSize, len(tc.body))
			}
			payloadAt := bextAt + ChunkHeaderSize
			if !bytes.Equal(lay.Bytes[payloadAt:payloadAt+len(tc.body)], tc.body) {
				t.Errorf("bext payload does not match what BuildHeader was given")
			}

			next := payloadAt + len(tc.body)
			if len(tc.body)%2 != 0 {
				if lay.Bytes[next] != 0 {
					t.Errorf("odd length bext has no zero pad byte at offset %d", next)
				}
				next++
			}

			wantNextID := idData
			if tc.format.Format == wav.SampleFormatFloat {
				wantNextID = idFact
			}
			if got := string(lay.Bytes[next : next+4]); got != wantNextID {
				t.Errorf("chunk after bext = %q, want %q", got, wantNextID)
			}

			if lay.DataOffset != int64(len(lay.Bytes)) {
				t.Errorf("DataOffset = %d, want %d (len of Bytes)", lay.DataOffset, len(lay.Bytes))
			}
		})
	}
}

// TestBuildHeaderNoBextByDefault checks that leaving HeaderConfig.Bext at its
// zero value, nil, writes no bext chunk at all, so a caller who never touches
// the field gets exactly the header it always got.
func TestBuildHeaderNoBextByDefault(t *testing.T) {
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	if bytes.Contains(lay.Bytes, []byte(idBext)) {
		t.Errorf("header contains a bext chunk despite HeaderConfig.Bext being nil")
	}
}
