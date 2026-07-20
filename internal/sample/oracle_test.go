package sample

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// The helpers here are an independent reading and writing of the on-disk
// sample formats, kept deliberately separate from the block loops the package
// ships. Tests compare the two, so a mistake in decodeBlock or encodeBlock
// shows up as a disagreement rather than being confirmed by its own logic.
//
// They were the production implementation before the conversion loops were
// blocked. Keeping them as an oracle is the point: a test that decodes with the
// same code it is testing cannot fail.

// decodeIntRef reads one little-endian integer PCM sample of the given width
// and returns it as a signed value. The 8-bit case is the odd one out: it is
// stored biased by 128, so the bias is removed here.
func decodeIntRef(b []byte, bits int) int64 {
	switch bits {
	case 8:
		return int64(b[0]) - 128
	case 16:
		return int64(int16(binary.LittleEndian.Uint16(b)))
	case 24:
		u := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if u&0x800000 != 0 {
			u |= 0xFF000000 // sign-extend bit 23 into the unused high byte
		}
		return int64(int32(u))
	default: // 32
		return int64(int32(binary.LittleEndian.Uint32(b)))
	}
}

// encodeIntRef writes one signed sample as little-endian integer PCM of the
// given width. v must already be in range for bits. The 8-bit case re-applies
// the 128 bias.
func encodeIntRef(b []byte, v int64, bits int) {
	switch bits {
	case 8:
		b[0] = byte(v + 128)
	case 16:
		binary.LittleEndian.PutUint16(b, uint16(v)) //nolint:gosec // in int16 range by construction.
	case 24:
		u := uint32(v) //nolint:gosec // in 24-bit signed range by construction.
		b[0] = byte(u)
		b[1] = byte(u >> 8)
		b[2] = byte(u >> 16) // packed, three bytes, no padding
	default: // 32
		binary.LittleEndian.PutUint32(b, uint32(v)) //nolint:gosec // in int32 range by construction.
	}
}

// TestBlockHelpersMatchOracle checks the shipped decodeBlock and encodeBlock
// against the independent reference above, across every supported width and
// the full value range of each.
//
// This is what pins the on-disk format rules to the code that actually runs.
// The 24-bit sign extension and the 8-bit bias are each written twice in this
// package now, once in the block loops and once in the oracle, and this test is
// what stops the two drifting apart.
func TestBlockHelpersMatchOracle(t *testing.T) {
	for _, bits := range []int{8, 16, 24, 32} {
		t.Run(fmt.Sprintf("s%d", bits), func(t *testing.T) {
			assertBlockHelpersMatchOracle(t, bits)
		})
	}
}

// assertBlockHelpersMatchOracle is the per-width body of the test above.
func assertBlockHelpersMatchOracle(t *testing.T, bits int) {
	t.Helper()
	width := bits / 8
	lo, hi := int64(-1)<<(bits-1), int64(1)<<(bits-1)-1

	// Every value at 8 bits, and a spread that includes both extremes
	// and every sign-bit boundary at the wider depths.
	var vals []int64
	if bits == 8 {
		for v := lo; v <= hi; v++ {
			vals = append(vals, v)
		}
	} else {
		// Only values the width can actually hold: encodeBlock's
		// contract is that the caller has already shifted or clamped
		// into range, so feeding it more would test nothing real.
		for _, v := range []int64{lo, lo + 1, -65536, -32768, -256, -128,
			-2, -1, 0, 1, 2, 127, 128, 255, 256, 32767, 65535, hi - 1, hi} {
			if v >= lo && v <= hi {
				vals = append(vals, v)
			}
		}
	}

	// encodeBlock must produce exactly what the oracle produces.
	in := make([]int32, len(vals))
	for i, v := range vals {
		in[i] = int32(v)
	}
	got := make([]byte, len(vals)*width)
	encodeBlock(got, in, bits)

	want := make([]byte, len(vals)*width)
	for i, v := range vals {
		encodeIntRef(want[i*width:], v, bits)
	}
	if !bytes.Equal(got, want) {
		for i := range vals {
			g, w := got[i*width:(i+1)*width], want[i*width:(i+1)*width]
			if !bytes.Equal(g, w) {
				t.Fatalf("encodeBlock(%d) = % x, oracle = % x", vals[i], g, w)
			}
		}
	}

	// decodeBlock must read back exactly what the oracle reads.
	out := make([]int32, len(vals))
	decodeBlock(out, want, bits)
	for i, v := range vals {
		if int64(out[i]) != v {
			t.Errorf("decodeBlock round trip of %d gave %d", v, out[i])
		}
		if ref := decodeIntRef(want[i*width:], bits); int64(out[i]) != ref {
			t.Errorf("decodeBlock(% x) = %d, oracle = %d", want[i*width:(i+1)*width], out[i], ref)
		}
	}
}
