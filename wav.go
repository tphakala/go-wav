package wav

import (
	"math"
	"time"
)

// Version is the module version.
const Version = "0.1.0"

// headerSize is the number of bytes [Sniff] needs: the four-byte magic, the
// 32-bit size that follows it, and the four-byte form type.
const headerSize = 12

// Container identifies the RIFF flavour of a stream.
type Container int

const (
	// ContainerRIFF is a plain RIFF WAVE stream with 32-bit size fields. It
	// cannot describe 4 GiB or more of audio.
	ContainerRIFF Container = iota
	// ContainerRF64 is an RF64 stream as defined by EBU Tech 3306: the magic
	// is "RF64", the 32-bit sizes hold 0xFFFFFFFF, and a ds64 chunk carries
	// the real 64-bit values.
	ContainerRF64
	// ContainerBW64 is a BW64 stream as defined by ITU-R BS.2088. It is
	// structurally identical to RF64 and differs only in its magic.
	//
	// It is read but never written, so it is reported in StreamInfo of a
	// decoder and never of an encoder. A BW64 file exists to carry ADM
	// metadata in its axml and chna chunks, and this library carries
	// neither, so writing the magic alone would hand back an RF64 file under
	// a name that promises metadata it does not hold: strict tooling may
	// reject it, and a caller who asked for BW64 would reasonably expect the
	// metadata that is the point of the format. An encoder therefore emits
	// RIFF or RF64, chosen by pcm.RF64Mode.
	ContainerBW64
)

// String returns the four-character magic of the container, or the string
// "unknown" for a value outside the declared set.
func (c Container) String() string {
	switch c {
	case ContainerRIFF:
		return "RIFF"
	case ContainerRF64:
		return "RF64"
	case ContainerBW64:
		return "BW64"
	default:
		return "unknown"
	}
}

// Sized64 reports whether the container carries 64-bit sizes in a ds64 chunk.
func (c Container) Sized64() bool {
	return c == ContainerRF64 || c == ContainerBW64
}

// SampleFormat identifies how samples in the data chunk are encoded.
type SampleFormat int

const (
	// SampleFormatPCM is integer PCM: unsigned with a midpoint of 128 at 8
	// bits, signed two's complement at 16, 24 and 32 bits.
	SampleFormatPCM SampleFormat = iota
	// SampleFormatFloat is IEEE 754 floating point at 32 or 64 bits, with a
	// nominal full scale of [-1, +1].
	SampleFormatFloat
)

// String returns a short lower-case name for the sample format: "pcm" for
// integer PCM, "float" for IEEE 754 floating point, and "unknown" for a value
// outside the declared set.
func (f SampleFormat) String() string {
	switch f {
	case SampleFormatPCM:
		return "pcm"
	case SampleFormatFloat:
		return "float"
	default:
		return "unknown"
	}
}

// StreamInfo describes a WAVE stream. A Decoder reports the properties of the
// stream it is reading; an Encoder reports the properties of the stream it is
// writing. It mirrors flac.StreamInfo in the sibling go-flac library.
type StreamInfo struct {
	// SampleRate is the number of samples per second per channel.
	SampleRate int

	// Channels is the interleaved channel count.
	Channels int

	// BitDepth is the storage width in bits of one sample as the caller sees
	// it: 8, 16, 24 or 32 for integer PCM, 32 or 64 for float. It describes
	// the bytes Decoder.Read yields, so under a conversion option it is the
	// converted width, not the width stored in the file. SourceBitDepth
	// reports the latter.
	//
	// It is the container width, which under WAVE_FORMAT_EXTENSIBLE may
	// exceed the meaningful width reported by ValidBits.
	BitDepth int

	// SourceBitDepth is the storage width in bits of one sample as it is
	// encoded in the file. It differs from BitDepth only when the decoder
	// was asked to convert; otherwise the two are equal.
	SourceBitDepth int

	// ValidBits is the number of meaningful bits per sample declared by an
	// extensible fmt chunk, for sources such as 20-bit audio stored in a
	// 24-bit container. It is 0 when the stream did not declare one, in
	// which case every bit of BitDepth is meaningful.
	ValidBits int

	// Format is the sample encoding as the caller sees it. Like BitDepth it
	// describes the bytes Decoder.Read yields, so a float file being
	// converted to integer reports SampleFormatPCM here and
	// SampleFormatFloat in SourceFormat.
	Format SampleFormat

	// SourceFormat is the sample encoding as it appears in the file. It
	// differs from Format only when the decoder was asked to convert;
	// otherwise the two are equal.
	SourceFormat SampleFormat

	// Container is the RIFF flavour the stream was read from or written as.
	Container Container

	// Extensible reports whether the fmt chunk used WAVE_FORMAT_EXTENSIBLE
	// rather than a bare format tag.
	Extensible bool

	// ChannelMask is the dwChannelMask speaker assignment from an extensible
	// fmt chunk. It is 0 when the stream did not declare one.
	ChannelMask uint32

	// TotalFrames is the number of inter-channel frames in the stream. When
	// the data chunk size is known the count is derived from it, so it
	// describes bytes that are present. When the size is absent or
	// unreadable the count comes instead from whatever the header declared,
	// a ds64 sampleCount or a fact chunk, which the reader passes through
	// without checking it against the audio that follows: a declared count
	// can therefore be any 64-bit value, up to and including
	// [math.MaxUint64], and need not match the bytes the stream actually
	// carries. It is 0 only when no source offered a count at all.
	TotalFrames uint64
}

// BytesPerSample is the storage width of a single-channel sample in bytes.
//
//nolint:gocritic // a value receiver is the right shape for a value type callers receive by value.
func (si StreamInfo) BytesPerSample() int {
	return (si.BitDepth + 7) / 8
}

// BytesPerFrame is the storage width of one inter-channel frame in bytes. It
// is the value a WAVE fmt chunk records as nBlockAlign.
//
//nolint:gocritic // a value receiver is the right shape for a value type callers receive by value.
func (si StreamInfo) BytesPerFrame() int {
	return si.BytesPerSample() * si.Channels
}

// Duration is the length of the stream, or 0 when that length cannot be stated
// as a [time.Duration]. A caller that sees 0 knows the length is unavailable;
// an unchecked computation would instead hand back a wrapped, meaningless
// number. Such a number is as readily positive as negative, and the positive
// ones are the more dangerous, because nothing about a plausible-looking length
// invites a second look.
//
// A TotalFrames of 0 means the stream carried no frame count at all, and a
// SampleRate of 0 or less cannot divide anything. The remaining zero cases are
// arithmetic ceilings, and they are reachable because TotalFrames is not always
// derived from bytes that exist: when the data chunk size is unknown the reader
// falls back to a declared count, and a ds64 sampleCount is a raw 64-bit field
// it passes through unchecked. Each step below therefore rejects what it cannot
// carry. The conversion to int64 rejects a count above [math.MaxInt64]. The
// whole-seconds term rejects a count whose whole seconds alone would pass
// math.MaxInt64 nanoseconds, which is the ceiling time.Duration itself has,
// about 292 years. The final addition rejects the few counts that clear both
// and overflow only once the sub-second remainder is added. At 48 kHz the
// largest frame count that survives all three is 442721857769029.
//
// The remainder term carries a bound of its own: rem * nsPerSecond overflows
// for a sample rate above math.MaxInt64/nsPerSecond, roughly 9.22 GHz, so such
// a rate is rejected outright. This is the one bound that gives up a length it
// could in principle have represented, and it is deliberate. No file can reach
// it, because a fmt chunk stores the sample rate in 32 bits, so the only way in
// is a hand-built StreamInfo, and rejecting those costs less than the wider
// arithmetic serving them would take.
//
// The whole-seconds-plus-remainder split is what buys the range in between. A
// naive frames * time.Second wraps once the frame count passes
// math.MaxInt64/nsPerSecond, about 53 hours of audio at 48 kHz; splitting the
// multiplication off the whole seconds pushes that out to the full 292 years.
//
//nolint:gocritic // a value receiver is the right shape for a value type callers receive by value.
func (si StreamInfo) Duration() time.Duration {
	const nsPerSecond = int64(time.Second)
	if si.TotalFrames == 0 || si.TotalFrames > math.MaxInt64 ||
		si.SampleRate <= 0 || int64(si.SampleRate) > math.MaxInt64/nsPerSecond {
		return 0
	}
	//nolint:gosec // G115: the guard above rejects every TotalFrames above math.MaxInt64.
	frames, rate := int64(si.TotalFrames), int64(si.SampleRate)
	whole, rem := frames/rate, frames%rate
	if whole > math.MaxInt64/nsPerSecond {
		return 0
	}
	// rem is below rate, and rate is at most math.MaxInt64/nsPerSecond, so
	// rem*nsPerSecond cannot overflow and remNs is below one second.
	ns, remNs := whole*nsPerSecond, rem*nsPerSecond/rate
	if ns > math.MaxInt64-remNs {
		return 0
	}
	return time.Duration(ns + remNs)
}

// Sniff reports whether b begins with a RIFF, RF64 or BW64 WAVE header. It
// reads at most the first twelve bytes, needs no allocation, and returns false
// rather than panicking on a short slice.
//
// It exists so that callers dispatching on file type do not have to hand-roll
// a magic check that forgets RF64 and BW64.
func Sniff(b []byte) bool {
	_, ok := sniffContainer(b)
	return ok
}

// sniffContainer reports the container flavour of a header, and whether the
// header is a WAVE header at all.
func sniffContainer(b []byte) (Container, bool) {
	if len(b) < headerSize {
		return 0, false
	}
	if string(b[8:12]) != "WAVE" {
		return 0, false
	}
	switch string(b[0:4]) {
	case "RIFF":
		return ContainerRIFF, true
	case "RF64":
		return ContainerRF64, true
	case "BW64":
		return ContainerBW64, true
	default:
		return 0, false
	}
}
