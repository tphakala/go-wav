package pcm_test

import (
	"bytes"
	"io"
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
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, pattern(64)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}, pattern(120)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2,
		Format: wav.SampleFormatFloat}, floatPattern(64, 32)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}, pattern(7)))
	f.Add(encodeFixture(f, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1,
		RF64: pcm.RF64Always}, pattern(64)))

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
				continue
			}
			info := d.Info()
			if info.Channels <= 0 {
				t.Fatalf("a decoder was built for a stream with %d channels", info.Channels)
			}
			_ = info.Duration()
			_ = info.BytesPerFrame()

			// Draining must terminate and must not panic.
			if _, err := io.Copy(io.Discard, d); err != nil {
				continue
			}
		}

		// Sniff must never panic on a short or hostile slice either.
		_ = wav.Sniff(data)
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
