// Package pcm reads and writes WAV audio as interleaved little-endian PCM.
//
// It is the entry point of go-wav, and it presents the same shape as the pcm
// packages of the sibling libraries go-flac, go-opus and go-aac: a flat Config
// struct for the encoder, variadic options for the decoder, New plus Reset
// pairs so both can be pooled, and one-shot [EncodeInterleaved] and
// [DecodeInterleaved] entry points for callers who already hold the whole
// buffer.
//
//	import wavpcm "github.com/tphakala/go-wav/pcm"
//
// The package name deliberately collides with its siblings, so a program using
// more than one of them imports each under an alias.
//
// # Encoding
//
// The encoder is an [io.WriteCloser] over interleaved samples in the format
// named by [Config]. Any chunk size is accepted; a sub-sample remainder is
// carried internally until the next Write or Close.
//
//	cfg := wavpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
//
//	// One shot, drawn from a pool and safe for concurrent use:
//	err := wavpcm.EncodeInterleaved(w, cfg, samples)
//
//	// Or streaming:
//	e, err := wavpcm.NewEncoder(w, cfg)
//	_, err = io.Copy(e, src)
//	err = e.Close()
//
// A plain [io.Writer] is always accepted. When the sink also implements
// [io.WriteSeeker] the encoder patches the header at Close, which is what
// allows it to upgrade a stream to RF64 once it outgrows the 4 GiB limit of
// the 32-bit RIFF size fields. See [RF64Mode] for the policy and the
// package documentation of Encoder.Close for what happens on each kind of sink.
//
// RIFF and RF64 are the whole of the container choice: an encoder writes one or
// the other. BW64 is read but never written, because the ADM metadata in its
// axml and chna chunks is what makes a file BW64 rather than RF64 and this
// library writes no such chunk. See [github.com/tphakala/go-wav.ContainerBW64].
//
// The encoder never writes a size field it knows to be wrong. Where a correct
// size cannot be produced it reports [github.com/tphakala/go-wav.ErrTooLarge].
//
// # Decoding
//
// The decoder is an [io.Reader] and an [io.WriterTo] over the data chunk. By
// default it is a pass-through: Read yields the stored bytes verbatim, so
// 24-bit audio stays packed in three bytes and nothing is widened behind the
// caller's back.
//
//	d, err := wavpcm.NewDecoder(r)
//	info := d.Info()       // valid immediately
//	_, err = io.Copy(w, d)
//
// A whole file already in memory needs no reader and no copy:
//
//	info, samples, err := wavpcm.DecodeInterleaved(b)
//
// The samples it returns alias b rather than being copied out of it, which is
// the one place in the package where a returned slice is a window onto the
// caller's own memory. See [DecodeInterleaved] for what that means.
//
// Pass-through means the sample encoding varies with the file: notably, 8-bit
// data is unsigned while every wider integer depth is signed, because that is
// how WAV stores them. [WithConvertTo] normalises everything to signed integer
// PCM of a chosen width, converting float and 8-bit sources on the way.
//
// [Decoder.Info] always describes what Read yields rather than what is on disk,
// so it never disagrees with the bytes. The stored encoding remains available
// as SourceBitDepth and SourceFormat.
//
// # Concurrency
//
// An Encoder or Decoder is not safe for concurrent use. [EncodeInterleaved] and
// [DecodeInterleaved] are, because they draw from a pool. The package holds no
// mutable global state.
package pcm
