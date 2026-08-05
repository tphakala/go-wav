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

// FuzzDecode checks that no arbitrary byte string can make the decoder panic.
// A WAV header is entirely attacker controlled, so every size field, chunk walk
// and conversion path has to survive nonsense.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("RIFF"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVE"))
	// BW64 has no encoder here, so its magic reaches the header path only from
	// a hand-built seed; without this the fuzzer would rarely assemble it.
	f.Add([]byte("BW64\x00\x00\x00\x00WAVE"))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}, pattern(120)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2,
		Format: wav.SampleFormatFloat}, floatPattern(64, 32)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}, pattern(7)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
		RF64: pcm.RF64Always}, pattern(64)))
	// The two companding laws, which no encoder here can produce, so their
	// seeds are hand-built. They are the one source the decoder expands
	// without being asked, so a mutation of one reaches that path with a
	// header the fuzzer would rarely assemble on its own.
	f.Add(compandedFile(f, tagALaw, 1, 8000, pattern(37)))
	f.Add(compandedFile(f, tagMuLaw, 2, 8000, pattern(64)))

	f.Fuzz(func(t *testing.T, data []byte) {
		optionSets := [][]pcm.Option{
			nil,
			{pcm.WithIgnoreLength()},
			{pcm.WithConvertTo(16)},
			{pcm.WithConvertTo(32), pcm.WithIgnoreLength()},
			{pcm.WithConvertTo(8)},
			{pcm.WithConvertTo(24)},
		}
		for _, opts := range optionSets {
			d, err := pcm.NewDecoder(bytes.NewReader(data), opts...)
			if err != nil {
				// The one-shot path parses the same header, so a stream the
				// streaming decoder refuses must be refused here too rather
				// than sliced by bounds nothing validated.
				if _, got, oneErr := pcm.DecodeInterleaved(data, opts...); oneErr == nil {
					t.Fatalf("DecodeInterleaved accepted %d bytes of a stream NewDecoder refused with %v",
						len(got), err)
				}
				continue
			}
			info := d.Info()
			if info.Channels <= 0 {
				t.Fatalf("a decoder was built for a stream with %d channels", info.Channels)
			}
			_ = info.Duration()
			_ = info.BytesPerFrame()

			// Draining must terminate and must not panic.
			var streamed bytes.Buffer
			if _, err := io.Copy(&streamed, d); err != nil {
				continue
			}

			// Every bound the one-shot path computes comes from the same
			// attacker-controlled header, and it slices rather than reads, so
			// a bound the streaming path merely stops early on is one this
			// path would panic on. Whatever the header claims, the two must
			// agree byte for byte.
			oneInfo, one, oneErr := pcm.DecodeInterleaved(data, opts...)
			if oneErr != nil {
				t.Fatalf("DecodeInterleaved refused a stream the decoder drained: %v", oneErr)
			}
			if oneInfo != info {
				t.Fatalf("StreamInfo: DecodeInterleaved reports %+v, the decoder reports %+v", oneInfo, info)
			}
			// The result is a window onto the input exactly when the bytes
			// handed back are the bytes as stored, which needs both that no
			// conversion was asked for and that the source is not companded:
			// a companding law is expanded to linear 16-bit with or without
			// the option, so its result is about twice the stored audio and
			// cannot possibly alias it. Stating the condition as "no options"
			// alone would be the stale form of this invariant.
			//
			// A window can be no longer than the input it looks into. A
			// rewritten result is a fresh buffer at a different width, which
			// may legitimately be wider than the stream it came from.
			aliases := opts == nil && !info.SourceFormat.Companded()
			if aliases && len(one) > len(data) {
				t.Fatalf("a pass-through DecodeInterleaved returned %d bytes from a %d byte stream",
					len(one), len(data))
			}
			// The capacity has to stop exactly at the length either way. On the
			// aliasing path any spare capacity at all is room for a caller's
			// append to overwrite whatever their own buffer holds past the
			// audio; on the allocated path it is a promise the package makes so
			// that appending behaves the same whichever path a file took.
			if cap(one) != len(one) {
				t.Fatalf("DecodeInterleaved returned %d bytes with capacity %d from a %d byte stream",
					len(one), cap(one), len(data))
			}
			if !bytes.Equal(one, streamed.Bytes()) {
				t.Fatalf("audio: DecodeInterleaved returned %d bytes, the decoder returned %d",
					len(one), streamed.Len())
			}
		}

		// Sniff and the decoder must agree on what a WAVE header is. They carry
		// separate copies of the magic table: wav cannot import internal/riff
		// (that would be a cycle, since riff imports wav), and the decoder there
		// parses a stream with richer errors rather than sniffing a slice, so
		// the two are written independently and must not drift. Only this
		// direction is an invariant: the decoder rejects plenty of headers Sniff
		// accepts, for reasons past byte twelve, but it must never reject one
		// with ErrNotRIFF.
		if container, ok := wav.SniffContainer(data); ok {
			d, err := pcm.NewDecoder(bytes.NewReader(data))
			if errors.Is(err, wav.ErrNotRIFF) {
				t.Fatalf("SniffContainer accepted a header the decoder rejected as not-RIFF: %v", err)
			}
			// When the decoder does accept it, the two must also agree on WHICH
			// container it is, so the RIFF/RF64/BW64 discrimination cannot drift
			// between the twelve-byte sniff and the full parse.
			if err == nil && d.Info().Container != container {
				t.Fatalf("SniffContainer saw %v but the decoder parsed %v", container, d.Info().Container)
			}
		}
	})
}

// FuzzDecodeSeek exercises the seek path against arbitrary streams, since
// SeekToFrame does arithmetic on attacker controlled sizes.
func FuzzDecodeSeek(f *testing.F) {
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}, pattern(400)), int64(3))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVE"), int64(0))
	// The frame index that makes frame*bytesPerFrame wrap to exactly minus the
	// data chunk's start offset, which used to seek to byte zero and report a
	// negative frame. Random fuzzing would effectively never find it, since it
	// has to hit one value out of the whole int64 range, so it is seeded.
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 2}, pattern(400)),
		int64(4611686018427387893))
	// A companded source, where a seek counts stored frames one byte wide while
	// Read yields expanded ones two bytes wide. Every other seed has the two
	// widths equal, so none of them would catch an offset computed against the
	// wrong one.
	f.Add(compandedFile(f, tagALaw, 2, 8000, pattern(400)), int64(7))

	f.Fuzz(func(t *testing.T, data []byte, frame int64) {
		d, err := pcm.NewDecoder(bytes.NewReader(data))
		if err != nil {
			return
		}
		got, serr := d.SeekToFrame(frame)
		if serr != nil {
			return
		}
		if got < 0 {
			t.Fatalf("SeekToFrame(%d) reported a negative frame %d", frame, got)
		}
		_, _ = io.Copy(io.Discard, d)
	})
}

// FuzzRoundTrip checks that an arbitrary payload survives an encode and decode
// under any supported configuration.
func FuzzRoundTrip(f *testing.F) {
	f.Add(pattern(64), uint8(1), uint8(1), uint8(0), uint8(0))
	f.Add(pattern(300), uint8(2), uint8(2), uint8(0), uint8(1))
	f.Add([]byte{}, uint8(0), uint8(1), uint8(0), uint8(2))
	f.Add(pattern(999), uint8(3), uint8(6), uint8(1), uint8(0))

	f.Fuzz(func(t *testing.T, payload []byte, depthSel, chanSel, formatSel, modeSel uint8) {
		// Keep the configuration inside the supported set; the point is the
		// payload, not another pass over Config.validate.
		intDepths := []int{8, 16, 24, 32}
		floatDepths := []int{32, 64}
		modes := []pcm.RF64Mode{pcm.RF64Auto, pcm.RF64Never, pcm.RF64Always}

		cfg := pcm.Config{
			SampleRate: 48000,
			Channels:   1 + int(chanSel)%8,
			RF64:       modes[int(modeSel)%len(modes)],
		}
		if formatSel%2 == 0 {
			cfg.Format = wav.SampleFormatPCM
			cfg.BitDepth = intDepths[int(depthSel)%len(intDepths)]
		} else {
			cfg.Format = wav.SampleFormatFloat
			cfg.BitDepth = floatDepths[int(depthSel)%len(floatDepths)]
		}

		// The one-shot path requires whole frames.
		perFrame := cfg.Channels * ((cfg.BitDepth + 7) / 8)
		payload = payload[:len(payload)-len(payload)%perFrame]

		var buf bytes.Buffer
		if err := pcm.EncodeInterleaved(&buf, cfg, payload); err != nil {
			// RF64Always with an empty payload on a plain writer is the one
			// configuration that legitimately has nothing to describe.
			if cfg.RF64 == pcm.RF64Always && len(payload) == 0 {
				return
			}
			t.Fatalf("EncodeInterleaved with %+v and %d bytes: %v", cfg, len(payload), err)
		}

		if !wav.Sniff(buf.Bytes()) {
			t.Fatal("Sniff rejected our own output")
		}

		d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("NewDecoder on our own output: %v", err)
		}
		info := d.Info()
		if info.SampleRate != cfg.SampleRate || info.Channels != cfg.Channels ||
			info.BitDepth != cfg.BitDepth || info.Format != cfg.Format {
			t.Fatalf("stream info %+v does not match config %+v", info, cfg)
		}
		if want := uint64(len(payload) / perFrame); info.TotalFrames != want {
			t.Fatalf("TotalFrames: got %d want %d", info.TotalFrames, want)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload did not survive: got %d bytes want %d", len(got), len(payload))
		}
	})
}

// bextDescriptionWidth is Bext.Description's wire width, restated here the
// same way rf64_test.go and encoder_test.go restate riff wire constants
// locally rather than reaching into internal/riff, since the point of a
// black box test is not to trust the package under test for its own layout.
const bextDescriptionWidth = 256

// FuzzBext checks that Bext.Validate and Bext.Serialize never panic on
// arbitrary field values, since a Config.Bext built from attacker controlled
// strings (metadata read from an untrusted source and forwarded into a
// recording) must not be able to crash a program that calls NewEncoder. It
// also checks that whenever Validate accepts a descriptor, Serialize's output
// has exactly the length the fixed body plus CodingHistory implies, and that
// Description and CodingHistory land at their documented offsets rather than
// bleeding into a neighbouring field.
func FuzzBext(f *testing.F) {
	f.Add("field recording, site 4", "recorder-01", "REF0001",
		"2026-08-05", "09:15:30", "A=PCM,F=48000,W=16,M=mono\r\n")
	// One byte over the Description width: Validate must refuse it rather
	// than let Serialize truncate it silently.
	f.Add(strings.Repeat("x", bextDescriptionWidth+1), "", "", "", "", "")
	// A CodingHistory carrying the CR LF row break the format allows.
	f.Add("", "", "", "", "", "row one\r\nrow two\r\n")
	// Junk: control bytes, non-ASCII, and a garbage date/time shape.
	f.Add("\x00\x07\x1f", "café", "\x1b[31m", "not-a-date", "25:99:99", "\x00junk\x07")

	f.Fuzz(func(t *testing.T, description, originator, originatorReference,
		originationDate, originationTime, codingHistory string,
	) {
		b := &pcm.Bext{
			Description:         description,
			Originator:          originator,
			OriginatorReference: originatorReference,
			OriginationDate:     originationDate,
			OriginationTime:     originationTime,
			CodingHistory:       codingHistory,
		}

		// Neither call may panic on any input: a panic here is a crash a
		// caller forwarding untrusted metadata into Config.Bext could trigger.
		err := b.Validate("fuzz")
		body := b.Serialize()

		if err != nil {
			return
		}
		if want := pcm.BextFixedSize + len(codingHistory); len(body) != want {
			t.Fatalf("Serialize() = %d bytes for an accepted Bext, want %d", len(body), want)
		}
		// Description starts at offset 0. Validate having accepted it means
		// it is printable ASCII with no NUL, so the field this exact prefix
		// occupies cannot itself contain the padding checked just below.
		if got := string(body[:len(description)]); got != description {
			t.Fatalf("Description at offset 0: got %q want %q", got, description)
		}
		// The bytes between the end of Description and the start of the next
		// field must still be NUL padding, not a leaked byte of Originator or
		// beyond: proof Description's copy respects its 256 byte width.
		for i := len(description); i < bextDescriptionWidth; i++ {
			if body[i] != 0 {
				t.Fatalf("byte %d, in the Description padding, is %#02x, want 0", i, body[i])
			}
		}
		// CodingHistory starts right after the fixed 602 byte body.
		if got := string(body[pcm.BextFixedSize:]); got != codingHistory {
			t.Fatalf("CodingHistory at offset %d: got %q want %q", pcm.BextFixedSize, got, codingHistory)
		}
	})
}
