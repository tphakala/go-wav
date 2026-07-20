package pcm

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// countingReader records how many times the decoder actually goes to the
// source, which is the syscall count on a real file or socket.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

// TestDecoderDoesNotAmplifySourceReads pins that a caller reading in blocks
// smaller than the internal window does not pay extra source reads for it.
//
// The header window is small, which keeps a Decoder cheap to open. Left at
// that, streaming would suffer: bufio only passes a request through untouched
// when it is at least the window size, so a small window refills once per read
// for every smaller block. The decoder layers a wide window over the small one
// for audio, so the source sees roughly one read per streaming buffer whatever
// block size the caller uses.
func TestDecoderDoesNotAmplifySourceReads(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	payload := make([]byte, 1<<20) // 1 MiB of audio
	for i := range payload {
		payload[i] = byte(i)
	}
	var file bytes.Buffer
	if err := EncodeInterleaved(&file, cfg, payload); err != nil {
		t.Fatal(err)
	}

	for _, chunk := range []int{512, 1024, 4096, 8192, 16384, 65536} {
		src := &countingReader{r: bytes.NewReader(file.Bytes())}
		d, err := NewDecoder(src)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, chunk)
		var got []byte
		for {
			n, rerr := d.Read(buf)
			got = append(got, buf[:n]...)
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					t.Fatalf("chunk %d: read: %v", chunk, rerr)
				}
				break
			}
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("chunk %d: payload mismatch (%d vs %d bytes)", chunk, len(got), len(payload))
		}
		callerReads := (len(payload) + chunk - 1) / chunk
		// The source should be read about once per streaming buffer, whatever
		// the caller's block size. A few extra cover header parsing and the
		// final short read.
		limit := len(payload)/streamBufferSize + 8
		t.Logf("chunk=%-6d caller reads=%-5d source reads=%-5d", chunk, callerReads, src.reads)
		if src.reads > limit {
			t.Errorf("chunk %d: %d source reads for %d caller reads (limit %d); "+
				"a small read block should not multiply the reads reaching the source",
				chunk, src.reads, callerReads, limit)
		}
	}
}
