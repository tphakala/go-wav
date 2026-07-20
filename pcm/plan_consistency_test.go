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
			// What checkCapacity will enforce.
			layLen := e.lay.DataOffset
			// What plan consulted, when it consulted anything.
			planLen := riff.HeaderLen(riff.HeaderConfig{
				Format:    formatOf(tc.cfg),
				Container: wav.ContainerRIFF,
			})
			framesKnown := tc.cfg.TotalFrames > 0
			reserved := e.lay.DS64Offset >= 0 && !e.cont.Sized64()

			t.Logf("layout header=%d, plan probe=%d, framesKnown=%v, reservedJUNK=%v, container=%v",
				layLen, planLen, framesKnown, reserved, e.cont)

			// The only path where plan's probe feeds the decision is the
			// framesKnown one, and there the layout must not reserve ds64
			// space, or the two lengths differ by exactly the reservation.
			if framesKnown && reserved {
				t.Errorf("plan sized the decision without a ds64 reservation (%d bytes) "+
					"but the layout reserved one (%d bytes): the RF64 decision and the "+
					"capacity limit disagree by %d bytes", planLen, layLen, layLen-planLen)
			}
			if framesKnown && !reserved && !e.cont.Sized64() && layLen != planLen {
				t.Errorf("header length %d != plan probe %d", layLen, planLen)
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
