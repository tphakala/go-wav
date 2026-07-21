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
// Only RF64 is emitted. BW64 is parsed and reported, but [BuildHeader] rejects
// it, because the ADM metadata in the axml and chna chunks is the whole reason
// a file is BW64 rather than RF64 and this package writes no such chunk.
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
// The fmt chunk's own fields are the exception, being small by construction:
// its 16-bit fields land in an int on any platform. The sample rate is 32 bits
// and does not, so it is checked before it is narrowed and a declaration above
// [MaxSampleRate] is refused rather than wrapped into a negative rate. The
// writer applies that same ceiling, so the rate in a header this package emits
// is always one it will read back.
//
// # Tolerance
//
// Real WAV files violate the specification constantly, so the reader accepts a
// missing pad byte, a data size of zero or 0xFFFFFFFF, a declared size that
// runs past the end of the file, trailing bytes after the audio, chunks in any
// order before fmt and data, and unknown chunks anywhere. A declared frame
// count above the ceiling the reader will believe, or a fact chunk holding the
// supersession sentinel, is reported as unknown rather than repeated to the
// caller. Two fmt fields get a milder form of the same latitude: a declared
// nBlockAlign that disagrees with the channel count and sample width is
// replaced by the derived value, and a ValidBits wider than its container is
// reported as absent.
//
// The fields that describe what the samples ARE get none, because a stream
// whose shape is unreadable cannot be decoded at all: zero channels, a zero
// sample rate, and a rate above [MaxSampleRate] are each refused outright
// rather than reported as unknown, since none of them has a way to say
// "unknown" that a caller could act on. It does not guess a sample format
// either: a stream it cannot decode is reported, never reinterpreted, and a
// fmt chunk naming A-law or mu-law at a width other than the 8 bits G.711
// defines is refused rather than read as though the depth field were the
// mistake.
//
// The two companding laws are parsed but, like BW64, never written:
// [BuildHeader] rejects them, because nothing in this library compands linear
// samples and the only header it could write would announce a law over a
// payload that carries none.
package riff
