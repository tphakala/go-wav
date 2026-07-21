package riff

import (
	"bufio"
	"bytes"
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
	const channels, bits uint16 = 1, 16
	const blockAlign = bits / 8 * channels
	return cat(
		le16(tagPCM),
		le16(channels),
		le32(rate),
		// The byte rate is derived from the declared rate and would overflow
		// its own field at these values. It is a redundant field the parser
		// does not read, so it is left at zero rather than made to lie
		// plausibly.
		le32(0),
		le16(blockAlign),
		le16(bits),
	)
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
		{"hi-res pcm", 768000, 768000},
		{"bat detector", 2_000_000, 2_000_000},
		{"software defined radio capture", 20_000_000, 20_000_000},
		{"at the ceiling", MaxSampleRate, int(MaxSampleRate)},
		{"zero rate", 0, 0},
		{"one past the ceiling", MaxSampleRate + 1, 0},
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
		MaxSampleRate - 1, MaxSampleRate, MaxSampleRate + 1, MaxSampleRate + 2,
		0xDEADBEEF, math.MaxUint32,
	}

	accepted := 0
	for _, rate := range rates {
		f, err := parseFmt(fmtPayloadRate(rate))
		if err != nil {
			continue
		}
		accepted++
		if f.SampleRate <= 0 {
			t.Errorf("a declared rate of %d parsed to SampleRate %d, want a positive rate or an error",
				rate, f.SampleRate)
		}
	}
	// Without this the test would be green if every payload were rejected for
	// some unrelated reason, which is how an invariant test quietly stops
	// testing its invariant.
	if accepted == 0 {
		t.Fatal("no declared rate parsed at all; fmtPayloadRate no longer builds a chunk parseFmt accepts")
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

// TestWriterRefusesWhatReaderRefuses pins the two ends of the ceiling
// together. A reader bound with no writer bound would let this package emit a
// header it cannot read back, which is the shape of the fact-chunk sentinel
// defect fixed earlier: the fmt chunk's byte-rate field bounds the rate on its
// own only when a frame is wider than one byte, so 8-bit mono was free to
// stamp a rate the reader would then reject.
func TestWriterRefusesWhatReaderRefuses(t *testing.T) {
	t.Parallel()

	// One byte per frame, so the derived byte rate equals the sample rate and
	// cannot overflow its own field before the rate overflows the ceiling.
	// This is the only geometry that reaches the gap.
	format := Format{Channels: 1, BitDepth: 8, Format: wav.SampleFormatPCM}

	for _, rate := range []int64{48000, int64(MaxSampleRate), int64(MaxSampleRate) + 1, math.MaxUint32} {
		// A rate past the ceiling is not expressible as an int at all on a
		// 32-bit target, so there it cannot even be asked for. The two
		// refusal cases therefore run only on a 64-bit target, and this
		// test's passing under GOARCH=386 is not evidence the bound exists.
		if int64(int(rate)) != rate {
			continue
		}
		format.SampleRate = int(rate)
		layout, err := BuildHeader(HeaderConfig{Format: format, Container: wav.ContainerRIFF})

		if rate > int64(MaxSampleRate) {
			if err == nil {
				t.Errorf("BuildHeader wrote a header declaring %d, a rate parseFmt refuses", rate)
			}
			continue
		}
		if err != nil {
			t.Fatalf("BuildHeader refused %d, a rate parseFmt accepts: %v", rate, err)
		}

		// Whatever the writer does emit must parse back to the rate asked for.
		stream := cat(layout.Bytes, make([]byte, 8))
		got, perr := ParseHeader(bufio.NewReader(bytes.NewReader(stream)))
		if perr != nil {
			t.Fatalf("rate %d: BuildHeader wrote a header ParseHeader refuses: %v", rate, perr)
		}
		if int64(got.Info.SampleRate) != rate {
			t.Errorf("rate %d round tripped as %d", rate, got.Info.SampleRate)
		}
	}
}
