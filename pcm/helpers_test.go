package pcm_test

import (
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"slices"
	"testing"
)

// headKeep is how many leading bytes the counting sinks retain. It must cover
// the largest header this package emits, with room to spare.
const headKeep = 512

// chunkSpan records where a chunk payload begins and what its size field says.
type chunkSpan struct {
	payload int
	size    int
}

// walkChunks indexes the chunks of a WAVE stream up to and including data. The
// data payload is not walked, since its size field is a sentinel under RF64.
func walkChunks(tb testing.TB, b []byte) map[string]chunkSpan {
	tb.Helper()
	if len(b) < 12 {
		tb.Fatalf("stream is %d bytes, shorter than a 12 byte file header", len(b))
	}
	spans := make(map[string]chunkSpan)
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		payload := off + 8
		if _, seen := spans[id]; !seen {
			spans[id] = chunkSpan{payload: payload, size: size}
		}
		if id == "data" {
			break
		}
		adv := size
		if adv%2 != 0 {
			adv++
		}
		off = payload + adv
	}
	return spans
}

// requireChunk returns the span of a chunk that must be present.
func requireChunk(tb testing.TB, b []byte, id string) chunkSpan {
	tb.Helper()
	spans := walkChunks(tb, b)
	span, ok := spans[id]
	if !ok {
		tb.Fatalf("stream has no %q chunk; chunks found: %v", id, chunkIDs(spans))
	}
	return span
}

// chunkIDs lists the identifiers of an indexed stream, for error messages.
func chunkIDs(spans map[string]chunkSpan) []string {
	return slices.Collect(maps.Keys(spans))
}

// magic is the four-character container identifier of a stream.
func magic(tb testing.TB, b []byte) string {
	tb.Helper()
	if len(b) < 4 {
		tb.Fatalf("stream is %d bytes, too short for a magic", len(b))
	}
	return string(b[0:4])
}

// u32At reads a little-endian 32-bit field.
func u32At(tb testing.TB, b []byte, off int) uint32 {
	tb.Helper()
	if off+4 > len(b) {
		tb.Fatalf("offset %d is past the end of a %d byte stream", off, len(b))
	}
	return binary.LittleEndian.Uint32(b[off : off+4])
}

// u64At reads a little-endian 64-bit field.
func u64At(tb testing.TB, b []byte, off int) uint64 {
	tb.Helper()
	if off+8 > len(b) {
		tb.Fatalf("offset %d is past the end of a %d byte stream", off, len(b))
	}
	return binary.LittleEndian.Uint64(b[off : off+8])
}

// pattern builds a deterministic byte payload.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// failWriter accepts limit bytes and then reports err for ever after. It is how
// the tests reach the encoder's error latching path.
type failWriter struct {
	limit int64
	n     int64
	err   error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n >= f.limit {
		return 0, f.err
	}
	room := f.limit - f.n
	if int64(len(p)) <= room {
		f.n += int64(len(p))
		return len(p), nil
	}
	f.n = f.limit
	return int(room), f.err
}

// countingSink is a seekable sink that counts every byte and stores only the
// leading header region, so a stream past 4 GiB can be produced without holding
// it in memory. A rewrite at offset zero overwrites the stored head, which is
// what makes the ds64 upgrade observable.
type countingSink struct {
	head [headKeep]byte
	pos  int64
	size int64
}

func (c *countingSink) Write(p []byte) (int, error) {
	if c.pos < headKeep {
		copy(c.head[int(c.pos):], p)
	}
	c.pos += int64(len(p))
	if c.pos > c.size {
		c.size = c.pos
	}
	return len(p), nil
}

func (c *countingSink) Seek(off int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = off
	case io.SeekCurrent:
		next = c.pos + off
	case io.SeekEnd:
		next = c.size + off
	default:
		return 0, errors.New("countingSink: bad whence")
	}
	if next < 0 {
		return 0, errors.New("countingSink: negative position")
	}
	c.pos = next
	return c.pos, nil
}

// bytes returns the stored head, trimmed to what was actually written.
func (c *countingSink) bytes() []byte {
	n := c.size
	if n > headKeep {
		n = headKeep
	}
	return c.head[:n]
}

// countingWriter is the non-seekable counterpart of countingSink.
type countingWriter struct {
	head []byte
	n    int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if len(c.head) < headKeep {
		room := headKeep - len(c.head)
		c.head = append(c.head, p[:min(room, len(p))]...)
	}
	c.n += int64(len(p))
	return len(p), nil
}

// nonSeekReader hides an io.Seeker implementation from the decoder.
type nonSeekReader struct{ r io.Reader }

func (n nonSeekReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// nonSeekWriter hides an io.Seeker implementation from the encoder.
type nonSeekWriter struct{ w io.Writer }

func (n nonSeekWriter) Write(p []byte) (int, error) { return n.w.Write(p) }
