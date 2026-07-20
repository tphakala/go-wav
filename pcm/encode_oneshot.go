package pcm

import (
	"fmt"
	"io"
	"sync"

	wav "github.com/tphakala/go-wav"
)

// encoderPool recycles encoders for the one-shot path, so that encoding many
// short clips does not allocate a fresh encoder each time.
var encoderPool = sync.Pool{New: func() any { return new(Encoder) }}

// EncodeInterleaved writes a complete WAVE stream holding pcm, which must be
// interleaved samples in the format named by cfg.
//
// It is the one-shot counterpart to [NewEncoder], for callers that already
// hold the whole buffer. Unlike an [Encoder], it is safe for concurrent use,
// because it draws its encoder from a pool.
//
// It is stricter than [Encoder.Write] in one way: pcm must be a whole number of
// inter-channel frames, since a one-shot call has no later write to complete a
// partial one. cfg.TotalFrames is ignored, because the length is known.
func EncodeInterleaved(w io.Writer, cfg Config, pcm []byte) error {
	if w == nil {
		return fmt.Errorf("go-wav/pcm: EncodeInterleaved: %w", errNilWriter)
	}
	if err := cfg.validate("EncodeInterleaved"); err != nil {
		return err
	}

	perFrame := cfg.bytesPerFrame()
	if perFrame <= 0 {
		return fmt.Errorf("go-wav/pcm: EncodeInterleaved: frame size is not positive")
	}
	if int64(len(pcm))%perFrame != 0 {
		return fmt.Errorf(
			"go-wav/pcm: EncodeInterleaved: %d bytes is not a whole number of %d byte frames",
			len(pcm), perFrame)
	}

	// The length is known, so declare it. That lets a sink which cannot seek
	// still receive a correct header, including an RF64 one.
	//nolint:gosec // G115: len is non-negative and perFrame is positive.
	cfg.TotalFrames = uint64(int64(len(pcm)) / perFrame)

	e, _ := encoderPool.Get().(*Encoder)
	defer func() {
		// Drop the caller's sink before pooling so it is not pinned.
		if resetErr := e.reset("EncodeInterleaved", io.Discard, minimalConfig(), false); resetErr == nil {
			encoderPool.Put(e)
		}
	}()

	// The payload length is known exactly, including when it is zero, so the
	// header can be final from the first byte even on a sink that cannot seek.
	if err := e.reset("EncodeInterleaved", w, cfg, true); err != nil {
		return err
	}
	if _, err := e.Write(pcm); err != nil {
		return err
	}
	return e.Close()
}

// minimalConfig is a valid configuration used only to unbind a pooled encoder
// from the caller's sink.
func minimalConfig() Config {
	return Config{SampleRate: 8000, BitDepth: 16, Channels: 1, Format: wav.SampleFormatPCM}
}
