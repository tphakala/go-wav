package pcm

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Field widths of the bext (Broadcast Wave Format) chunk body, as EBU Tech
// 3285 lays them out. Each width is also the maximum a Bext string field may
// occupy: validate rejects a value that does not fit rather than truncating
// it, since a silently shortened description or reference is exactly the
// corruption this package exists to avoid.
const (
	bextDescriptionSize         = 256
	bextOriginatorSize          = 32
	bextOriginatorReferenceSize = 32
	bextOriginationDateSize     = 10
	bextOriginationTimeSize     = 8
	bextTimeReferenceSize       = 8 // two little-endian uint32 halves
	bextVersionSize             = 2
	bextUMIDSize                = 64
	bextLoudnessFieldSize       = 2
	// bextLoudnessFieldCount is LoudnessValue, LoudnessRange,
	// MaxTruePeakLevel, MaxMomentaryLoudness and MaxShortTermLoudness.
	bextLoudnessFieldCount = 5
	bextReservedSize       = 180

	// bextFixedSize is the bext body up to CodingHistory, the only
	// variable-width part: 256+32+32+10+8+8+2+64+2*5+180 = 602 bytes.
	bextFixedSize = bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize +
		bextOriginationDateSize + bextOriginationTimeSize + bextTimeReferenceSize +
		bextVersionSize + bextUMIDSize + bextLoudnessFieldSize*bextLoudnessFieldCount +
		bextReservedSize
)

// Bext describes a bext (Broadcast Wave Format) chunk to write ahead of the
// audio data, following the layout EBU Tech 3285 defines. A nil Config.Bext
// writes no bext chunk at all, and the encoder's output is then byte-for-byte
// identical to a Config that never mentions this type.
//
// Bext is written only, never read back: a decoder that opens a stream
// carrying one skips it like any other chunk it does not recognise, the same
// as every other metadata chunk this package does not expose on read.
//
// Every string field below is written ASCII and NUL-padded to a fixed wire
// width; Config.validate rejects a value that does not fit rather than
// truncating it. UMID is binary and written verbatim; the five loudness
// fields are signed little-endian int16 values. Version gates those last two
// groups, and validate keeps Version and the fields consistent: see the field
// documentation below.
type Bext struct {
	// Description is free text describing the sound sequence. NUL-padded to
	// 256 bytes on the wire; must not exceed that width.
	Description string

	// Originator names the producing device or station. NUL-padded to 32
	// bytes on the wire; must not exceed that width.
	Originator string

	// OriginatorReference is an unambiguous reference allocated by the
	// originator. NUL-padded to 32 bytes on the wire; must not exceed that
	// width.
	OriginatorReference string

	// OriginationDate is the date of creation, formatted "YYYY-MM-DD", the
	// 10-byte field the format defines. Empty writes ten NUL bytes.
	OriginationDate string

	// OriginationTime is the time of creation, formatted "HH:MM:SS", the
	// 8-byte field the format defines. Empty writes eight NUL bytes.
	OriginationTime string

	// TimeReference is the number of samples, counted from midnight of
	// OriginationDate, at which the first sample of the file occurs. It is
	// stored on the wire as two little-endian 32-bit words, the low half
	// first.
	TimeReference uint64

	// Version is the bext chunk version number, written verbatim. It selects
	// which of the fields below a reader interprets: version 1 adds UMID,
	// version 2 adds the five loudness fields. validate keeps Version and those
	// fields consistent, refusing a UMID set with Version below 1 or a loudness
	// field set with Version below 2. The converse is deliberately not
	// enforced: a version 2 chunk that leaves the loudness fields zero asserts
	// a measured zero, not "not measured", so populate them or lower Version if
	// that is not what you mean.
	Version uint16

	// UMID is the SMPTE 330M unique material identifier that bext version 1
	// adds, written verbatim as the 64 raw bytes of the field. Unlike the text
	// fields above it is binary, not ASCII, so it is neither width-checked nor
	// character-checked; the array itself is the wire field. The zero value
	// writes 64 NUL bytes, which readers treat as "no UMID". validate refuses a
	// non-zero UMID unless Version is at least 1.
	UMID [bextUMIDSize]byte

	// LoudnessValue, LoudnessRange, MaxTruePeakLevel, MaxMomentaryLoudness and
	// MaxShortTermLoudness are the five signed loudness metrics that bext
	// version 2 adds, from EBU R128 and Tech 3285. Each is a little-endian
	// int16 in units of 0.01: LoudnessValue, MaxMomentaryLoudness and
	// MaxShortTermLoudness are LUFS, LoudnessRange is LU, and MaxTruePeakLevel
	// is dBTP. The zero value writes zeros; validate refuses any non-zero
	// loudness field unless Version is at least 2.
	LoudnessValue        int16
	LoudnessRange        int16
	MaxTruePeakLevel     int16
	MaxMomentaryLoudness int16
	MaxShortTermLoudness int16

	// CodingHistory is a free text record of the coding processes applied to
	// the audio. It is appended after the fixed 602-byte body and may be
	// empty. Unlike the fixed-width text fields above, EBU Tech 3285 defines
	// CodingHistory as multiple rows, one per transcoding step, each
	// terminated by CR (0x0D) then LF (0x0A); those two bytes are the only
	// control characters this field accepts.
	CodingHistory string
}

// Names of the five bext loudness fields, used in validate's version-gating
// error messages and, in the tests, as labels tied to the same wire order.
// They are constants because each name appears at more than one site.
const (
	bextLoudnessValueName        = "LoudnessValue"
	bextLoudnessRangeName        = "LoudnessRange"
	bextMaxTruePeakLevelName     = "MaxTruePeakLevel"
	bextMaxMomentaryLoudnessName = "MaxMomentaryLoudness"
	bextMaxShortTermLoudnessName = "MaxShortTermLoudness"
)

// validate reports the first problem with a bext descriptor, or nil. The op
// names the calling entry point, matching Config.validate.
func (b *Bext) validate(op string) error {
	if err := checkASCIIWidth(op, "Description", b.Description, bextDescriptionSize); err != nil {
		return err
	}
	if err := checkASCIIWidth(op, "Originator", b.Originator, bextOriginatorSize); err != nil {
		return err
	}
	if err := checkASCIIWidth(op, "OriginatorReference", b.OriginatorReference, bextOriginatorReferenceSize); err != nil {
		return err
	}
	if err := checkDateTime(op, "OriginationDate", b.OriginationDate, time.DateOnly); err != nil {
		return err
	}
	if err := checkDateTime(op, "OriginationTime", b.OriginationTime, time.TimeOnly); err != nil {
		return err
	}
	if err := checkCodingHistory(op, "CodingHistory", b.CodingHistory); err != nil {
		return err
	}
	// UMID is a bext version 1 field and the loudness values are version 2
	// fields. Refuse a value paired with a Version too low to make it readable:
	// a lower-version reader ignores the bytes, so leaving Version there while
	// filling the field in silently drops the datum, and a zero left in a field
	// the Version does advertise is itself a claim (a measured zero), which is
	// why validate gates on the field being non-zero rather than the reverse.
	if b.UMID != ([bextUMIDSize]byte{}) && b.Version < 1 {
		return fmt.Errorf("go-wav/pcm: %s: bext UMID is set but Version is %d; UMID needs Version >= 1",
			op, b.Version)
	}
	if b.Version < 2 {
		loudness := []struct {
			name string
			val  int16
		}{
			{bextLoudnessValueName, b.LoudnessValue},
			{bextLoudnessRangeName, b.LoudnessRange},
			{bextMaxTruePeakLevelName, b.MaxTruePeakLevel},
			{bextMaxMomentaryLoudnessName, b.MaxMomentaryLoudness},
			{bextMaxShortTermLoudnessName, b.MaxShortTermLoudness},
		}
		for _, f := range loudness {
			if f.val != 0 {
				return fmt.Errorf("go-wav/pcm: %s: bext %s is set but Version is %d; the loudness fields need Version >= 2",
					op, f.name, b.Version)
			}
		}
	}
	return nil
}

// checkASCIIWidth reports an error when s is not printable ASCII or exceeds
// width bytes.
func checkASCIIWidth(op, field, s string, width int) error {
	if len(s) > width {
		return fmt.Errorf("go-wav/pcm: %s: bext %s is %d bytes, exceeds the %d byte field",
			op, field, len(s), width)
	}
	return checkASCII(op, field, s)
}

// checkASCII reports an error when s carries a byte outside printable ASCII,
// 0x20 through 0x7E, the same range internal/riff uses to sanity check a
// chunk identifier. It excludes NUL and every other control byte, which
// would otherwise land in a field this package NUL-pads or length-prefixes
// and corrupt the boundary with its neighbour.
func checkASCII(op, field, s string) error {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c > 0x7E {
			return fmt.Errorf("go-wav/pcm: %s: bext %s contains byte %#02x at offset %d, outside printable ASCII",
				op, field, c, i)
		}
	}
	return nil
}

// checkCodingHistory reports an error when s carries a byte outside
// printable ASCII and outside CR (0x0D) and LF (0x0A), the row terminator
// EBU Tech 3285 defines for CodingHistory. This is deliberately more
// permissive than checkASCII: a real CodingHistory is multiple rows, one per
// transcoding step, each ending CR LF, so the strict single-line check the
// other free text fields use would make a compliant history impossible to
// write. Every other control byte, and anything outside ASCII, is still
// refused.
func checkCodingHistory(op, field, s string) error {
	for i := range len(s) {
		c := s[i]
		if c == '\r' || c == '\n' {
			continue
		}
		if c < 0x20 || c > 0x7E {
			return fmt.Errorf(
				"go-wav/pcm: %s: bext %s contains byte %#02x at offset %d, outside printable ASCII "+
					"(CR and LF are the only control bytes accepted, for row breaks)",
				op, field, c, i)
		}
	}
	return nil
}

// checkDateTime reports an error when s is non-empty and does not parse
// under layout, which is time.DateOnly for OriginationDate or time.TimeOnly
// for OriginationTime. Parsing catches both a malformed shape and an
// impossible calendar value such as day 30 of February, which a plain width
// or character class check would miss.
func checkDateTime(op, field, s, layout string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse(layout, s); err != nil {
		return fmt.Errorf("go-wav/pcm: %s: bext %s %q does not match what bext expects: %w",
			op, field, s, err)
	}
	return nil
}

// serialize renders the bext chunk body: the 602-byte fixed part followed by
// CodingHistory. The caller must have validated b first; serialize trusts
// that every fixed field already fits its wire width.
//
// The returned bytes are handed to riff.HeaderConfig.Bext as an opaque
// payload; internal/riff stays format agnostic, and this package is the only
// one that knows what they mean.
func (b *Bext) serialize() []byte {
	buf := make([]byte, 0, bextFixedSize+len(b.CodingHistory))
	buf = appendPadded(buf, b.Description, bextDescriptionSize)
	buf = appendPadded(buf, b.Originator, bextOriginatorSize)
	buf = appendPadded(buf, b.OriginatorReference, bextOriginatorReferenceSize)
	buf = appendPadded(buf, b.OriginationDate, bextOriginationDateSize)
	buf = appendPadded(buf, b.OriginationTime, bextOriginationTimeSize)

	var timeRef [bextTimeReferenceSize]byte
	//nolint:gosec // G115: intentional truncation, splitting TimeReference into its low half.
	binary.LittleEndian.PutUint32(timeRef[0:4], uint32(b.TimeReference))
	//nolint:gosec // G115: intentional truncation, splitting TimeReference into its high half.
	binary.LittleEndian.PutUint32(timeRef[4:8], uint32(b.TimeReference>>32))
	buf = append(buf, timeRef[:]...)

	var version [bextVersionSize]byte
	binary.LittleEndian.PutUint16(version[:], b.Version)
	buf = append(buf, version[:]...)

	// UMID (bext version 1) and the five loudness fields (version 2) are
	// written from the descriptor. validate has already refused a non-zero
	// value paired with a Version too low to make it readable, so a zero here
	// is a deliberate "not present", which BWF readers treat as absent.
	buf = append(buf, b.UMID[:]...)

	var loudness [bextLoudnessFieldSize * bextLoudnessFieldCount]byte
	putInt16LE(loudness[0:2], b.LoudnessValue)
	putInt16LE(loudness[2:4], b.LoudnessRange)
	putInt16LE(loudness[4:6], b.MaxTruePeakLevel)
	putInt16LE(loudness[6:8], b.MaxMomentaryLoudness)
	putInt16LE(loudness[8:10], b.MaxShortTermLoudness)
	buf = append(buf, loudness[:]...)

	buf = append(buf, make([]byte, bextReservedSize)...)

	buf = append(buf, b.CodingHistory...)
	return buf
}

// appendPadded appends s to dst as a fixed-width, NUL-padded ASCII field. The
// caller must have already checked len(s) <= width.
func appendPadded(dst []byte, s string, width int) []byte {
	field := make([]byte, width)
	copy(field, s)
	return append(dst, field...)
}

// putInt16LE writes v as a little-endian 16-bit value into the first two bytes
// of dst. The bext loudness fields are signed (EBU R128 values in units of
// 0.01 LU, LUFS or dBTP), so the sign bit is part of the datum; the uint16
// conversion reinterprets the same 16 bits for the wire and never loses any.
func putInt16LE(dst []byte, v int16) {
	//nolint:gosec // G115: width-preserving int16 to uint16 reinterpretation of a fixed 16-bit wire field.
	binary.LittleEndian.PutUint16(dst, uint16(v))
}
