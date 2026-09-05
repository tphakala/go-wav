package riff

import (
	"bytes"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// baseIXMLFormat is the trivial fmt used by the iXML round-trip tests.
func baseIXMLFormat() Format {
	return Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM}
}

// TestParseHeaderCapturesIXML mirrors TestParseHeaderCapturesBext: the raw iXML
// body comes back through Header.IXML, an odd-length body still lets the walk
// reach data, and a stream without an iXML chunk yields a nil IXML.
func TestParseHeaderCapturesIXML(t *testing.T) {
	const dataSize = int64(64)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"even_body", bytes.Repeat([]byte{0x3C}, 700)},
		{"odd_body", bytes.Repeat([]byte{0x3E}, 701)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := HeaderConfig{
				Format:    baseIXMLFormat(),
				Container: wav.ContainerRIFF,
				DataSize:  dataSize,
				Frames:    uint64(dataSize / 2),
				IXML:      tc.body,
			}
			lay, err := BuildHeader(cfg)
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}
			h, err := parseBytes(cat(lay.Bytes, make([]byte, padded(dataSize))))
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if !bytes.Equal(h.IXML, tc.body) {
				t.Errorf("Header.IXML = %d bytes, want the %d byte body verbatim", len(h.IXML), len(tc.body))
			}
			if h.DataSize != dataSize {
				t.Errorf("DataSize = %d, want %d: the walk did not reach data past the iXML chunk", h.DataSize, dataSize)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		cfg := HeaderConfig{
			Format:    baseIXMLFormat(),
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
		if h.IXML != nil {
			t.Errorf("Header.IXML = %d bytes for a stream with no iXML, want nil", len(h.IXML))
		}
	})
}

// TestBuildHeaderWritesIXMLAfterBext checks that a header carrying both chunks
// places iXML after bext (fmt, bext, iXML, data), and that both survive a
// round trip through ParseHeader.
func TestBuildHeaderWritesIXMLAfterBext(t *testing.T) {
	const dataSize = int64(64)
	bext := bytes.Repeat([]byte{0xAB}, 610)
	ixml := []byte("<BWFXML><TAKE>001</TAKE></BWFXML>")

	cfg := HeaderConfig{
		Format:    baseIXMLFormat(),
		Container: wav.ContainerRIFF,
		DataSize:  dataSize,
		Frames:    uint64(dataSize / 2),
		Bext:      bext,
		IXML:      ixml,
	}
	lay, err := BuildHeader(cfg)
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}

	bextAt := bytes.Index(lay.Bytes, []byte(idBext))
	ixmlAt := bytes.Index(lay.Bytes, []byte(idIXML))
	if bextAt < 0 || ixmlAt < 0 {
		t.Fatalf("missing chunk id: bext@%d iXML@%d", bextAt, ixmlAt)
	}
	if ixmlAt < bextAt {
		t.Errorf("iXML chunk at %d precedes bext at %d, want iXML after bext", ixmlAt, bextAt)
	}

	h, err := parseBytes(cat(lay.Bytes, make([]byte, padded(dataSize))))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !bytes.Equal(h.Bext, bext) {
		t.Errorf("round-trip Bext = %d bytes, want %d", len(h.Bext), len(bext))
	}
	if !bytes.Equal(h.IXML, ixml) {
		t.Errorf("round-trip IXML = %q, want %q", h.IXML, ixml)
	}
}

// TestHeaderLenCountsIXML checks that HeaderLen accounts for the iXML chunk it
// will write: header id and size plus the word-padded body.
func TestHeaderLenCountsIXML(t *testing.T) {
	base := HeaderConfig{Format: baseIXMLFormat(), Container: wav.ContainerRIFF}

	for _, n := range []int{1, 32, 33, 5226} {
		body := make([]byte, n)
		withIXML := base
		withIXML.IXML = body
		got := HeaderLen(withIXML) - HeaderLen(base)
		want := int64(ChunkHeaderSize) + padded(int64(n))
		if got != want {
			t.Errorf("HeaderLen delta for %d-byte iXML = %d, want %d", n, got, want)
		}
	}
}

// TestParseHeaderSkipsOversizeIXML checks that an iXML chunk larger than the
// in-memory cap is skipped, reported as nil, without derailing the walk to data.
func TestParseHeaderSkipsOversizeIXML(t *testing.T) {
	const dataSize = int64(64)
	body := bytes.Repeat([]byte{0x2A}, MaxChunkPayload+1)

	cfg := HeaderConfig{
		Format:    baseIXMLFormat(),
		Container: wav.ContainerRIFF,
		DataSize:  dataSize,
		Frames:    uint64(dataSize / 2),
		IXML:      body,
	}
	lay, err := BuildHeader(cfg)
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	h, err := parseBytes(cat(lay.Bytes, make([]byte, padded(dataSize))))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.IXML != nil {
		t.Errorf("Header.IXML = %d bytes for an oversize chunk, want nil (skipped)", len(h.IXML))
	}
	if h.DataSize != dataSize {
		t.Errorf("DataSize = %d, want %d: the walk did not reach data past the skipped iXML", h.DataSize, dataSize)
	}
}
