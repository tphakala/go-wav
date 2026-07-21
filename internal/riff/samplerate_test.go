package riff

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// fmtPayloadRate builds a 16-byte PCM fmt payload with the sample rate stamped
// as a raw 32-bit field. It exists alongside fmtPayload16 because that helper
// takes the rate as an int, and the rates this file is about do not fit an int
// on a 32-bit target, which is the platform the defect was visible on.
func fmtPayloadRate(rate uint32) []byte {
	const channels, bits = 1, 16
	blockAlign := bits / 8 * channels
	b := make([]byte, fmtSizePCM)
	binary.LittleEndian.PutUint16(b[0:2], tagPCM)
	binary.LittleEndian.PutUint16(b[2:4], channels)
	binary.LittleEndian.PutUint32(b[4:8], rate)
	// The byte rate is derived from the declared rate and would overflow its
	// own field at these values. It is a redundant field the parser does not
	// read, so it is left at zero rather than made to lie plausibly.
	binary.LittleEndian.PutUint32(b[8:12], 0)
	binary.LittleEndian.PutUint16(b[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(b[14:16], bits)
	return b
}

// TestParseFmtBoundsSampleRate pins the ceiling on the declared sample rate.
//
// The field is a uint32 on the wire and was read straight into a native int.
// On a 32-bit target that conversion wraps: a declared 0x80000000 became
// -2147483648 and 0xFFFFFFFF became -1, and both reached the exported
// StreamInfo.SampleRate with nothing marking them as untrustworthy.
func TestParseFmtBoundsSampleRate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rate uint32
		want int // 0 means the chunk must be rejected
	}{
		{"telephony", 8000, 8000},
		{"cd", 44100, 44100},
		{"studio", 48000, 48000},
		{"ultrasonic", 384000, 384000},
		{"highest rate any recorder uses", 768000, 768000},
		{"software defined radio capture", 20_000_000, 20_000_000},
		{"at the ceiling", math.MaxInt32, math.MaxInt32},
		{"zero rate", 0, 0},
		{"one past the ceiling", 1 << 31, 0},
		{"all ones", math.MaxUint32, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := parseFmt(fmtPayloadRate(tc.rate))
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("parseFmt accepted a declared rate of %d, giving SampleRate %d",
						tc.rate, f.SampleRate)
				}
				if !errors.Is(err, wav.ErrCorruptStream) {
					t.Errorf("parseFmt error is %v, want one wrapping wav.ErrCorruptStream", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFmt rejected a declared rate of %d: %v", tc.rate, err)
			}
			if f.SampleRate != tc.want {
				t.Errorf("SampleRate = %d, want %d", f.SampleRate, tc.want)
			}
		})
	}
}

// TestParsedSampleRateIsAlwaysPositive is the invariant the ceiling exists to
// give callers: whatever a header declares, a Format that parsed carries a
// sample rate a caller can divide by. A negative one is what the wrap
// produced, and it is worse than a rejection because nothing reports it.
func TestParsedSampleRateIsAlwaysPositive(t *testing.T) {
	t.Parallel()

	rates := []uint32{
		0, 1, 8000, 48000, 384000, 1 << 24,
		math.MaxInt32 - 1, math.MaxInt32, 1 << 31, 1<<31 + 1,
		0xDEADBEEF, math.MaxUint32,
	}

	for _, rate := range rates {
		f, err := parseFmt(fmtPayloadRate(rate))
		if err != nil {
			continue
		}
		if f.SampleRate <= 0 {
			t.Errorf("a declared rate of %d parsed to SampleRate %d, want a positive rate or an error",
				rate, f.SampleRate)
		}
	}
}

// TestHeaderRejectsUnbelievableSampleRate covers the same bound through the
// parser a caller actually reaches, so the refusal is known to happen before
// any StreamInfo is built rather than only in the helper.
func TestHeaderRejectsUnbelievableSampleRate(t *testing.T) {
	t.Parallel()

	stream := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, fmtPayloadRate(math.MaxUint32)),
		chunk(idData, make([]byte, 8)),
	)

	_, err := ParseHeader(bufio.NewReader(bytes.NewReader(stream)))
	if err == nil {
		t.Fatal("ParseHeader accepted a stream declaring a sample rate of 4294967295")
	}
	if !errors.Is(err, wav.ErrCorruptStream) {
		t.Errorf("ParseHeader error is %v, want one wrapping wav.ErrCorruptStream", err)
	}
}
