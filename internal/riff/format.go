package riff

import (
	"encoding/binary"
	"fmt"

	wav "github.com/tphakala/go-wav"
)

// WAVE format tags. The tag names the encoding of the data chunk; under
// tagExtensible the real encoding comes from the SubFormat GUID instead.
const (
	tagPCM        uint16 = 0x0001
	tagIEEEFloat  uint16 = 0x0003
	tagALaw       uint16 = 0x0006
	tagMuLaw      uint16 = 0x0007
	tagExtensible uint16 = 0xFFFE
)

// SubFormat GUIDs, in the byte order they appear on the wire: the first three
// groups little-endian, the trailing eight bytes in sequence.
var (
	// guidPCM is KSDATAFORMAT_SUBTYPE_PCM, 00000001-0000-0010-8000-00aa00389b71.
	guidPCM = [16]byte{
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
		0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
	}
	// guidFloat is KSDATAFORMAT_SUBTYPE_IEEE_FLOAT,
	// 00000003-0000-0010-8000-00aa00389b71.
	guidFloat = [16]byte{
		0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
		0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
	}
)

// Speaker positions of dwChannelMask, in the order interleaved channels must
// appear.
const (
	speakerFrontLeft    uint32 = 0x1
	speakerFrontRight   uint32 = 0x2
	speakerFrontCenter  uint32 = 0x4
	speakerLowFrequency uint32 = 0x8
	speakerBackLeft     uint32 = 0x10
	speakerBackRight    uint32 = 0x20
	speakerBackCenter   uint32 = 0x100
	speakerSideLeft     uint32 = 0x200
	speakerSideRight    uint32 = 0x400
)

// conventionalMasks holds the usual speaker layout for each channel count from
// one to eight. Anything wider gets no mask, since there is no single
// convention worth guessing at.
var conventionalMasks = map[int]uint32{
	1: speakerFrontCenter,
	2: speakerFrontLeft | speakerFrontRight,
	3: speakerFrontLeft | speakerFrontRight | speakerFrontCenter,
	4: speakerFrontLeft | speakerFrontRight | speakerBackLeft | speakerBackRight,
	5: speakerFrontLeft | speakerFrontRight | speakerFrontCenter | speakerBackLeft | speakerBackRight,
	6: speakerFrontLeft | speakerFrontRight | speakerFrontCenter | speakerLowFrequency |
		speakerBackLeft | speakerBackRight,
	7: speakerFrontLeft | speakerFrontRight | speakerFrontCenter | speakerLowFrequency |
		speakerBackCenter | speakerSideLeft | speakerSideRight,
	8: speakerFrontLeft | speakerFrontRight | speakerFrontCenter | speakerLowFrequency |
		speakerBackLeft | speakerBackRight | speakerSideLeft | speakerSideRight,
}

// ConventionalChannelMask returns the usual dwChannelMask for a channel count,
// or 0 when there is no established convention for that width. A mask of 0 is
// valid on the wire and means the layout is unspecified.
func ConventionalChannelMask(channels int) uint32 {
	return conventionalMasks[channels]
}

// Format is a parsed fmt chunk.
type Format struct {
	SampleRate  int
	Channels    int
	BitDepth    int
	ValidBits   int
	BlockAlign  int
	Format      wav.SampleFormat
	Extensible  bool
	ChannelMask uint32
}

// parseFmt decodes a fmt chunk payload.
func parseFmt(b []byte) (Format, error) {
	if len(b) < fmtSizePCM {
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: fmt chunk is %d bytes, want at least %d",
			wav.ErrCorruptStream, len(b), fmtSizePCM)
	}

	var f Format
	tag := binary.LittleEndian.Uint16(b[0:2])
	f.Channels = int(binary.LittleEndian.Uint16(b[2:4]))
	f.SampleRate = int(binary.LittleEndian.Uint32(b[4:8]))
	f.BlockAlign = int(binary.LittleEndian.Uint16(b[12:14]))
	f.BitDepth = int(binary.LittleEndian.Uint16(b[14:16]))

	// Zero channels or a zero sample rate appear in genuinely damaged files
	// and would divide by zero in every size computation downstream, so they
	// are rejected here rather than tolerated.
	if f.Channels == 0 {
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: fmt chunk declares zero channels",
			wav.ErrCorruptStream)
	}
	if f.SampleRate == 0 {
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: fmt chunk declares a zero sample rate",
			wav.ErrCorruptStream)
	}

	if tag == tagExtensible {
		if len(b) < fmtSizeExtensible {
			return Format{}, fmt.Errorf(
				"go-wav/internal/riff: %w: extensible fmt chunk is %d bytes, want %d",
				wav.ErrCorruptStream, len(b), fmtSizeExtensible)
		}
		f.Extensible = true
		f.ValidBits = int(binary.LittleEndian.Uint16(b[18:20]))
		f.ChannelMask = binary.LittleEndian.Uint32(b[20:24])

		var guid [16]byte
		copy(guid[:], b[24:40])
		switch guid {
		case guidPCM:
			tag = tagPCM
		case guidFloat:
			tag = tagIEEEFloat
		default:
			return Format{}, fmt.Errorf(
				"go-wav/internal/riff: %w: SubFormat GUID %x is not PCM or IEEE float", wav.ErrUnsupported, guid)
		}
	}

	switch tag {
	case tagPCM:
		f.Format = wav.SampleFormatPCM
	case tagIEEEFloat:
		f.Format = wav.SampleFormatFloat
	case tagALaw:
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: A-law", wav.ErrUnsupported)
	case tagMuLaw:
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: mu-law", wav.ErrUnsupported)
	default:
		return Format{}, fmt.Errorf("go-wav/internal/riff: %w: format tag 0x%04X", wav.ErrUnsupported, tag)
	}

	if err := validateDepth(f.Format, f.BitDepth); err != nil {
		return Format{}, err
	}

	// A block align of zero is unusable and is repaired from the fields that
	// are present, which is what every tolerant reader does.
	if f.BlockAlign <= 0 {
		f.BlockAlign = (f.BitDepth + 7) / 8 * f.Channels
	}
	// ValidBits wider than the container is nonsense; treat it as absent
	// rather than propagating it.
	if f.ValidBits > f.BitDepth {
		f.ValidBits = 0
	}
	return f, nil
}

// validateDepth reports whether a bit depth is supported for a sample format.
func validateDepth(format wav.SampleFormat, bits int) error {
	switch format {
	case wav.SampleFormatPCM:
		switch bits {
		case 8, 16, 24, 32:
			return nil
		}
		return fmt.Errorf("go-wav/internal/riff: %w: integer bit depth %d (want 8, 16, 24 or 32)",
			wav.ErrUnsupported, bits)
	case wav.SampleFormatFloat:
		switch bits {
		case 32, 64:
			return nil
		}
		return fmt.Errorf("go-wav/internal/riff: %w: float bit depth %d (want 32 or 64)",
			wav.ErrUnsupported, bits)
	default:
		return fmt.Errorf("go-wav/internal/riff: %w: sample format %d", wav.ErrUnsupported, format)
	}
}

// needsExtensible reports whether the format requires the extensible fmt form.
// More than two channels or an integer depth above 16 bits both mandate it, and
// a caller asking for a specific speaker layout needs somewhere to put it.
func needsExtensible(f Format) bool {
	if f.Extensible || f.ChannelMask != 0 {
		return true
	}
	if f.Channels > 2 {
		return true
	}
	return f.Format == wav.SampleFormatPCM && f.BitDepth > 16
}

// appendFmt appends a complete fmt chunk, choosing the 16-byte or 40-byte form.
func appendFmt(dst []byte, f Format) ([]byte, error) {
	extensible := needsExtensible(f)

	// A non-PCM encoding must carry the cbSize field even when it has no
	// extension to describe, so float gets the 18-byte form at minimum.
	// Tools reject or warn on a bare 16-byte fmt chunk for float.
	payload := fmtSizePCM
	switch {
	case extensible:
		payload = fmtSizeExtensible
	case f.Format != wav.SampleFormatPCM:
		payload = fmtSizeExtended
	}

	bytesPerSample := int64((f.BitDepth + 7) / 8)
	blockAlign := bytesPerSample * int64(f.Channels)
	byteRate := blockAlign * int64(f.SampleRate)

	blockAlign16, err := u16("fmt chunk block align", blockAlign)
	if err != nil {
		return nil, err
	}
	byteRate32, err := u32("fmt chunk byte rate", byteRate)
	if err != nil {
		return nil, err
	}
	channels16, err := u16("fmt chunk channel count", int64(f.Channels))
	if err != nil {
		return nil, err
	}
	rate32, err := u32("fmt chunk sample rate", int64(f.SampleRate))
	if err != nil {
		return nil, err
	}

	tag := tagPCM
	if f.Format == wav.SampleFormatFloat {
		tag = tagIEEEFloat
	}
	if extensible {
		tag = tagExtensible
	}

	buf := make([]byte, ChunkHeaderSize+payload)
	putFourCC(buf, idFmt)
	putU32(buf[4:], uint32(payload))
	putU16(buf[8:], tag)
	putU16(buf[10:], channels16)
	putU32(buf[12:], rate32)
	putU32(buf[16:], byteRate32)
	putU16(buf[20:], blockAlign16)
	//nolint:gosec // G115: BitDepth is one of 8, 16, 24, 32 or 64.
	putU16(buf[22:], uint16(f.BitDepth))

	if payload == fmtSizeExtended {
		// cbSize of zero: the field is present, the extension is empty.
		putU16(buf[24:], 0)
	}

	if extensible {
		putU16(buf[24:], fmtExtensibleCBSze)
		validBits := f.ValidBits
		if validBits <= 0 || validBits > f.BitDepth {
			validBits = f.BitDepth
		}
		//nolint:gosec // G115: validBits is bounded by BitDepth.
		putU16(buf[26:], uint16(validBits))

		mask := f.ChannelMask
		if mask == 0 {
			mask = ConventionalChannelMask(f.Channels)
		}
		putU32(buf[28:], mask)

		guid := guidPCM
		if f.Format == wav.SampleFormatFloat {
			guid = guidFloat
		}
		copy(buf[32:], guid[:])
	}
	return append(dst, buf...), nil
}

// u16 narrows a non-negative int64 to uint16, reporting wav.ErrTooLarge rather
// than wrapping.
func u16(op string, v int64) (uint16, error) {
	if v < 0 || v > int64(^uint16(0)) {
		return 0, fmt.Errorf("go-wav/internal/riff: %s: %w: %d does not fit a 16-bit field",
			op, wav.ErrTooLarge, v)
	}
	return uint16(v), nil
}
