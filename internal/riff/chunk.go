package riff

import (
	"encoding/binary"
	"fmt"

	wav "github.com/tphakala/go-wav"
)

// Chunk identifiers and the form type, as they appear on the wire.
const (
	idRIFF = "RIFF"
	idRF64 = "RF64"
	idBW64 = "BW64"
	idWAVE = "WAVE"
	idFmt  = "fmt "
	idData = "data"
	idDS64 = "ds64"
	idFact = "fact"
	idJUNK = "JUNK"
)

// Structural sizes fixed by the format.
const (
	// FileHeaderSize is the magic, the 32-bit size and the form type.
	FileHeaderSize = 12
	// ChunkHeaderSize is a four-byte identifier plus a 32-bit size.
	ChunkHeaderSize = 8
	// DS64PayloadSize is the ds64 payload with no chunk-size table:
	// riffSize, dataSize and sampleCount as 64-bit values, then a 32-bit
	// table length of zero.
	DS64PayloadSize = 28
	// DS64ChunkSize is a complete ds64 chunk on the wire, and therefore the
	// size of the JUNK chunk reserved to be overwritten by one.
	DS64ChunkSize = ChunkHeaderSize + DS64PayloadSize

	// factPayloadSize is the fact payload this package writes: a single
	// 32-bit frame count.
	factPayloadSize = 4

	// fmtSizePCM, fmtSizeExtended and fmtSizeExtensible are the three valid
	// fmt payload sizes.
	fmtSizePCM         = 16
	fmtSizeExtended    = 18
	fmtSizeExtensible  = 40
	fmtExtensibleCBSze = 22
)

// sizeUnknown marks a chunk size the reader could not determine, meaning the
// caller should read to the end of the stream.
const sizeUnknown int64 = -1

// maxUint32 is the largest value a 32-bit RIFF size field can hold, and
// therefore the point past which a stream needs RF64.
const maxUint32 = int64(1)<<32 - 1

// maxDataSize is the largest audio payload the reader will believe a header
// when it declares one. It is far above any real recording, and it keeps a
// whole bit of headroom under math.MaxInt64, so a length or a frame count that
// passes it still converts to a signed 64-bit value without wrapping. That
// separates a plausible declaration from a corrupt or hostile one without
// having to guess where real files stop.
//
// It is a policy of this reader rather than a limit of the format, which can
// describe more than the reader will accept on a header's word alone.
//
// Two kinds of declaration are checked against it: a ds64 data size, directly,
// and a declared frame count (a ds64 sampleCount or a fact chunk count),
// against the ceiling divided by the frame width, since a count and a length
// describe the same bytes in different units. Where the frame width is not
// known there is nothing to divide by and the bare ceiling applies. The 32-bit
// size fields need no ceiling, sitting far below this one by construction, and
// the auxiliary chunk payloads the reader buffers are bounded separately by
// maxChunkPayload.
const maxDataSize uint64 = 1 << 62

// sentinel32 is the value RF64 writes into the 32-bit size fields it has
// superseded.
const sentinel32 uint32 = 0xFFFFFFFF

// putFourCC writes a four-character identifier at the start of b.
func putFourCC(b []byte, id string) {
	copy(b[:4], id)
}

// putU16 writes a little-endian 16-bit value.
func putU16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
}

// putU32 writes a little-endian 32-bit value.
func putU32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
}

// putU64 writes a little-endian 64-bit value.
func putU64(b []byte, v uint64) {
	binary.LittleEndian.PutUint64(b, v)
}

// u32 narrows a non-negative int64 to uint32, reporting wav.ErrTooLarge rather
// than wrapping. Every 32-bit size field in this package goes through it, which
// is what prevents the silent truncation the library exists to eliminate.
func u32(op string, v int64) (uint32, error) {
	if v < 0 || v > maxUint32 {
		return 0, fmt.Errorf("go-wav/internal/riff: %s: %w: %d bytes does not fit a 32-bit size field",
			op, wav.ErrTooLarge, v)
	}
	return uint32(v), nil
}

// padded returns size rounded up to the even boundary the format requires. The
// pad byte is not part of the size field, only of the bytes on disk.
func padded(size int64) int64 {
	if size%2 != 0 {
		return size + 1
	}
	return size
}

// plausibleFourCC reports whether b looks like a chunk identifier. It is the
// test that lets the reader tell a missing pad byte from a real one without
// seeking: at the unpadded offset, either the next four bytes already form an
// identifier or they do not.
//
// Identifiers are printable ASCII in practice, and every identifier this
// package cares about is. Accepting the whole printable range keeps unknown
// chunks walkable.
func plausibleFourCC(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	for _, c := range b[:4] {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}
