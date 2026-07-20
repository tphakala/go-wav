package pcm

import (
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/riff"
)

// TestPlanAndCheckCapacityAgreeOnHeaderLength pins the two places the encoder
// measures a header against each other.
//
// plan decides up front whether a stream needs RF64 by asking fitsPlainRIFF,
// which sizes a header that never reserves ds64 space. checkCapacity then
// enforces the limit at run time against the layout actually emitted, which may
// reserve 36 bytes for a ds64 that a later upgrade would fill in. If those two
// lengths could disagree the encoder would choose a container on one basis and
// police it on another, and a stream could pass the up-front check yet fail the
// running one.
//
// They cannot disagree, and the reason is worth pinning rather than
// rediscovering: the probe only feeds the decision when the frame count is
// known, and that is exactly the path where plan returns no reservation.
func TestPlanAndCheckCapacityAgreeOnHeaderLength(t *testing.T) {
	seekable := &memSeekerC{}

	cases := []struct {
		name     string
		cfg      Config
		seekable bool
	}{
		{"auto seekable no frames", Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, true},
		{"auto plain no frames", Config{SampleRate: 48000, BitDepth: 16, Channels: 1}, false},
		{"auto with frames", Config{SampleRate: 48000, BitDepth: 16, Channels: 1, TotalFrames: 1000}, true},
		{"auto float 6ch frames", Config{SampleRate: 96000, BitDepth: 32, Channels: 6,
			Format: wav.SampleFormatFloat, TotalFrames: 1000}, false},
		{"never", Config{SampleRate: 48000, BitDepth: 24, Channels: 2, RF64: RF64Never}, true},
		{"always seekable", Config{SampleRate: 48000, BitDepth: 16, Channels: 1, RF64: RF64Always}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w io.Writer
			if tc.seekable {
				w = seekable
			} else {
				w = io.Discard
			}
			e, err := NewEncoder(w, tc.cfg)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			// Assert against the encoder's OWN layout rather than recomputing
			// the probe here, which would only prove the test can copy the
			// implementation. Every case asserts; the invariant holds whether
			// or not the frame count was declared.
			layLen := e.lay.DataOffset
			declared := int64(tc.cfg.TotalFrames) * tc.cfg.bytesPerFrame()

			// The container the encoder chose must match what its own layout
			// says about the declared size. A stream that fits plain RIFF must
			// not have been promoted, and one that cannot fit must have been.
			fits := riff.FitsRIFF(e.lay, declared)
			if tc.cfg.TotalFrames > 0 && tc.cfg.RF64 == RF64Auto {
				if fits && e.cont != wav.ContainerRIFF {
					t.Errorf("declared %d bytes fits the emitted %d byte header, "+
						"yet the encoder chose %v", declared, layLen, e.cont)
				}
				if !fits && e.cont != wav.ContainerRF64 {
					t.Errorf("declared %d bytes does not fit the emitted %d byte header, "+
						"yet the encoder chose %v", declared, layLen, e.cont)
				}
			}

			// A reserved ds64 makes the emitted header longer than the probe
			// plan consults. That is only sound where plan does not consult
			// the probe, which is the undeclared-length path.
			reserved := e.lay.DS64Offset >= 0 && !e.cont.Sized64()
			if reserved && tc.cfg.TotalFrames > 0 {
				t.Errorf("plan sized its decision without a ds64 reservation but the "+
					"layout reserved one: the RF64 decision and the capacity limit "+
					"would disagree by %d bytes", riff.DS64ChunkSize)
			}

			// checkCapacity enforces against this layout, so it must describe a
			// header the encoder actually wrote.
			if layLen != int64(len(e.lay.Bytes)) {
				t.Errorf("layout claims a %d byte header but emitted %d bytes",
					layLen, len(e.lay.Bytes))
			}
		})
	}
}

type memSeekerC struct {
	b   []byte
	pos int64
}

func (m *memSeekerC) Write(p []byte) (int, error) {
	need := m.pos + int64(len(p))
	if int64(len(m.b)) < need {
		nb := make([]byte, need)
		copy(nb, m.b)
		m.b = nb
	}
	copy(m.b[m.pos:], p)
	m.pos = need
	return len(p), nil
}
func (m *memSeekerC) Seek(off int64, whence int) (int64, error) {
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
