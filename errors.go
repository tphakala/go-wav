package wav

import "errors"

// Sentinel errors returned by the wav and pcm packages, testable with
// errors.Is. They live in the root package so that both layers can report the
// same values, mirroring go-flac's layout.
var (
	// ErrNotRIFF reports a stream whose first twelve bytes are not a RIFF,
	// RF64 or BW64 WAVE header. It means "this is not a WAV file at all",
	// as distinct from ErrCorruptStream, which means "this is a WAV file and
	// it is broken".
	ErrNotRIFF = errors.New("go-wav: not a RIFF, RF64 or BW64 stream")

	// ErrCorruptStream reports a malformed stream: a chunk header that runs
	// past the end of its parent, a fmt chunk shorter than 16 bytes, a
	// missing fmt or data chunk, or an RF64 stream with no ds64 chunk. It is
	// reserved for damage the reader cannot work around; the tolerated
	// real-world deviations documented on the decoder do not produce it.
	ErrCorruptStream = errors.New("go-wav: corrupt stream")

	// ErrUnsupported reports a well-formed stream this package will not
	// decode, or an output this package will not write: a compressed or
	// companded format tag such as A-law, mu-law or ADPCM, a bit depth
	// outside the supported set, a float stream at a width other than 32 or
	// 64 bits, or a container that is read but never written, which is BW64.
	// The text is deliberately container-neutral, because the sentinel covers
	// more than the sample format.
	ErrUnsupported = errors.New("go-wav: unsupported format")

	// ErrEncoderClosed is returned by Encoder.Write and Encoder.Close after
	// Close has been called, and by methods on an encoder left uninitialised
	// by a zero value or a failed Reset.
	ErrEncoderClosed = errors.New("go-wav: encoder is closed")

	// ErrTooLarge reports a stream that has outgrown the 4 GiB limit of the
	// 32-bit RIFF size fields at a point where no RF64 upgrade is possible:
	// the policy forbids it, or the sink cannot seek and the frame count was
	// not declared up front. The encoder returns this instead of writing a
	// size field it knows to be wrong.
	ErrTooLarge = errors.New("go-wav: stream exceeds the 4 GiB RIFF limit and RF64 is unavailable")

	// ErrSeekUnsupported reports a seek requested on a source that does not
	// implement io.Seeker.
	ErrSeekUnsupported = errors.New("go-wav: seek unsupported (source is not an io.Seeker)")
)
