package pcm_test

import (
	"bytes"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

func TestSmokeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cfg  pcm.Config
	}{
		{"s16 mono", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}},
		{"s24 stereo", pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 2}},
		{"s32 6ch", pcm.Config{SampleRate: 384000, BitDepth: 32, Channels: 6}},
		{"u8 mono", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}},
		{"f32 stereo", pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2, Format: wav.SampleFormatFloat}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := 100
			n := frames * tc.cfg.Channels * ((tc.cfg.BitDepth + 7) / 8)
			src := make([]byte, n)
			for i := range src {
				src[i] = byte(i * 7)
			}
			var buf bytes.Buffer
			if err := pcm.EncodeInterleaved(&buf, tc.cfg, src); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !wav.Sniff(buf.Bytes()) {
				t.Fatalf("Sniff rejected our own output")
			}
			d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			info := d.Info()
			if info.SampleRate != tc.cfg.SampleRate || info.Channels != tc.cfg.Channels || info.BitDepth != tc.cfg.BitDepth {
				t.Errorf("info mismatch: got %+v", info)
			}
			if info.Format != tc.cfg.Format {
				t.Errorf("format: got %v want %v", info.Format, tc.cfg.Format)
			}
			if info.TotalFrames != uint64(frames) {
				t.Errorf("frames: got %d want %d", info.TotalFrames, frames)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, src) {
				t.Errorf("payload mismatch: got %d bytes want %d", len(got), len(src))
			}
		})
	}
}

func TestSmokeStreamingSeekable(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	f := &memSeeker{}
	e, err := pcm.NewEncoder(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	src := bytes.Repeat([]byte{1, 2}, 500)
	// write in awkward chunks to exercise the carry path
	for i := 0; i < len(src); i += 7 {
		end := min(i+7, len(src))
		if _, err := e.Write(src[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := pcm.NewDecoder(bytes.NewReader(f.b))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("streaming payload mismatch: %d vs %d", len(got), len(src))
	}
	if d.Info().Container != wav.ContainerRIFF {
		t.Errorf("expected plain RIFF, got %v", d.Info().Container)
	}
}

func TestSmokeConvert(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 8, Channels: 1}
	var buf bytes.Buffer
	// 8-bit unsigned: 128 is silence
	if err := pcm.EncodeInterleaved(&buf, cfg, []byte{128, 255, 0, 128}); err != nil {
		t.Fatal(err)
	}
	d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()), pcm.WithConvertTo(16))
	if err != nil {
		t.Fatal(err)
	}
	if d.Info().BitDepth != 16 {
		t.Errorf("Info must report converted width, got %d", d.Info().BitDepth)
	}
	if d.Info().SourceBitDepth != 8 {
		t.Errorf("SourceBitDepth got %d want 8", d.Info().SourceBitDepth)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("want 8 bytes of s16, got %d", len(got))
	}
	t.Logf("converted bytes: %v", got)
}

type memSeeker struct {
	b   []byte
	pos int64
}

func (m *memSeeker) Write(p []byte) (int, error) {
	need := m.pos + int64(len(p))
	if int64(cap(m.b)) < need {
		nb := make([]byte, need)
		copy(nb, m.b)
		m.b = nb
	} else if int64(len(m.b)) < need {
		m.b = m.b[:need]
	}
	copy(m.b[m.pos:], p)
	m.pos = need
	return len(p), nil
}

func (m *memSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.b)) + off
	}
	return m.pos, nil
}
