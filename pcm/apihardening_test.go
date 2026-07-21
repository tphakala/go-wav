package pcm_test

import (
	"bytes"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// TestDecoderTinyReadsUnderConversion pins that a converting decoder honours
// the io.Reader contract for any buffer size, including one smaller than a
// single converted sample. Converting straight into the caller's buffer could
// not do that, so the pass-through and converting paths would have disagreed.
func TestDecoderTinyReadsUnderConversion(t *testing.T) {
	cfg := pcm.Config{SampleRate: 8000, BitDepth: 8, Channels: 1}
	src := []byte{128, 255, 0, 129, 127, 130}
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, src); err != nil {
		t.Fatal(err)
	}

	for _, size := range []int{1, 2, 3, 5, 7} {
		t.Run(string(rune('0'+size))+"_byte_reads", func(t *testing.T) {
			got := drainInto(t, buf.Bytes(), 16, size)
			if len(got) != len(src)*2 {
				t.Errorf("got %d bytes, want %d", len(got), len(src)*2)
			}
			// Must equal what a single large read produces.
			if want := readAllConverted(t, buf.Bytes(), 16); !bytes.Equal(got, want) {
				t.Errorf("tiny reads differ from one large read")
			}
		})
	}
}

// TestEncodeInterleavedEmptyRF64Always pins that a zero length one-shot works
// on a sink that cannot seek. The payload length is known exactly, so the
// header can be final immediately; only an undeclared length is unresolvable.
func TestEncodeInterleavedEmptyRF64Always(t *testing.T) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, RF64: pcm.RF64Always}
	var buf bytes.Buffer // a plain io.Writer, deliberately not seekable
	if err := pcm.EncodeInterleaved(&buf, cfg, nil); err != nil {
		t.Fatalf("empty one-shot RF64Always on a plain writer: %v", err)
	}
	if got := string(buf.Bytes()[0:4]); got != "RF64" {
		t.Errorf("magic: got %q want RF64", got)
	}
	d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Info().Container != wav.ContainerRF64 {
		t.Errorf("container: got %v", d.Info().Container)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes of audio, want 0", len(got))
	}
}

// TestEncoderNilLayoutNoPanic pins that a Reset rejected inside header building
// leaves an encoder that reports an error rather than dereferencing a nil
// layout.
func TestEncoderNilLayoutNoPanic(t *testing.T) {
	var good bytes.Buffer
	e, err := pcm.NewEncoder(&good, pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 65535 channels passes Config.validate but overflows nBlockAlign.
	if err := e.Reset(io.Discard, pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 65535}); err == nil {
		t.Fatal("Reset accepted a config whose block align cannot be expressed")
	}
	before := good.Len()
	if _, err := e.Write([]byte{1, 2, 3, 4}); err == nil {
		t.Error("Write succeeded on an encoder whose Reset failed")
	}
	if err := e.Close(); err == nil {
		t.Error("Close reported success on an encoder whose Reset failed")
	}
	if good.Len() != before {
		t.Errorf("the rejected encoder wrote %d bytes to the previous sink", good.Len()-before)
	}
}

// TestFailedResetInvalidatesTheDecoder pins that every way Reset can fail
// leaves the decoder unusable rather than still bound to the stream it held
// before.
//
// It matters most on the pooling path, where a caller legitimately holds a
// *Decoder across a Reset. Two of the three failure paths used to return
// before touching the decoder, so a caller who checked the error and moved on
// would find Info still describing the previous file and Read still handing
// back its audio, silently mixing two streams with nothing reporting it.
func TestFailedResetInvalidatesTheDecoder(t *testing.T) {
	t.Parallel()

	cfg := pcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 1}
	good := encodeFixture(t, cfg, pattern(64))

	cases := []struct {
		name  string
		reset func(d *pcm.Decoder) error
	}{
		{"nil reader", func(d *pcm.Decoder) error {
			return d.Reset(nil)
		}},
		{"unusable conversion width", func(d *pcm.Decoder) error {
			return d.Reset(bytes.NewReader(good), pcm.WithConvertTo(7))
		}},
		{"unparseable stream", func(d *pcm.Decoder) error {
			return d.Reset(bytes.NewReader([]byte("not a wav file at all")))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := pcm.NewDecoder(bytes.NewReader(good))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if got := d.Info().SampleRate; got != cfg.SampleRate {
				t.Fatalf("before Reset, SampleRate = %d, want %d", got, cfg.SampleRate)
			}

			if err := tc.reset(d); err == nil {
				t.Fatal("Reset reported no error")
			}

			if got := d.Info().SampleRate; got != 0 {
				t.Errorf("after a failed Reset, Info reports SampleRate %d, want 0: "+
					"the decoder still describes the stream it held before", got)
			}
			n, rerr := d.Read(make([]byte, 16))
			if rerr == nil {
				t.Errorf("after a failed Reset, Read returned %d bytes and no error: "+
					"the decoder is still serving the previous stream", n)
			}
			if n != 0 {
				t.Errorf("after a failed Reset, Read returned %d bytes, want 0", n)
			}
		})
	}
}
