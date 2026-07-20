// Package riff implements the container half of a WAV file: the RIFF chunk
// structure, the fmt chunk, and the ds64 mechanics that lift RF64 and BW64 past
// the 4 GiB ceiling of a plain RIFF stream.
//
// It knows nothing about sample values. Decoding a byte into an amplitude is
// the job of the internal/sample package, and the streaming API callers use
// lives in the pcm package. This package walks chunks, turns a fmt chunk into a
// [github.com/tphakala/go-wav.StreamInfo], and emits and patches headers.
//
// # Wire format
//
// Every integer is little-endian. A chunk is a four-byte ASCII identifier, a
// 32-bit payload size, then that many payload bytes, then a single 0x00 pad
// byte when the size is odd. The pad byte is not counted in the size field,
// which is the single most common source of off-by-one bugs in RIFF parsers.
// A file opens with a twelve-byte header: the magic "RIFF", "RF64" or "BW64",
// a 32-bit size, and the form type "WAVE".
//
// # Sizes wider than 32 bits
//
// RF64 (EBU Tech 3306) and BW64 (ITU-R BS.2088) share one trick. The magic
// changes, the 32-bit size fields of the file header and the data chunk are
// filled with 0xFFFFFFFF, and a ds64 chunk placed immediately after the file
// header carries the real 64-bit riffSize, dataSize and sampleCount. A writer
// that does not yet know whether it will cross 4 GiB reserves the space by
// emitting a JUNK chunk of exactly the size a ds64 would occupy; if the stream
// outgrows RIFF, the header is rewritten in place, JUNK becoming ds64. That is
// what ffmpeg calls "-rf64 auto", and [UpgradeToRF64] performs it.
//
// # Integer width
//
// Every offset, chunk size and frame count in this package is int64 or uint64.
// On a 32-bit target int is 32 bits, so an int offset silently overflows at
// 2 GiB, which in a library whose reason to exist is crossing 4 GiB would be a
// correctness bug rather than a style preference. Every narrowing to uint32 is
// guarded and reports [github.com/tphakala/go-wav.ErrTooLarge] rather than
// wrapping.
//
// # Tolerance
//
// Real WAV files violate the specification constantly, so the reader accepts a
// missing pad byte, a data size of zero or 0xFFFFFFFF, a declared size that
// runs past the end of the file, trailing bytes after the audio, chunks in any
// order before fmt and data, and unknown chunks anywhere. It does not guess a
// sample format: a stream it cannot decode is reported, never reinterpreted.
package riff
