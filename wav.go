package wav

import "time"

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
	ContainerBW64
)

// String returns the four-character magic of the container.
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

// Sized reports whether the container carries 64-bit sizes in a ds64 chunk.
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

// String returns a short name for the sample format.
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

	// TotalFrames is the number of inter-channel frames in the stream. It is
	// 0 when the count is not known, which happens for a stream whose data
	// chunk size was absent or unreadable.
	TotalFrames uint64
}

// BytesPerSample is the storage width of a single-channel sample in bytes.
func (si StreamInfo) BytesPerSample() int {
	return (si.BitDepth + 7) / 8
}

// BytesPerFrame is the storage width of one inter-channel frame in bytes. It
// is the value a WAVE fmt chunk records as nBlockAlign.
func (si StreamInfo) BytesPerFrame() int {
	return si.BytesPerSample() * si.Channels
}

// Duration is the length of the stream. It is 0 when TotalFrames or SampleRate
// is 0, so a stream of unknown length reports 0 rather than a wrong answer.
func (si StreamInfo) Duration() time.Duration {
	if si.TotalFrames == 0 || si.SampleRate <= 0 {
		return 0
	}
	const nsPerSecond = int64(time.Second)
	//nolint:gosec // G115: TotalFrames is bounded by the ds64 64-bit data size.
	frames := int64(si.TotalFrames)
	whole := frames / int64(si.SampleRate)
	rem := frames % int64(si.SampleRate)
	return time.Duration(whole*nsPerSecond + rem*nsPerSecond/int64(si.SampleRate))
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
