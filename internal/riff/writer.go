package riff

import (
	"fmt"
	"io"

	wav "github.com/tphakala/go-wav"
)

// HeaderConfig describes the header to emit.
type HeaderConfig struct {
	Format Format

	// Container selects the flavour to write, which is RIFF or RF64.
	// ContainerRIFF with ReserveDS64 set is the streaming case: a plain RIFF
	// header that can later be rewritten in place as RF64. BW64 is read
	// only and is rejected here; see wav.ContainerBW64.
	Container wav.Container

	// ReserveDS64 emits a JUNK chunk sized to be overwritten by a ds64,
	// immediately after the file header. It is meaningless for a container
	// that already carries a ds64.
	ReserveDS64 bool

	// DataSize is the data chunk length in bytes when it is known up front,
	// or 0 when it is not. A known size lets the header be written once and
	// never patched.
	DataSize int64

	// Frames is the inter-channel frame count matching DataSize, used for
	// the ds64 sampleCount and the fact chunk.
	Frames uint64
}

// Layout is an emitted header plus the offsets a caller needs in order to
// patch it once the true sizes are known. Offsets are from the start of the
// stream, and are -1 when the corresponding field is absent.
type Layout struct {
	// Bytes is the complete header, written before any audio.
	Bytes []byte

	// RIFFSizeOffset locates the 32-bit size in the file header.
	RIFFSizeOffset int64

	// DataSizeOffset locates the 32-bit size of the data chunk.
	DataSizeOffset int64

	// DS64Offset locates the ds64 or JUNK chunk header, or -1 when neither
	// is present. The payload begins ChunkHeaderSize bytes later.
	DS64Offset int64

	// FactOffset locates the fact chunk payload, or -1 when absent.
	FactOffset int64

	// DataOffset is where audio begins, and therefore the length of Bytes.
	DataOffset int64
}

// BuildHeader emits a complete WAVE header.
//
// When cfg.DataSize is zero the size fields are written as zero and the caller
// is expected to patch them through [PatchSizes], or to have declared the
// length up front so that this function can compute them.
//
//nolint:gocritic // HeaderConfig by value mirrors the by-value Config of the public API.
func BuildHeader(cfg HeaderConfig) (*Layout, error) {
	if err := validateDepth(cfg.Format.Format, cfg.Format.BitDepth); err != nil {
		return nil, err
	}
	if cfg.Format.Channels <= 0 {
		return nil, fmt.Errorf("go-wav/internal/riff: channel count %d must be positive", cfg.Format.Channels)
	}
	if cfg.Format.SampleRate <= 0 {
		return nil, fmt.Errorf("go-wav/internal/riff: sample rate %d must be positive", cfg.Format.SampleRate)
	}
	// The magic is written from Container.String, so an out-of-range value
	// would emit a seven-byte "unknown" where four bytes belong and corrupt
	// every offset after it.
	switch cfg.Container {
	case wav.ContainerRIFF, wav.ContainerRF64:
	case wav.ContainerBW64:
		// BW64 is read only. What separates it from RF64 is the ADM metadata
		// it carries in axml and chna chunks, and this library emits neither,
		// so a header written with the BW64 magic would be an RF64 file under
		// a name promising metadata it does not hold. See wav.ContainerBW64.
		return nil, fmt.Errorf(
			"go-wav/internal/riff: %w: BW64 is read only, because none of the ADM metadata "+
				"that separates it from RF64 is written", wav.ErrUnsupported)
	default:
		return nil, fmt.Errorf("go-wav/internal/riff: unknown container %d", cfg.Container)
	}

	sized64 := cfg.Container.Sized64()
	writeFact := cfg.Format.Format == wav.SampleFormatFloat

	lay := &Layout{
		DS64Offset: -1,
		FactOffset: -1,
	}

	// Exactly the length HeaderLen predicts, so the buffer never grows and
	// every BuildHeader call exercises the arithmetic HeaderLen states.
	buf := make([]byte, 0, HeaderLen(cfg))

	// File header. The size is filled in below once the rest is measured.
	buf = append(buf, cfg.Container.String()...)
	lay.RIFFSizeOffset = int64(len(buf))
	buf = append(buf, 0, 0, 0, 0)
	buf = append(buf, idWAVE...)

	// A ds64 chunk, or the JUNK placeholder that can become one, must sit
	// immediately after the file header.
	switch {
	case sized64:
		lay.DS64Offset = int64(len(buf))
		buf = appendChunkHeader(buf, idDS64, DS64PayloadSize)
		buf = append(buf, make([]byte, DS64PayloadSize)...)
	case cfg.ReserveDS64:
		lay.DS64Offset = int64(len(buf))
		buf = appendChunkHeader(buf, idJUNK, DS64PayloadSize)
		buf = append(buf, make([]byte, DS64PayloadSize)...)
	}

	var err error
	if buf, err = appendFmt(buf, cfg.Format); err != nil {
		return nil, err
	}

	// The fact chunk is mandatory for every non-PCM encoding, which for this
	// library means float.
	if writeFact {
		buf = appendChunkHeader(buf, idFact, factPayloadSize)
		lay.FactOffset = int64(len(buf))
		buf = append(buf, 0, 0, 0, 0)
	}

	buf = append(buf, idData...)
	lay.DataSizeOffset = int64(len(buf))
	buf = append(buf, 0, 0, 0, 0)

	lay.DataOffset = int64(len(buf))
	lay.Bytes = buf

	// Fill in whatever is already known.
	if err := writeSizesInto(lay, cfg.Container, cfg.DataSize, cfg.Frames); err != nil {
		return nil, err
	}
	return lay, nil
}

// appendChunkHeader appends an identifier and a payload size.
func appendChunkHeader(dst []byte, id string, size uint32) []byte {
	var hdr [ChunkHeaderSize]byte
	putFourCC(hdr[:], id)
	putU32(hdr[4:], size)
	return append(dst, hdr[:]...)
}

// riffSizeFor returns the value of the file header size field: the whole file
// less the eight bytes of the magic and the size field itself. The audio is
// counted padded, because the pad byte is on disk even though it is not part of
// the data chunk's own size.
//
// The ds64 riffSize is defined identically, so both containers share this.
func riffSizeFor(lay *Layout, dataSize int64) int64 {
	return riffSizeForLen(lay.DataOffset, dataSize)
}

// riffSizeForLen is riffSizeFor expressed against a header length, so callers
// that know only the length share the one statement of the formula.
func riffSizeForLen(headerLen, dataSize int64) int64 {
	return headerLen + padded(dataSize) - 8
}

// writeSizesInto stamps the size fields into an in-memory header.
func writeSizesInto(lay *Layout, container wav.Container, dataSize int64, frames uint64) error {
	if dataSize < 0 {
		return fmt.Errorf("go-wav/internal/riff: data size %d must not be negative", dataSize)
	}
	riffSize := riffSizeFor(lay, dataSize)

	if container.Sized64() {
		// The 32-bit fields are superseded and carry the sentinel; the ds64
		// chunk holds the real values.
		putU32(lay.Bytes[lay.RIFFSizeOffset:], sentinel32)
		putU32(lay.Bytes[lay.DataSizeOffset:], sentinel32)
		if lay.DS64Offset < 0 {
			return fmt.Errorf("go-wav/internal/riff: %s header has no ds64 chunk", container)
		}
		p := lay.DS64Offset + int64(ChunkHeaderSize)
		//nolint:gosec // G115: both values are non-negative int64.
		putU64(lay.Bytes[p:], uint64(riffSize))
		//nolint:gosec // G115: checked non-negative above.
		putU64(lay.Bytes[p+8:], uint64(dataSize))
		putU64(lay.Bytes[p+16:], frames)
		putU32(lay.Bytes[p+24:], 0)
	} else {
		riff32, err := u32("file header size", riffSize)
		if err != nil {
			return err
		}
		data32, err := u32("data chunk size", dataSize)
		if err != nil {
			return err
		}
		putU32(lay.Bytes[lay.RIFFSizeOffset:], riff32)
		putU32(lay.Bytes[lay.DataSizeOffset:], data32)
	}

	if lay.FactOffset >= 0 {
		//nolint:gosec // G115: a frame count past 2^32 is clamped below.
		f32 := uint32(frames)
		if frames > uint64(maxUint32) {
			f32 = sentinel32
		}
		putU32(lay.Bytes[lay.FactOffset:], f32)
	}
	return nil
}

// FitsRIFF reports whether a stream of dataSize bytes can be described by the
// 32-bit size fields of a plain RIFF header.
func FitsRIFF(lay *Layout, dataSize int64) bool {
	return fitsRIFF(lay.DataOffset, dataSize)
}

// fitsRIFF is the size test expressed against a header length, so a caller who
// only needs the answer does not have to build a header to get it.
func fitsRIFF(headerLen, dataSize int64) bool {
	return dataSize >= 0 && dataSize <= maxUint32 && riffSizeForLen(headerLen, dataSize) <= maxUint32
}

// HeaderLen returns the number of bytes [BuildHeader] would emit for cfg,
// computed rather than built.
//
// It exists so that deciding whether a stream needs RF64 costs arithmetic
// instead of a throwaway header and its allocations, which matters because that
// decision is made once per stream and a caller may write many short streams.
//
// It assumes a cfg that BuildHeader would accept and validates nothing itself,
// and it reads only the fields the layout depends on: DataSize and Frames do
// not affect the header's length.
//
//nolint:gocritic // HeaderConfig by value matches BuildHeader and the by-value public Config.
func HeaderLen(cfg HeaderConfig) int64 {
	n := int64(FileHeaderSize)
	if cfg.Container.Sized64() || cfg.ReserveDS64 {
		n += int64(DS64ChunkSize)
	}
	n += int64(ChunkHeaderSize) + int64(fmtPayloadLen(cfg.Format))
	if cfg.Format.Format == wav.SampleFormatFloat {
		n += int64(ChunkHeaderSize) + int64(factPayloadSize)
	}
	return n + int64(ChunkHeaderSize) // the data chunk's own id and size
}

// FitsPlainRIFF reports whether a stream of dataSize bytes described by cfg
// fits the 32-bit size fields of a plain RIFF header.
//
//nolint:gocritic // HeaderConfig by value, as above.
func FitsPlainRIFF(cfg HeaderConfig, dataSize int64) bool {
	return fitsRIFF(HeaderLen(cfg), dataSize)
}

// PatchSizes rewrites the size fields of an already written header in place,
// leaving the stream positioned at its end.
//
// It is the seekable-sink path: the header goes out with zeroes, audio is
// streamed, and the true sizes are stamped at Close.
func PatchSizes(w io.WriteSeeker, lay *Layout, container wav.Container, dataSize int64, frames uint64) error {
	// The magic was fixed when the header was built, so patching sizes for a
	// different container would stamp 64-bit sizes into a file still claiming
	// to be plain RIFF, or the reverse. Changing container is what
	// UpgradeToRF64 is for.
	if got := string(lay.Bytes[:4]); got != container.String() {
		return fmt.Errorf(
			"go-wav/internal/riff: cannot patch a %q header as %s; use UpgradeToRF64 to change container",
			got, container)
	}
	patched := &Layout{
		Bytes:          make([]byte, len(lay.Bytes)),
		RIFFSizeOffset: lay.RIFFSizeOffset,
		DataSizeOffset: lay.DataSizeOffset,
		DS64Offset:     lay.DS64Offset,
		FactOffset:     lay.FactOffset,
		DataOffset:     lay.DataOffset,
	}
	copy(patched.Bytes, lay.Bytes)
	if err := writeSizesInto(patched, container, dataSize, frames); err != nil {
		return err
	}
	return rewriteHead(w, patched.Bytes)
}

// UpgradeToRF64 rewrites a header that was emitted as plain RIFF with a
// reserved JUNK chunk so that it becomes a valid RF64 stream: the magic
// changes, the 32-bit sizes become the sentinel, the JUNK chunk becomes a ds64
// carrying the real 64-bit sizes, and the stream is left positioned at its end.
//
// This is the technique ffmpeg calls "-rf64 auto". It requires that the header
// was built with ReserveDS64, since there is otherwise nowhere to put the ds64
// without shifting every byte of audio.
//
// The container argument exists to mirror [PatchSizes], and RF64 is the only
// value it accepts: BW64 is the other 64-bit container, and this library reads
// it without ever writing it. See wav.ContainerBW64.
func UpgradeToRF64(w io.WriteSeeker, lay *Layout, container wav.Container, dataSize int64, frames uint64) error {
	// The container is checked first because it validates the caller's
	// argument, while the reserved space describes the header that was already
	// built. Asking for BW64 is wrong whether or not a ds64 was reserved, and
	// reporting the missing reservation instead would send the caller after
	// the wrong problem.
	if container != wav.ContainerRF64 {
		return fmt.Errorf(
			"go-wav/internal/riff: cannot upgrade to %s: RF64 is the only 64-bit container written",
			container)
	}
	if lay.DS64Offset < 0 {
		return fmt.Errorf(
			"go-wav/internal/riff: cannot upgrade to %s: no ds64 space was reserved in the header", container)
	}

	patched := &Layout{
		Bytes:          make([]byte, len(lay.Bytes)),
		RIFFSizeOffset: lay.RIFFSizeOffset,
		DataSizeOffset: lay.DataSizeOffset,
		DS64Offset:     lay.DS64Offset,
		FactOffset:     lay.FactOffset,
		DataOffset:     lay.DataOffset,
	}
	copy(patched.Bytes, lay.Bytes)

	// The magic and the reserved chunk identifier both change.
	putFourCC(patched.Bytes, container.String())
	putFourCC(patched.Bytes[patched.DS64Offset:], idDS64)

	if err := writeSizesInto(patched, container, dataSize, frames); err != nil {
		return err
	}
	return rewriteHead(w, patched.Bytes)
}

// rewriteHead writes b at the start of w and restores the position to the end.
func rewriteHead(w io.WriteSeeker, b []byte) error {
	end, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Seek(end, io.SeekStart); err != nil {
		return err
	}
	return nil
}
