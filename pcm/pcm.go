package pcm

import (
	"fmt"

	wav "github.com/tphakala/go-wav"
)

// RF64Mode selects how an [Encoder] handles the 4 GiB limit of the 32-bit RIFF
// size fields. The vocabulary matches ffmpeg's -rf64 flag.
type RF64Mode int

const (
	// RF64Auto writes a plain RIFF header preceded by a JUNK chunk sized to
	// hold a ds64, and rewrites that header in place as RF64 if the stream
	// outgrows 4 GiB. The rewrite needs an io.WriteSeeker; on a sink that
	// cannot seek, a stream that outgrows 4 GiB reports wav.ErrTooLarge
	// unless Config.TotalFrames declared the size up front.
	//
	// This is the zero value and the right default: small files stay plain
	// RIFF and readable by anything, large ones become valid RF64.
	RF64Auto RF64Mode = iota

	// RF64Never always writes a plain RIFF header and reports
	// wav.ErrTooLarge rather than emitting a size field it knows is wrong.
	RF64Never

	// RF64Always writes an RF64 header with a real ds64 chunk from the first
	// byte. On a sink that cannot seek it requires Config.TotalFrames,
	// because the 64-bit sizes are written before any audio and can never be
	// patched; NewEncoder rejects that combination rather than emitting a
	// file it knows to be wrong.
	RF64Always
)

// String returns a short name for the mode.
func (m RF64Mode) String() string {
	switch m {
	case RF64Auto:
		return "auto"
	case RF64Never:
		return "never"
	case RF64Always:
		return "always"
	default:
		return "unknown"
	}
}

// Config configures an [Encoder]. Every field's zero value is documented, so a
// Config carrying only the three required fields is complete and valid.
type Config struct {
	// SampleRate is the number of samples per second per channel, for
	// example 48000. Required; there is no zero default.
	SampleRate int

	// BitDepth is the storage width of one sample in bits: 8, 16, 24 or 32
	// for integer PCM, 32 or 64 for float. Required; there is no zero
	// default.
	BitDepth int

	// Channels is the interleaved channel count. Required; there is no zero
	// default.
	Channels int

	// Format selects integer or float samples. The zero value is
	// wav.SampleFormatPCM.
	//
	// A float stream also gets a fact chunk, which the format requires for
	// every non-PCM encoding.
	Format wav.SampleFormat

	// Extensible forces a WAVE_FORMAT_EXTENSIBLE fmt chunk. The zero value
	// (false) still promotes to extensible automatically wherever the format
	// requires it: more than two channels, an integer depth above 16 bits,
	// or a non-zero ChannelMask. Set it to true to force the extensible form
	// in cases where it would not otherwise be used.
	Extensible bool

	// ChannelMask is the dwChannelMask speaker assignment written in an
	// extensible fmt chunk. Zero derives a conventional mask from Channels,
	// which is what almost every caller wants.
	ChannelMask uint32

	// RF64 selects the policy for streams that outgrow the 4 GiB limit of
	// the 32-bit RIFF size fields. The zero value is RF64Auto.
	RF64 RF64Mode

	// TotalFrames declares the number of inter-channel frames the caller
	// will write. Zero means unknown.
	//
	// It matters only for a sink that cannot seek, where it is the sole way
	// to emit a correct 64-bit header, since nothing can be patched
	// afterwards. Declaring it is a promise: Close reports an error if the
	// caller wrote a different number of frames. This mirrors
	// Config.TotalSamples in the sibling go-flac library.
	TotalFrames uint64
}

// bytesPerSample is the storage width of a single-channel sample in bytes.
func (c Config) bytesPerSample() int64 {
	return int64((c.BitDepth + 7) / 8)
}

// bytesPerFrame is the storage width of one inter-channel frame in bytes, the
// value a fmt chunk records as nBlockAlign.
func (c Config) bytesPerFrame() int64 {
	return c.bytesPerSample() * int64(c.Channels)
}

// validate reports the first problem with the configuration, or nil. The op
// names the calling entry point so that NewEncoder and Reset report
// themselves, matching go-flac.
func (c Config) validate(op string) error {
	if c.SampleRate <= 0 {
		return fmt.Errorf("go-wav/pcm: %s: sample rate %d must be positive", op, c.SampleRate)
	}
	if c.Channels <= 0 {
		return fmt.Errorf("go-wav/pcm: %s: channel count %d must be positive", op, c.Channels)
	}
	if c.Channels > maxChannels {
		return fmt.Errorf("go-wav/pcm: %s: channel count %d exceeds the %d the format can express",
			op, c.Channels, maxChannels)
	}
	switch c.Format {
	case wav.SampleFormatPCM:
		switch c.BitDepth {
		case 8, 16, 24, 32:
		default:
			return fmt.Errorf("go-wav/pcm: %s: %w: integer bit depth %d (want 8, 16, 24 or 32)",
				op, wav.ErrUnsupported, c.BitDepth)
		}
	case wav.SampleFormatFloat:
		switch c.BitDepth {
		case 32, 64:
		default:
			return fmt.Errorf("go-wav/pcm: %s: %w: float bit depth %d (want 32 or 64)",
				op, wav.ErrUnsupported, c.BitDepth)
		}
	default:
		return fmt.Errorf("go-wav/pcm: %s: %w: sample format %d", op, wav.ErrUnsupported, c.Format)
	}
	switch c.RF64 {
	case RF64Auto, RF64Never, RF64Always:
	default:
		return fmt.Errorf("go-wav/pcm: %s: unknown RF64 mode %d", op, c.RF64)
	}
	return nil
}

// maxChannels is the largest channel count a WAVE fmt chunk can express, since
// nChannels is a 16-bit field.
const maxChannels = 65535

// streamInfo describes the stream this configuration produces.
func (c Config) streamInfo(container wav.Container, frames uint64) wav.StreamInfo {
	return wav.StreamInfo{
		SampleRate:     c.SampleRate,
		Channels:       c.Channels,
		BitDepth:       c.BitDepth,
		SourceBitDepth: c.BitDepth,
		Format:         c.Format,
		SourceFormat:   c.Format,
		Container:      container,
		Extensible:     c.Extensible,
		ChannelMask:    c.ChannelMask,
		TotalFrames:    frames,
	}
}
