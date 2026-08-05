package riff

import (
	"bytes"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// TestParseHeaderCapturesBext checks that ParseHeader hands the raw bext chunk
// body back through Header.Bext, that an odd-length body still lets the walk
// reach data, and that a stream without a bext chunk yields a nil Bext.
func TestParseHeaderCapturesBext(t *testing.T) {
	const dataSize = int64(64)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"even_body", bytes.Repeat([]byte{0xAB}, 700)},
		{"odd_body", bytes.Repeat([]byte{0xCD}, 701)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRIFF,
				DataSize:  dataSize,
				Frames:    uint64(dataSize / 2),
				Bext:      tc.body,
			}
			lay, err := BuildHeader(cfg)
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}
			h, err := parseBytes(cat(lay.Bytes, make([]byte, padded(dataSize))))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if !bytes.Equal(h.Bext, tc.body) {
				t.Errorf("Header.Bext = %d bytes, want the %d byte body verbatim", len(h.Bext), len(tc.body))
			}
			if h.DataSize != dataSize {
				t.Errorf("DataSize = %d, want %d: the walk did not reach data past the bext chunk", h.DataSize, dataSize)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		cfg := HeaderConfig{
			Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			Container: wav.ContainerRIFF,
			DataSize:  dataSize,
			Frames:    uint64(dataSize / 2),
		}
		lay, err := BuildHeader(cfg)
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		h, err := parseBytes(cat(lay.Bytes, make([]byte, padded(dataSize))))
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if h.Bext != nil {
			t.Errorf("Header.Bext = %d bytes for a stream with no bext, want nil", len(h.Bext))
		}
	})
}
