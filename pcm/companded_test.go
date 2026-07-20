package pcm_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// WAVE format tags for the two companding laws, repeated here rather than
// reached for across the internal boundary, since these tests build their
// fixtures byte by byte.
const (
	tagALaw  uint16 = 0x0006
	tagMuLaw uint16 = 0x0007
)

// compandedFile builds a minimal RIFF WAVE stream carrying a companded payload.
// It writes the 18-byte fmt chunk real encoders use for a non-PCM tag, so the
// fixture matches what ffmpeg and sox produce rather than a form only this test
// would ever see.
func compandedFile(tb testing.TB, tag uint16, channels, rate int, payload []byte) []byte {
	tb.Helper()
	//nolint:gosec // G115: test values are small.
	blockAlign := uint16(channels)

	var fmtChunk bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&fmtChunk, binary.LittleEndian, v); err != nil {
			tb.Fatalf("building fmt chunk: %v", err)
		}
	}
	write(tag)
	write(uint16(channels)) //nolint:gosec // G115: test values are small.
	write(uint32(rate))     //nolint:gosec // G115: test values are small.
	//nolint:gosec // G115: test values are small.
	write(uint32(rate) * uint32(blockAlign)) // byte rate
	write(blockAlign)
	write(uint16(8)) // bits per sample
	write(uint16(0)) // cbSize, required on every non-PCM tag

	body := new(bytes.Buffer)
	body.WriteString("WAVE")
	body.WriteString("fmt ")
	//nolint:gosec // G115: test values are small.
	_ = binary.Write(body, binary.LittleEndian, uint32(fmtChunk.Len()))
	body.Write(fmtChunk.Bytes())
	body.WriteString("data")
	//nolint:gosec // G115: test values are small.
	_ = binary.Write(body, binary.LittleEndian, uint32(len(payload)))
	body.Write(payload)
	if len(payload)%2 != 0 {
		body.WriteByte(0)
	}

	out := new(bytes.Buffer)
	out.WriteString("RIFF")
	//nolint:gosec // G115: test values are small.
	_ = binary.Write(out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// allCodes is every 8-bit code, which is also every input either law has.
func allCodes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// TestDecodeCompandedInfo pins what a decoder says about a companded stream.
//
// The stored encoding stays visible in SourceFormat and SourceBitDepth, and
// what Read yields is linear 16-bit PCM, so BitDepth and Format describe that.
// The derived widths have to follow BitDepth rather than the stored width, or
// a caller sizing a buffer from BytesPerFrame would get half of what it needs.
// TotalFrames counts frames, which the expansion does not change.
func TestDecodeCompandedInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  uint16
		want wav.SampleFormat
	}{
		{"alaw", tagALaw, wav.SampleFormatALaw},
		{"mulaw", tagMuLaw, wav.SampleFormatMuLaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const frames = 128
			file := compandedFile(t, tc.tag, 2, 8000, make([]byte, frames*2))
			d, err := pcm.NewDecoder(bytes.NewReader(file))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			info := d.Info()

			if info.SourceFormat != tc.want {
				t.Errorf("SourceFormat = %v, want %v", info.SourceFormat, tc.want)
			}
			if !info.SourceFormat.Companded() {
				t.Errorf("SourceFormat %v does not report itself as companded", info.SourceFormat)
			}
			if info.SourceBitDepth != 8 {
				t.Errorf("SourceBitDepth = %d, want 8", info.SourceBitDepth)
			}
			if info.Format != wav.SampleFormatPCM {
				t.Errorf("Format = %v, want pcm", info.Format)
			}
			if info.BitDepth != 16 {
				t.Errorf("BitDepth = %d, want 16", info.BitDepth)
			}
			if info.BytesPerSample() != 2 {
				t.Errorf("BytesPerSample = %d, want 2", info.BytesPerSample())
			}
			if info.BytesPerFrame() != 4 {
				t.Errorf("BytesPerFrame = %d, want 4", info.BytesPerFrame())
			}
			if info.TotalFrames != frames {
				t.Errorf("TotalFrames = %d, want %d", info.TotalFrames, frames)
			}
		})
	}
}

// TestDecodeCompandedYieldsLinearPCM checks that Read hands back what Info
// promises: two bytes of linear 16-bit PCM for every stored code, never the
// stored byte itself. This is the invariant the decision to expand rests on, so
// it is checked against the shape of the output as well as its length.
func TestDecodeCompandedYieldsLinearPCM(t *testing.T) {
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		codes := allCodes()
		file := compandedFile(t, tag, 1, 8000, codes)
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != len(codes)*2 {
			t.Fatalf("read %d bytes from %d codes, want %d", len(got), len(codes), len(codes)*2)
		}
		if bytes.Equal(got[:len(codes)], codes) {
			t.Error("Read handed back the stored companded bytes rather than linear PCM")
		}
	}
}

// TestDecodeCompandedToEveryDepth checks the conversion option over a companded
// source at every width it accepts, against decoding to 16 bits and requantising
// from there. A caller asking for 24-bit output must get the same answer either
// way, or the companded path has invented a rounding rule of its own.
func TestDecodeCompandedToEveryDepth(t *testing.T) {
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		file := compandedFile(t, tag, 1, 8000, allCodes())

		base := readAllConverted(t, file, 16)
		for _, depth := range []int{8, 16, 24, 32} {
			got := readAllConverted(t, file, depth)

			// The same 16-bit samples put through the ordinary integer path,
			// by encoding them as a linear 16-bit file and converting that.
			linear := encodeFixture(t, pcm.Config{SampleRate: 8000, BitDepth: 16, Channels: 1}, base)
			want := readAllConverted(t, linear, depth)

			if !bytes.Equal(got, want) {
				t.Errorf("tag 0x%04X at %d bits differs from decoding to 16 and converting", tag, depth)
			}
		}
	}
}

// TestDecodeCompandedDefaultMatchesExplicit16 checks that the implicit
// expansion and an explicit WithConvertTo(16) are the same thing, so that a
// caller who spells out what it wants is not on a different code path from one
// who takes the default.
func TestDecodeCompandedDefaultMatchesExplicit16(t *testing.T) {
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		file := compandedFile(t, tag, 1, 8000, allCodes())

		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		implicit, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		explicit := readAllConverted(t, file, 16)
		if !bytes.Equal(implicit, explicit) {
			t.Errorf("tag 0x%04X: the default differs from WithConvertTo(16)", tag)
		}
	}
}

// TestSeekCompandedFrames checks that seeking counts frames in stored bytes
// while Read yields expanded ones. The two widths are different here, which is
// exactly the confusion a seek could make, so the test lands mid-stream and
// compares against the tail of a full read.
func TestSeekCompandedFrames(t *testing.T) {
	const skip = 100
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		file := compandedFile(t, tag, 1, 8000, allCodes())

		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		whole, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		d, err = pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		at, err := d.SeekToFrame(skip)
		if err != nil {
			t.Fatalf("SeekToFrame: %v", err)
		}
		if at != skip {
			t.Fatalf("SeekToFrame reached frame %d, want %d", at, skip)
		}
		tail, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("read after seek: %v", err)
		}
		if !bytes.Equal(tail, whole[skip*2:]) {
			t.Errorf("tag 0x%04X: reading from frame %d does not match the tail of a whole read", tag, skip)
		}

		// Past the end, the clamp has to be computed in stored bytes too. Doing
		// it in returned bytes would put the boundary at twice the frame count
		// the file actually holds, and the seek would land beyond the audio.
		d, err = pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		at, err = d.SeekToFrame(1 << 20)
		if err != nil {
			t.Fatalf("SeekToFrame past the end: %v", err)
		}
		if want := int64(len(allCodes())); at != want {
			t.Errorf("tag 0x%04X: a seek past the end reached frame %d, want the %d frames stored",
				tag, at, want)
		}
		rest, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("read after seeking past the end: %v", err)
		}
		if len(rest) != 0 {
			t.Errorf("tag 0x%04X: a seek past the end left %d bytes to read", tag, len(rest))
		}
	}
}

// TestDecodeInterleavedCompanded covers the one-shot path over a companded
// source, where the package's two decisions collide: DecodeInterleaved hands
// back a window onto the caller's own buffer when it can, and a companded
// source is expanded whether or not a conversion was asked for. Expanding
// doubles the width, so there is nothing in the input for the result to be a
// window onto, and the no-option call has to allocate like a converting one.
// This pins that it does, that the result still ends at its capacity, and that
// it is the same audio the streaming decoder produces.
func TestDecodeInterleavedCompanded(t *testing.T) {
	for _, tc := range []struct {
		tag  uint16
		want wav.SampleFormat
	}{
		{tagALaw, wav.SampleFormatALaw},
		{tagMuLaw, wav.SampleFormatMuLaw},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			codes := allCodes()
			file := compandedFile(t, tc.tag, 1, 8000, codes)
			start := dataOffset(t, file)
			before := bytes.Clone(file)

			// The same audio the streaming decoder yields, from the same
			// bytes, is checked first: everything below is about the shape of
			// the result rather than its content.
			got := assertMatchesDecoder(t, file)
			if len(got) != len(codes)*2 {
				t.Fatalf("audio: got %d bytes from %d codes, want %d", len(got), len(codes), len(codes)*2)
			}

			// Not a window onto the input, by address and then by writing.
			if &got[0] == &file[start] {
				t.Fatal("the returned slice begins at the data chunk of the input, so an expansion aliased it")
			}
			got[0] ^= 0xFF
			got[len(got)-1] ^= 0xFF
			if !bytes.Equal(file, before) {
				t.Error("writing through the returned slice reached the caller's buffer")
			}

			// The allocated result still ends at its length, so a caller's
			// append cannot walk off it into anything else.
			if cap(got) != len(got) {
				t.Errorf("returned slice has capacity %d for a length of %d", cap(got), len(got))
			}

			// And the info returned alongside describes those bytes, not the
			// stored ones, with the stored encoding still readable.
			info, _, err := pcm.DecodeInterleaved(file)
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if info.Format != wav.SampleFormatPCM || info.BitDepth != 16 {
				t.Errorf("StreamInfo describes the returned bytes as %v/%d, want pcm/16",
					info.Format, info.BitDepth)
			}
			if info.SourceFormat != tc.want || info.SourceBitDepth != 8 {
				t.Errorf("StreamInfo describes the stored bytes as %v/%d, want %v/8",
					info.SourceFormat, info.SourceBitDepth, tc.want)
			}
			if want := uint64(len(codes)); info.TotalFrames != want {
				t.Errorf("TotalFrames = %d, want %d", info.TotalFrames, want)
			}
			if int(info.TotalFrames)*info.BytesPerFrame() != len(got) {
				t.Errorf("%d frames of %d bytes does not account for the %d bytes returned",
					info.TotalFrames, info.BytesPerFrame(), len(got))
			}
		})
	}
}

// TestDecodeInterleavedCompandedWidths checks the one-shot path over a
// companded source at every width the conversion option accepts, including the
// implicit 16 the source takes with no option at all. A caller that spells out
// what it wants must not be on a different code path from one that does not.
func TestDecodeInterleavedCompandedWidths(t *testing.T) {
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		file := compandedFile(t, tag, 2, 8000, allCodes())

		implicit := assertMatchesDecoder(t, file)
		explicit := assertMatchesDecoder(t, file, pcm.WithConvertTo(16))
		if !bytes.Equal(implicit, explicit) {
			t.Errorf("tag 0x%04X: the default differs from WithConvertTo(16)", tag)
		}

		for _, depth := range []int{8, 16, 24, 32} {
			got := assertMatchesDecoder(t, file, pcm.WithConvertTo(depth))
			if want := len(allCodes()) * (depth / 8); len(got) != want {
				t.Errorf("tag 0x%04X at %d bits: got %d bytes want %d", tag, depth, len(got), want)
			}
			if cap(got) != len(got) {
				t.Errorf("tag 0x%04X at %d bits: capacity %d for a length of %d",
					tag, depth, cap(got), len(got))
			}
		}
	}
}

// TestDecodeInterleavedCompandedShortStreams covers the bounds the one-shot
// path computes from the header, over a source whose stored and returned widths
// differ. Each of these is a place where slicing by the wrong width would
// either overrun the buffer or hand back the wrong number of samples, and each
// is checked against the streaming decoder rather than against a length this
// test worked out for itself.
func TestDecodeInterleavedCompandedShortStreams(t *testing.T) {
	const codes = 37 // odd, so the data chunk carries a pad byte
	for _, tag := range []uint16{tagALaw, tagMuLaw} {
		full := compandedFile(t, tag, 1, 8000, pattern(codes))
		start := dataOffset(t, full)

		t.Run(fmt.Sprintf("tag %#04x odd length", tag), func(t *testing.T) {
			got := assertMatchesDecoder(t, full)
			if len(got) != codes*2 {
				t.Errorf("got %d bytes, want %d, so the pad byte was expanded as a sample",
					len(got), codes*2)
			}
		})

		t.Run(fmt.Sprintf("tag %#04x truncated", tag), func(t *testing.T) {
			cut := full[:len(full)-10]
			got := assertMatchesDecoder(t, cut)
			if want := (len(cut) - start) * 2; len(got) != want {
				t.Errorf("got %d bytes, want the %d present codes expanded", len(got), want)
			}
		})

		t.Run(fmt.Sprintf("tag %#04x header only", tag), func(t *testing.T) {
			got := assertMatchesDecoder(t, full[:start])
			if len(got) != 0 {
				t.Errorf("a stream that ends at its data chunk yielded %d bytes", len(got))
			}
		})

		t.Run(fmt.Sprintf("tag %#04x unknown length", tag), func(t *testing.T) {
			// An even payload, so that no pad byte blurs what reading to the
			// end of the source is supposed to pick up.
			even := compandedFile(t, tag, 1, 8000, pattern(codes+1))
			file := patchDataSize(t, even, 0)
			got := assertMatchesDecoder(t, file)
			if want := (len(file) - dataOffset(t, file)) * 2; len(got) != want {
				t.Errorf("got %d bytes, want the %d bytes present expanded", len(got), want)
			}
		})

		t.Run(fmt.Sprintf("tag %#04x ignore length", tag), func(t *testing.T) {
			assertMatchesDecoder(t, full, pcm.WithIgnoreLength())
		})
	}
}

// TestEncodeCompandedRejected checks that asking the encoder for a companding
// law is refused rather than quietly producing a linear PCM file wearing an
// A-law tag. Decoding is supported and encoding is not, and the error has to
// say so at the point the caller asks.
func TestEncodeCompandedRejected(t *testing.T) {
	for _, format := range []wav.SampleFormat{wav.SampleFormatALaw, wav.SampleFormatMuLaw} {
		cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1, Format: format}

		if _, err := pcm.NewEncoder(io.Discard, cfg); !errors.Is(err, wav.ErrUnsupported) {
			t.Errorf("NewEncoder with %v: error = %v, want wav.ErrUnsupported", format, err)
		}
		if err := pcm.EncodeInterleaved(io.Discard, cfg, make([]byte, 8)); !errors.Is(err, wav.ErrUnsupported) {
			t.Errorf("EncodeInterleaved with %v: error = %v, want wav.ErrUnsupported", format, err)
		}
	}
}
