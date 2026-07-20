package pcm_test

import (
	"bytes"
	"errors"
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
			d, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()), pcm.WithConvertTo(16))
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			p := make([]byte, size)
			for {
				n, rerr := d.Read(p)
				got = append(got, p[:n]...)
				if errors.Is(rerr, io.EOF) {
					break
				}
				if rerr != nil {
					t.Fatalf("Read with a %d byte buffer: %v", size, rerr)
				}
				if n == 0 {
					t.Fatalf("Read with a %d byte buffer made no progress", size)
				}
			}
			if len(got) != len(src)*2 {
				t.Errorf("got %d bytes, want %d", len(got), len(src)*2)
			}
			// Must equal what a single large read produces.
			d2, _ := pcm.NewDecoder(bytes.NewReader(buf.Bytes()), pcm.WithConvertTo(16))
			want, _ := io.ReadAll(d2)
			if !bytes.Equal(got, want) {
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
