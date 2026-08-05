package pcm

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestBextFixedBodyIs602Bytes pins the fixed part of the bext body to the
// width EBU Tech 3285 defines: everything up to CodingHistory.
func TestBextFixedBodyIs602Bytes(t *testing.T) {
	if bextFixedSize != 602 {
		t.Fatalf("bextFixedSize = %d, want 602", bextFixedSize)
	}
	b := &Bext{}
	body := b.serialize()
	if len(body) != bextFixedSize {
		t.Fatalf("serialize() with no CodingHistory = %d bytes, want %d", len(body), bextFixedSize)
	}
}

// TestBextFieldOffsets checks that each field lands at the byte offset the
// bext chunk layout requires, and that everything this package has no source
// for (UMID, the loudness fields, Reserved) is written zero.
func TestBextFieldOffsets(t *testing.T) {
	b := &Bext{
		Description:         "desc",
		Originator:          "orig",
		OriginatorReference: "origref",
		OriginationDate:     "2026-08-05",
		OriginationTime:     "12:34:56",
		TimeReference:       0x0102030405060708,
		Version:             1,
		CodingHistory:       "A=PCM,F=48000,W=16",
	}
	body := b.serialize()
	if want := bextFixedSize + len(b.CodingHistory); len(body) != want {
		t.Fatalf("serialize() = %d bytes, want %d", len(body), want)
	}

	checkField := func(name string, off, width int, want string) {
		t.Helper()
		got := string(bytes.TrimRight(body[off:off+width], "\x00"))
		if got != want {
			t.Errorf("%s at offset %d width %d: got %q, want %q", name, off, width, got, want)
		}
	}
	checkField("Description", 0, bextDescriptionSize, b.Description)
	checkField("Originator", bextDescriptionSize, bextOriginatorSize, b.Originator)
	checkField("OriginatorReference", bextDescriptionSize+bextOriginatorSize, bextOriginatorReferenceSize,
		b.OriginatorReference)
	checkField("OriginationDate", bextDescriptionSize+bextOriginatorSize+bextOriginatorReferenceSize,
		bextOriginationDateSize, b.OriginationDate)
	checkField("OriginationTime",
		bextDescriptionSize+bextOriginatorSize+bextOriginatorReferenceSize+bextOriginationDateSize,
		bextOriginationTimeSize, b.OriginationTime)

	trOff := bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize +
		bextOriginationDateSize + bextOriginationTimeSize
	gotLow := binary.LittleEndian.Uint32(body[trOff : trOff+4])
	gotHigh := binary.LittleEndian.Uint32(body[trOff+4 : trOff+8])
	//nolint:gosec // G115: test fixture, truncation is exactly what is under test.
	if wantLow := uint32(b.TimeReference); gotLow != wantLow {
		t.Errorf("TimeReferenceLow = %#x, want %#x", gotLow, wantLow)
	}
	//nolint:gosec // G115: test fixture, truncation is exactly what is under test.
	if wantHigh := uint32(b.TimeReference >> 32); gotHigh != wantHigh {
		t.Errorf("TimeReferenceHigh = %#x, want %#x", gotHigh, wantHigh)
	}

	verOff := trOff + bextTimeReferenceSize
	if gotVersion := binary.LittleEndian.Uint16(body[verOff : verOff+2]); gotVersion != b.Version {
		t.Errorf("Version = %d, want %d", gotVersion, b.Version)
	}

	// UMID, the five loudness fields and Reserved run from here to the end
	// of the fixed body, and this package has no source for any of them.
	zeroOff := verOff + bextVersionSize
	if !allZero(body[zeroOff:bextFixedSize]) {
		t.Errorf("bytes %d:%d (UMID, loudness fields, Reserved) are not all zero", zeroOff, bextFixedSize)
	}

	if got := string(body[bextFixedSize:]); got != b.CodingHistory {
		t.Errorf("CodingHistory suffix = %q, want %q", got, b.CodingHistory)
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestBextValidateWidths checks the max width enforced on each fixed text
// field: a value at the limit is accepted, one byte over is rejected.
func TestBextValidateWidths(t *testing.T) {
	cases := []struct {
		name  string
		build func(s string) *Bext
		width int
	}{
		{"Description", func(s string) *Bext { return &Bext{Description: s} }, bextDescriptionSize},
		{"Originator", func(s string) *Bext { return &Bext{Originator: s} }, bextOriginatorSize},
		{"OriginatorReference", func(s string) *Bext { return &Bext{OriginatorReference: s} },
			bextOriginatorReferenceSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok := tc.build(strings.Repeat("a", tc.width))
			if err := ok.validate("test"); err != nil {
				t.Errorf("a %d byte %s was rejected: %v", tc.width, tc.name, err)
			}
			bad := tc.build(strings.Repeat("a", tc.width+1))
			if err := bad.validate("test"); err == nil {
				t.Errorf("a %d byte %s, one over the limit, was accepted", tc.width+1, tc.name)
			}
		})
	}
}

// TestBextValidateASCII checks that a control byte or a non-ASCII byte in a
// free text field is refused rather than silently written and corrupting the
// chunk.
func TestBextValidateASCII(t *testing.T) {
	cases := []struct {
		name  string
		build func(s string) *Bext
	}{
		{"Description", func(s string) *Bext { return &Bext{Description: s} }},
		{"Originator", func(s string) *Bext { return &Bext{Originator: s} }},
		{"OriginatorReference", func(s string) *Bext { return &Bext{OriginatorReference: s} }},
		{"CodingHistory", func(s string) *Bext { return &Bext{CodingHistory: s} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build("clean ascii").validate("test"); err != nil {
				t.Errorf("plain ASCII was rejected: %v", err)
			}
			if err := tc.build("nul\x00byte").validate("test"); err == nil {
				t.Error("an embedded NUL byte was accepted")
			}
			if err := tc.build("bell\abyte").validate("test"); err == nil {
				t.Error("a control byte was accepted")
			}
			if err := tc.build("café").validate("test"); err == nil {
				t.Error("a non-ASCII byte was accepted")
			}
		})
	}
}

// TestBextCodingHistoryPermitsCRLF checks the one deliberate relaxation from
// checkASCII: EBU Tech 3285 defines CodingHistory as one row per transcoding
// step, each terminated by CR then LF, so those two bytes must be accepted
// there even though every other control byte, and every other free text
// field, still rejects them.
func TestBextCodingHistoryPermitsCRLF(t *testing.T) {
	history := "A=PCM,F=48000,W=16,M=mono\r\n"
	b := &Bext{CodingHistory: history}
	if err := b.validate("test"); err != nil {
		t.Fatalf("a CR LF terminated CodingHistory row was rejected: %v", err)
	}

	body := b.serialize()
	if want := bextFixedSize + len(history); len(body) != want {
		t.Fatalf("serialize() = %d bytes, want %d", len(body), want)
	}
	if got := string(body[bextFixedSize:]); got != history {
		t.Errorf("CodingHistory suffix = %q, want %q", got, history)
	}

	t.Run("other control bytes are still refused", func(t *testing.T) {
		if err := (&Bext{CodingHistory: "row one\r\nrow two\x07"}).validate("test"); err == nil {
			t.Error("a bell byte (0x07) alongside a CR LF row break was accepted")
		}
	})
	t.Run("non-ASCII is still refused", func(t *testing.T) {
		if err := (&Bext{CodingHistory: "café\r\n"}).validate("test"); err == nil {
			t.Error("a non-ASCII byte alongside a CR LF row break was accepted")
		}
	})
	t.Run("the fixed-width single-line fields still reject CR LF", func(t *testing.T) {
		if err := (&Bext{Description: "line one\r\nline two"}).validate("test"); err == nil {
			t.Error("Description accepted an embedded CR LF; only CodingHistory should")
		}
		if err := (&Bext{Originator: "a\r\nb"}).validate("test"); err == nil {
			t.Error("Originator accepted an embedded CR LF; only CodingHistory should")
		}
		if err := (&Bext{OriginatorReference: "a\r\nb"}).validate("test"); err == nil {
			t.Error("OriginatorReference accepted an embedded CR LF; only CodingHistory should")
		}
	})
}

// TestBextValidateDateTimeFormat checks that OriginationDate and
// OriginationTime are either empty or exactly the shape the bext fields
// expect, rejecting anything else including an impossible calendar date.
func TestBextValidateDateTimeFormat(t *testing.T) {
	t.Run("date", func(t *testing.T) {
		for _, good := range []string{"", "2026-08-05", "2000-01-01"} {
			if err := (&Bext{OriginationDate: good}).validate("test"); err != nil {
				t.Errorf("OriginationDate %q was rejected: %v", good, err)
			}
		}
		for _, bad := range []string{"2026/08/05", "26-08-05", "2026-08-05x", "2026-02-30", "not-a-date"} {
			if err := (&Bext{OriginationDate: bad}).validate("test"); err == nil {
				t.Errorf("OriginationDate %q was accepted", bad)
			}
		}
	})
	t.Run("time", func(t *testing.T) {
		for _, good := range []string{"", "12:34:56", "00:00:00", "23:59:59"} {
			if err := (&Bext{OriginationTime: good}).validate("test"); err != nil {
				t.Errorf("OriginationTime %q was rejected: %v", good, err)
			}
		}
		for _, bad := range []string{"12:34", "12-34-56", "25:00:00", "12:34:56x"} {
			if err := (&Bext{OriginationTime: bad}).validate("test"); err == nil {
				t.Errorf("OriginationTime %q was accepted", bad)
			}
		}
	})
}
