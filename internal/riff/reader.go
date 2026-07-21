package riff

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	wav "github.com/tphakala/go-wav"
)

// maxChunkPayload bounds how much of an auxiliary chunk the reader will hold in
// memory. A size field is attacker-controlled, so a chunk claiming gigabytes
// must not cause a matching allocation; anything larger than this is skipped
// rather than buffered. It is far above any real fmt or ds64 chunk.
const maxChunkPayload = 1 << 20

// Header is a parsed WAVE file header, positioned so that the next byte the
// source yields is the first byte of audio.
type Header struct {
	// Info describes the stream. Its DataSizeKnown mirrors DataSize below;
	// TotalFrames is whatever count the header yielded, which is not
	// necessarily 0 when the length is undeterminable, since a ds64
	// sampleCount or a fact chunk can supply one where the size did not.
	Info wav.StreamInfo

	// DataSize is the length of the data chunk in bytes, or -1 when the
	// stream did not declare a usable one and the caller should read to the
	// end of the source.
	DataSize int64

	// BlockAlign is the declared bytes per frame, repaired from the other
	// fmt fields when the stream declared a nonsensical value.
	BlockAlign int
}

// DataSizeUnknown reports whether the data chunk length was undeterminable, in
// which case the caller reads to EOF.
func (h *Header) DataSizeUnknown() bool { return h.DataSize == sizeUnknown }

// ParseHeader reads a WAVE header from br and leaves it positioned at the first
// byte of audio.
//
// It never seeks, so it works on a pipe. It tolerates the deviations real files
// exhibit, described in the package documentation, but it never guesses a
// sample format: a stream it cannot decode is reported rather than
// reinterpreted.
func ParseHeader(br *bufio.Reader) (*Header, error) {
	container, err := readFileHeader(br)
	if err != nil {
		return nil, err
	}

	var (
		fmtChunk   Format
		haveFmt    bool
		ds64       ds64Info
		haveDS64   bool
		factFrames uint64
		dataSize   = sizeUnknown
		haveData   bool
	)

	for !haveData {
		id, size, err := readChunkHeader(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		switch id {
		case idDS64:
			payload, rerr := readPayload(br, size)
			if rerr != nil {
				return nil, rerr
			}
			ds64, err = parseDS64(payload)
			if err != nil {
				return nil, err
			}
			haveDS64 = true

		case idFmt:
			payload, rerr := readPayload(br, size)
			if rerr != nil {
				return nil, rerr
			}
			fmtChunk, err = parseFmt(payload)
			if err != nil {
				return nil, err
			}
			haveFmt = true

		case idFact:
			payload, rerr := readPayload(br, size)
			if rerr != nil {
				return nil, rerr
			}
			if len(payload) >= factPayloadSize {
				factFrames = uint64(binary.LittleEndian.Uint32(payload[:factPayloadSize]))
			}

		case idData:
			// The data chunk is not consumed; the caller streams it.
			dataSize = resolveDataSize(size, container, haveDS64, ds64)
			haveData = true
			continue

		default:
			if err := skipChunk(br, size); err != nil {
				return nil, err
			}
			continue
		}

		if err := skipPad(br, size); err != nil {
			return nil, err
		}
	}

	if !haveFmt {
		return nil, fmt.Errorf("go-wav/internal/riff: %w: no fmt chunk", wav.ErrCorruptStream)
	}
	if !haveData {
		return nil, fmt.Errorf("go-wav/internal/riff: %w: no data chunk", wav.ErrCorruptStream)
	}
	if container.Sized64() && !haveDS64 {
		return nil, fmt.Errorf(
			"go-wav/internal/riff: %w: %s stream has no ds64 chunk, so its sizes are unrecoverable",
			wav.ErrCorruptStream, container)
	}

	h := &Header{
		DataSize:   dataSize,
		BlockAlign: fmtChunk.BlockAlign,
		Info: wav.StreamInfo{
			SampleRate:     fmtChunk.SampleRate,
			Channels:       fmtChunk.Channels,
			BitDepth:       fmtChunk.BitDepth,
			SourceBitDepth: fmtChunk.BitDepth,
			ValidBits:      fmtChunk.ValidBits,
			Format:         fmtChunk.Format,
			SourceFormat:   fmtChunk.Format,
			Container:      container,
			Extensible:     fmtChunk.Extensible,
			ChannelMask:    fmtChunk.ChannelMask,
		},
	}
	h.Info.TotalFrames = resolveFrames(dataSize, int64(fmtChunk.BlockAlign), ds64, haveDS64, factFrames)
	h.Info.DataSizeKnown = dataSize != sizeUnknown
	return h, nil
}

// resolveDataSize picks the authoritative data chunk length. In an RF64 or BW64
// stream the 32-bit field is a sentinel and ds64 holds the truth; elsewhere a
// zero or all-ones field means the writer never patched it, so the length is
// unknown and the caller reads to EOF.
func resolveDataSize(size uint32, container wav.Container, haveDS64 bool, ds64 ds64Info) int64 {
	if container.Sized64() && haveDS64 {
		// A ds64 dataSize of zero means the sizes were never stamped, so
		// the length is unknown and the caller reads to the end.
		//
		// A header interrupted before its sizes were patched is
		// indistinguishable from one describing an empty stream: both are
		// internally consistent. Recovery is the useful reading, since a
		// 64-bit container holding no audio is a contradiction in terms,
		// and reading to the end of a genuinely empty stream yields nothing
		// anyway.
		if ds64.dataSize == 0 || ds64.dataSize > maxDataSize {
			return sizeUnknown
		}
		//nolint:gosec // G115: bounded by the check above.
		return int64(ds64.dataSize)
	}
	if size == 0 || size == sentinel32 {
		return sizeUnknown
	}
	return int64(size)
}

// resolveFrames determines the inter-channel frame count. The data chunk size
// is authoritative because it describes bytes actually present; ds64's
// sampleCount and the fact chunk are consulted only when it is unknown, and
// never allowed to override it.
//
// Both fallbacks are bounded on the way through, the way resolveDataSize bounds
// the size it returns, because a count nothing corroborates is exactly the
// thing a corrupt or hostile header supplies. The result lands in the exported
// StreamInfo.TotalFrames, where an unchecked value near the top of the range
// makes int64(TotalFrames) negative and turns the obvious buffer sizing into a
// panic or an absurd allocation. A count that fails the check is reported as 0,
// which is how the rest of the package says it does not know.
func resolveFrames(dataSize, blockAlign int64, ds64 ds64Info, haveDS64 bool, factFrames uint64) uint64 {
	if dataSize != sizeUnknown && blockAlign > 0 {
		//nolint:gosec // G115: dataSize and blockAlign are both non-negative here.
		return uint64(dataSize / blockAlign)
	}
	if haveDS64 && ds64.sampleCount != 0 {
		// A rejected ds64 count stops here rather than falling through to the
		// fact chunk. In a 64-bit container the fact count is the field ds64
		// supersedes, so it is the less trustworthy of the two, and trading a
		// count that says "unknown" for another one that says "unknown" is not
		// a recovery.
		return boundedFrames(ds64.sampleCount, blockAlign)
	}
	// 0xFFFFFFFF in the fact chunk is that supersession marker rather than a
	// count, the same value resolveDataSize declines to read as a length. A
	// writer stamps it once the real count no longer fits 32 bits, this
	// library's own included, so reading it back as a number would report
	// 4294967295 frames for a file that declared it could not say.
	if factFrames == uint64(sentinel32) {
		return 0
	}
	return boundedFrames(factFrames, blockAlign)
}

// boundedFrames rejects a declared frame count whose audio could not fit under
// the maxDataSize ceiling, reporting 0 (unknown) instead. blockAlign is the
// frame width in bytes; when the width is not known the count is bounded on its
// own, which still keeps int64(TotalFrames) from going negative.
//
// A count reaching here was written into the file by something else and is not
// corroborated by any bytes the reader has seen, which is what separates it
// from the measured length the caller prefers.
func boundedFrames(frames uint64, blockAlign int64) uint64 {
	limit := maxDataSize
	if blockAlign > 0 {
		//nolint:gosec // G115: blockAlign is positive here.
		limit /= uint64(blockAlign)
	}
	if frames > limit {
		return 0
	}
	return frames
}

// readFileHeader consumes the twelve-byte file header. The 32-bit size it
// carries is deliberately discarded: it is a sentinel under RF64, routinely
// wrong elsewhere, and the data chunk size is what actually bounds the audio.
func readFileHeader(br *bufio.Reader) (wav.Container, error) {
	var hdr [FileHeaderSize]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("go-wav/internal/riff: %w: stream is shorter than a file header",
				wav.ErrNotRIFF)
		}
		return 0, err
	}

	if string(hdr[8:12]) != idWAVE {
		return 0, fmt.Errorf("go-wav/internal/riff: %w: form type %q is not %q",
			wav.ErrNotRIFF, hdr[8:12], idWAVE)
	}

	switch string(hdr[0:4]) {
	case idRIFF:
		return wav.ContainerRIFF, nil
	case idRF64:
		return wav.ContainerRF64, nil
	case idBW64:
		return wav.ContainerBW64, nil
	default:
		return 0, fmt.Errorf("go-wav/internal/riff: %w: magic %q", wav.ErrNotRIFF, hdr[0:4])
	}
}

// readChunkHeader consumes one chunk header, resolving a missing pad byte left
// over from the previous chunk.
func readChunkHeader(br *bufio.Reader) (id string, size uint32, err error) {
	var hdr [ChunkHeaderSize]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil { //nolint:govet // shadowing the named result is intentional here.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// A trailing fragment too short to be a chunk is not damage
			// worth reporting; it is the end of the useful stream.
			return "", 0, io.EOF
		}
		return "", 0, err
	}
	return string(hdr[0:4]), binary.LittleEndian.Uint32(hdr[4:8]), nil
}

// readPayload reads a chunk payload the parser needs in memory. Oversized
// chunks are skipped rather than buffered, so a hostile size field cannot drive
// an allocation.
func readPayload(br *bufio.Reader, size uint32) ([]byte, error) {
	if int64(size) > maxChunkPayload {
		if err := discardN(br, int64(size)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(br, buf)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			// A chunk truncated by the end of the file is tolerated, but
			// only the bytes that actually exist are handed back. Returning
			// the full buffer would feed the parser manufactured zeroes and
			// let a truncated fmt chunk parse as though it were complete.
			return buf[:n], nil
		}
		return nil, err
	}
	return buf, nil
}

// skipChunk discards a chunk payload and its pad byte.
func skipChunk(br *bufio.Reader, size uint32) error {
	if err := discardN(br, int64(size)); err != nil {
		return err
	}
	return skipPad(br, size)
}

// skipPad consumes the pad byte that follows an odd-sized chunk, if it is
// there.
//
// Some writers omit it. Peek tells the two layouts apart without seeking: a
// genuine pad byte is 0x00, which is not printable, whereas an unpadded chunk
// puts a printable identifier right here. So the presence of a plausible
// identifier at this offset means the pad was omitted and consuming a byte
// would desync the walk. This is how ffmpeg disambiguates the same case.
func skipPad(br *bufio.Reader, size uint32) error {
	if size%2 == 0 {
		return nil
	}
	next, err := br.Peek(4)
	if err != nil && len(next) < 4 {
		// Fewer than four bytes remain, so no further chunk will be parsed
		// and the pad byte cannot matter either way.
		return nil //nolint:nilerr // running out of input here is not an error.
	}
	if plausibleFourCC(next) {
		return nil
	}
	if _, derr := br.Discard(1); derr != nil && !errors.Is(derr, io.EOF) {
		return derr
	}
	return nil
}

// discardN skips n bytes, tolerating a stream that ends first. It takes an
// int64 because a 32-bit chunk size overflows int on a 32-bit target, and it
// loops because bufio.Reader.Discard takes an int.
func discardN(br *bufio.Reader, n int64) error {
	const step = int64(1) << 30
	for n > 0 {
		got, err := br.Discard(int(min(n, step)))
		n -= int64(got)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

// ds64Info is a parsed ds64 chunk.
type ds64Info struct {
	riffSize    uint64
	dataSize    uint64
	sampleCount uint64
}

// parseDS64 decodes a ds64 payload. The optional chunk-size table is parsed
// only far enough to be skipped, since this package writes no chunk that could
// need one.
func parseDS64(b []byte) (ds64Info, error) {
	if len(b) < DS64PayloadSize {
		return ds64Info{}, fmt.Errorf(
			"go-wav/internal/riff: %w: ds64 chunk is %d bytes, want at least %d",
			wav.ErrCorruptStream, len(b), DS64PayloadSize)
	}
	return ds64Info{
		riffSize:    binary.LittleEndian.Uint64(b[0:8]),
		dataSize:    binary.LittleEndian.Uint64(b[8:16]),
		sampleCount: binary.LittleEndian.Uint64(b[16:24]),
	}, nil
}
