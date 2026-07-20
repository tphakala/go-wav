package pcm_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// These tests validate go-wav against ffmpeg, the reference implementation most
// of the world actually uses. They are skipped when ffmpeg is absent, mirroring
// how go-flac cross-validates against libFLAC.
//
// Two directions matter and both are checked: files ffmpeg writes must decode
// here to exactly the samples ffmpeg itself extracts, and files written here
// must decode in ffmpeg to exactly the samples that went in. A format library
// that only agrees with itself has proved nothing.

func lookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed, skipping cross-validation", name)
	}
	return p
}

func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// codecFor names the ffmpeg pcm codec matching a configuration.
func codecFor(cfg pcm.Config) string {
	if cfg.Format == wav.SampleFormatFloat {
		return "pcm_f" + strconv.Itoa(cfg.BitDepth) + "le"
	}
	if cfg.BitDepth == 8 {
		return "pcm_u8"
	}
	return "pcm_s" + strconv.Itoa(cfg.BitDepth) + "le"
}

var crossCases = []struct {
	name string
	cfg  pcm.Config
}{
	{"s16_mono_48k", pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}},
	{"s16_stereo_44k", pcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2}},
	{"s24_stereo_96k", pcm.Config{SampleRate: 96000, BitDepth: 24, Channels: 2}},
	{"s32_6ch_48k", pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 6}},
	{"u8_mono_8k", pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}},
	{"f32_stereo_44k", pcm.Config{SampleRate: 44100, BitDepth: 32, Channels: 2,
		Format: wav.SampleFormatFloat}},
	{"f64_mono_48k", pcm.Config{SampleRate: 48000, BitDepth: 64, Channels: 1,
		Format: wav.SampleFormatFloat}},
	// The rate that matters most for bat and ultrasonic capture, and the one
	// no other native encoder in the family handles.
	{"s16_mono_384k", pcm.Config{SampleRate: 384000, BitDepth: 16, Channels: 1}},
}

// TestDecodeFFmpegOutput checks that a file ffmpeg wrote decodes here to the
// same samples ffmpeg extracts from it.
func TestDecodeFFmpegOutput(t *testing.T) {
	ffmpeg := lookPath(t, "ffmpeg")
	dir := t.TempDir()

	for _, tc := range crossCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".wav")
			codec := codecFor(tc.cfg)

			run(t, ffmpeg, "-v", "error", "-y",
				"-f", "lavfi", "-i",
				"sine=frequency=997:sample_rate="+strconv.Itoa(tc.cfg.SampleRate)+":duration=0.1",
				"-ac", strconv.Itoa(tc.cfg.Channels),
				"-c:a", codec, path)

			// What ffmpeg itself considers the sample payload.
			raw := strings.TrimPrefix(codec, "pcm_")
			want := run(t, ffmpeg, "-v", "error", "-i", path, "-f", raw, "-c:a", codec, "-")

			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			d, err := pcm.NewDecoder(f)
			if err != nil {
				t.Fatalf("NewDecoder on ffmpeg output: %v", err)
			}
			info := d.Info()
			if info.SampleRate != tc.cfg.SampleRate {
				t.Errorf("sample rate: got %d want %d", info.SampleRate, tc.cfg.SampleRate)
			}
			if info.Channels != tc.cfg.Channels {
				t.Errorf("channels: got %d want %d", info.Channels, tc.cfg.Channels)
			}
			if info.BitDepth != tc.cfg.BitDepth {
				t.Errorf("bit depth: got %d want %d", info.BitDepth, tc.cfg.BitDepth)
			}
			if info.Format != tc.cfg.Format {
				t.Errorf("format: got %v want %v", info.Format, tc.cfg.Format)
			}

			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("sample payload differs from ffmpeg: got %d bytes, ffmpeg %d bytes",
					len(got), len(want))
			}
		})
	}
}

// TestFFmpegDecodesOurOutput checks that ffmpeg reads what this package writes,
// and recovers exactly the samples that went in.
func TestFFmpegDecodesOurOutput(t *testing.T) {
	ffmpeg := lookPath(t, "ffmpeg")
	dir := t.TempDir()

	for _, tc := range crossCases {
		t.Run(tc.name, func(t *testing.T) {
			frames := tc.cfg.SampleRate / 20
			width := (tc.cfg.BitDepth + 7) / 8
			src := make([]byte, frames*tc.cfg.Channels*width)
			// A deterministic non-trivial pattern. The exact values do not
			// matter; that every byte survives the round trip does.
			for i := range src {
				src[i] = byte(i*31 + 7)
			}
			// Float payloads must be finite, so a byte pattern will not do.
			if tc.cfg.Format == wav.SampleFormatFloat {
				src = floatPattern(frames*tc.cfg.Channels, tc.cfg.BitDepth)
			}

			path := filepath.Join(dir, tc.name+".wav")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := pcm.EncodeInterleaved(f, tc.cfg, src); err != nil {
				f.Close()
				t.Fatalf("encode: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			codec := codecFor(tc.cfg)
			raw := strings.TrimPrefix(codec, "pcm_")
			got := run(t, ffmpeg, "-v", "error", "-i", path, "-f", raw, "-c:a", codec, "-")

			if !bytes.Equal(got, src) {
				t.Errorf("ffmpeg recovered %d bytes, wrote %d; payload differs",
					len(got), len(src))
			}
		})
	}
}

// TestDecodeFFmpegRF64 checks the RF64 reading path against a real RF64 file
// from ffmpeg, rather than only against files this package wrote.
func TestDecodeFFmpegRF64(t *testing.T) {
	ffmpeg := lookPath(t, "ffmpeg")
	dir := t.TempDir()
	path := filepath.Join(dir, "rf64.wav")

	run(t, ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=0.25",
		"-c:a", "pcm_s16le", "-rf64", "always", path)

	head, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(head[0:4]) != "RF64" {
		t.Fatalf("ffmpeg did not produce RF64, magic is %q", head[0:4])
	}
	if !wav.Sniff(head) {
		t.Error("Sniff rejected an ffmpeg RF64 file")
	}

	d, err := pcm.NewDecoder(bytes.NewReader(head))
	if err != nil {
		t.Fatalf("NewDecoder on ffmpeg RF64: %v", err)
	}
	info := d.Info()
	if info.Container != wav.ContainerRF64 {
		t.Errorf("container: got %v want RF64", info.Container)
	}
	if info.SampleRate != 48000 || info.Channels != 1 || info.BitDepth != 16 {
		t.Errorf("unexpected stream info: %+v", info)
	}
	if info.TotalFrames != 12000 {
		t.Errorf("frames: got %d want 12000", info.TotalFrames)
	}

	want := run(t, ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-c:a", "pcm_s16le", "-")
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RF64 payload differs from ffmpeg: got %d bytes, want %d", len(got), len(want))
	}
}

// TestFFmpegDecodesOurRF64 checks that ffmpeg accepts the RF64 streams this
// package writes, in both the forced and the upgraded-from-JUNK forms.
func TestFFmpegDecodesOurRF64(t *testing.T) {
	ffmpeg := lookPath(t, "ffmpeg")
	dir := t.TempDir()
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, RF64: pcm.RF64Always}

	frames := 4800
	src := make([]byte, frames*2)
	for i := range src {
		src[i] = byte(i * 13)
	}

	path := filepath.Join(dir, "ours_rf64.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pcm.EncodeInterleaved(f, cfg, src); err != nil {
		f.Close()
		t.Fatalf("encode: %v", err)
	}
	f.Close()

	head, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(head[0:4]) != "RF64" {
		t.Fatalf("expected RF64 magic, got %q", head[0:4])
	}

	got := run(t, ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-c:a", "pcm_s16le", "-")
	if !bytes.Equal(got, src) {
		t.Errorf("ffmpeg recovered %d bytes from our RF64, wrote %d", len(got), len(src))
	}
}

// TestSoxReadsOurOutput adds a second independent reader, since agreeing with
// one implementation is weaker evidence than agreeing with two.
func TestSoxReadsOurOutput(t *testing.T) {
	sox := lookPath(t, "sox")
	dir := t.TempDir()

	for _, tc := range crossCases {
		t.Run(tc.name, func(t *testing.T) {
			frames := 1000
			width := (tc.cfg.BitDepth + 7) / 8
			n := frames * tc.cfg.Channels * width
			src := make([]byte, n)
			if tc.cfg.Format == wav.SampleFormatFloat {
				src = floatPattern(frames*tc.cfg.Channels, tc.cfg.BitDepth)
			}

			path := filepath.Join(dir, tc.name+".wav")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := pcm.EncodeInterleaved(f, tc.cfg, src); err != nil {
				f.Close()
				t.Fatalf("encode: %v", err)
			}
			f.Close()

			// sox writes any parse complaint to stderr, so a clean run with
			// the right answers is the assertion.
			cmd := exec.Command(sox, "--i", path)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("sox --i: %v\n%s", err, stderr.String())
			}
			if warn := stderr.String(); strings.Contains(warn, "WARN") {
				t.Errorf("sox warned about our file: %s", strings.TrimSpace(warn))
			}
			info := string(out)
			if !strings.Contains(info, strconv.Itoa(tc.cfg.SampleRate)) {
				t.Errorf("sox did not report sample rate %d:\n%s", tc.cfg.SampleRate, info)
			}
		})
	}
}

// floatPattern builds a deterministic buffer of finite float samples inside
// nominal full scale, so that no clamping is involved and the round trip is
// exact.
func floatPattern(samples, bits int) []byte {
	width := bits / 8
	buf := make([]byte, samples*width)
	for i := range samples {
		v := float64(i%201-100) / 100.0
		if bits == 32 {
			binary.LittleEndian.PutUint32(buf[i*width:], math.Float32bits(float32(v)))
		} else {
			binary.LittleEndian.PutUint64(buf[i*width:], math.Float64bits(v))
		}
	}
	return buf
}
