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
// Every string field below is written ASCII and NUL-padded to a fixed wire
// width; Config.validate rejects a value that does not fit rather than
// truncating it. UMID and the five loudness fields the format defines are
// always written zero, since this package has no source for them.
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

	// Version is the bext chunk version number, written verbatim.
	Version uint16

	// CodingHistory is a free text record of the coding processes applied to
	// the audio. It is appended after the fixed 602-byte body and may be
	// empty.
	CodingHistory string
}

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
	if err := checkASCII(op, "CodingHistory", b.CodingHistory); err != nil {
		return err
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

	// UMID and the five loudness fields carry no source in this package, so
	// they are written zero, which BWF readers treat as "not present".
	buf = append(buf, make([]byte, bextUMIDSize)...)
	buf = append(buf, make([]byte, bextLoudnessFieldSize*bextLoudnessFieldCount)...)
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
