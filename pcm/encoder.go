package pcm

import (
	"errors"
	"fmt"
	"io"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/riff"
)

var _ io.WriteCloser = (*Encoder)(nil)

// errNilWriter reports a nil sink. A nil io.Writer is a programming error
// rather than a condition a caller branches on, so it is deliberately not an
// exported sentinel, matching the sibling libraries.
var errNilWriter = errors.New("go-wav/pcm: nil writer")

// Encoder writes interleaved samples to a WAVE stream.
//
// It implements io.WriteCloser. Write accepts any chunk size and carries a
// sub-sample remainder internally; Close finishes the stream and must be
// called for the header to be correct.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	w    io.Writer
	ws   io.WriteSeeker // non-nil when w can seek
	cfg  Config
	lay  *riff.Layout
	cont wav.Container

	// written counts audio bytes handed to the sink.
	written int64
	// carry holds a partial sample between calls to Write.
	carry []byte

	// headerWritten guards against emitting the header twice.
	headerWritten bool
	closed        bool
	// err latches the first failure so every later call reports it.
	err error
}

// NewEncoder returns an Encoder writing a WAVE stream to w.
//
// The header is emitted immediately, so a failure to write reaches the caller
// here rather than at the first Write.
//
// When w implements io.WriteSeeker the encoder can patch the header at Close,
// which is what lets [RF64Auto] upgrade a stream that outgrows 4 GiB. A plain
// io.Writer is fully supported; see [Config.TotalFrames] for how to describe a
// large stream to a sink that cannot seek.
func NewEncoder(w io.Writer, cfg Config) (*Encoder, error) {
	e := &Encoder{}
	if err := e.Reset(w, cfg); err != nil {
		return nil, err
	}
	return e, nil
}

// Reset rebinds the encoder to a new sink and configuration, discarding any
// state from a previous stream. It is the pooling entry point, and NewEncoder
// is a thin wrapper over it.
//
// It may be called on a closed encoder.
func (e *Encoder) Reset(w io.Writer, cfg Config) error {
	return e.reset("Reset", w, cfg)
}

func (e *Encoder) reset(op string, w io.Writer, cfg Config) error {
	if w == nil {
		return fmt.Errorf("go-wav/pcm: %s: %w", op, errNilWriter)
	}
	if err := cfg.validate(op); err != nil {
		return err
	}

	ws, _ := w.(io.WriteSeeker)
	container, reserve, err := plan(op, cfg, ws != nil)
	if err != nil {
		return err
	}

	*e = Encoder{
		w:     w,
		ws:    ws,
		cfg:   cfg,
		cont:  container,
		carry: e.carry[:0],
	}

	// A declared frame count means the sizes are known now, so they can be
	// written correctly even into a sink that will never be seekable.
	var dataSize int64
	if cfg.TotalFrames > 0 {
		dataSize, err = declaredDataSize(op, cfg)
		if err != nil {
			return err
		}
	}

	lay, err := riff.BuildHeader(riff.HeaderConfig{
		Format:      formatOf(cfg),
		Container:   container,
		ReserveDS64: reserve,
		DataSize:    dataSize,
		Frames:      cfg.TotalFrames,
	})
	if err != nil {
		return err
	}
	e.lay = lay

	if _, err := e.w.Write(lay.Bytes); err != nil {
		e.err = err
		return err
	}
	e.headerWritten = true
	return nil
}

// plan resolves the container to write and whether to reserve ds64 space,
// rejecting the one combination that cannot produce a correct file.
func plan(op string, cfg Config, seekable bool) (wav.Container, bool, error) {
	switch cfg.RF64 {
	case RF64Always:
		if !seekable && cfg.TotalFrames == 0 {
			return 0, false, fmt.Errorf(
				"go-wav/pcm: %s: RF64Always needs either a seekable sink or Config.TotalFrames, "+
					"because the 64-bit sizes are written before any audio and can never be patched", op)
		}
		return wav.ContainerRF64, false, nil

	case RF64Never:
		return wav.ContainerRIFF, false, nil

	case RF64Auto:
		// A declared frame count settles the question up front: emit RF64
		// only if the stream genuinely will not fit.
		if cfg.TotalFrames > 0 {
			size, err := declaredDataSize(op, cfg)
			if err != nil {
				return 0, false, err
			}
			if !fitsPlainRIFF(cfg, size) {
				return wav.ContainerRF64, false, nil
			}
			return wav.ContainerRIFF, false, nil
		}
		// Otherwise stay RIFF and reserve room to become RF64 later, which
		// is only useful when the sink can seek.
		return wav.ContainerRIFF, seekable, nil

	default:
		return 0, false, fmt.Errorf("go-wav/pcm: %s: unknown RF64 mode %d", op, cfg.RF64)
	}
}

// fitsPlainRIFF reports whether a data chunk of the given size can be described
// by 32-bit size fields, allowing for the header that precedes it.
func fitsPlainRIFF(cfg Config, dataSize int64) bool {
	probe, err := riff.BuildHeader(riff.HeaderConfig{
		Format:    formatOf(cfg),
		Container: wav.ContainerRIFF,
	})
	if err != nil {
		return false
	}
	return riff.FitsRIFF(probe, dataSize)
}

// declaredDataSize converts a declared frame count to a byte count, reporting
// wav.ErrTooLarge rather than overflowing.
func declaredDataSize(op string, cfg Config) (int64, error) {
	const maxInt64 = int64(^uint64(0) >> 1)
	perFrame := cfg.bytesPerFrame()
	if perFrame <= 0 {
		return 0, fmt.Errorf("go-wav/pcm: %s: frame size is not positive", op)
	}
	//nolint:gosec // G115: guarded immediately below.
	if cfg.TotalFrames > uint64(maxInt64)/uint64(perFrame) {
		return 0, fmt.Errorf("go-wav/pcm: %s: %w: %d frames of %d bytes overflows a byte count",
			op, wav.ErrTooLarge, cfg.TotalFrames, perFrame)
	}
	//nolint:gosec // G115: bounded by the check above.
	return int64(cfg.TotalFrames) * perFrame, nil
}

// formatOf projects a Config onto the container layer's format description.
func formatOf(cfg Config) riff.Format {
	return riff.Format{
		SampleRate:  cfg.SampleRate,
		Channels:    cfg.Channels,
		BitDepth:    cfg.BitDepth,
		Format:      cfg.Format,
		Extensible:  cfg.Extensible,
		ChannelMask: cfg.ChannelMask,
	}
}

// Write encodes interleaved samples.
//
// The bytes are passed through unchanged, so they must already be in the format
// named by the Config: little-endian, unsigned at 8 bits and signed above,
// or IEEE float where the Config says so. Any chunk size is accepted; a
// trailing partial sample is carried until the next call or until Close.
//
// It returns len(p) on success. Once the encoder has failed, every later call
// reports the same error until Reset.
func (e *Encoder) Write(p []byte) (int, error) {
	switch {
	case e.err != nil:
		return 0, e.err
	case e.closed:
		return 0, wav.ErrEncoderClosed
	case e.w == nil:
		return 0, wav.ErrEncoderClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	// The common case is an aligned buffer with nothing carried over, which
	// hands the caller's slice straight to the sink without copying.
	if len(e.carry) == 0 {
		n, err := e.writeAligned(p)
		return n, err
	}

	// Complete the carried sample first.
	width := int(e.cfg.bytesPerSample())
	need := width - len(e.carry)
	if len(p) < need {
		e.carry = append(e.carry, p...)
		return len(p), nil
	}
	e.carry = append(e.carry, p[:need]...)
	if err := e.emit(e.carry); err != nil {
		return 0, err
	}
	e.carry = e.carry[:0]

	if _, err := e.writeAligned(p[need:]); err != nil {
		return need, err
	}
	return len(p), nil
}

// writeAligned emits the whole samples of p and carries any remainder.
func (e *Encoder) writeAligned(p []byte) (int, error) {
	width := int(e.cfg.bytesPerSample())
	whole := len(p) - len(p)%width
	if whole > 0 {
		if err := e.emit(p[:whole]); err != nil {
			return 0, err
		}
	}
	if rest := p[whole:]; len(rest) > 0 {
		e.carry = append(e.carry, rest...)
	}
	return len(p), nil
}

// emit writes audio bytes to the sink and accounts for them, refusing to
// produce a stream whose size cannot be described.
func (e *Encoder) emit(b []byte) error {
	if err := e.checkCapacity(int64(len(b))); err != nil {
		return e.fail(err)
	}
	n, err := e.w.Write(b)
	e.written += int64(n)
	if err != nil {
		return e.fail(err)
	}
	return nil
}

// checkCapacity reports whether the stream can still be described once n more
// bytes are added. This is what replaces a silently truncated 32-bit size
// field with an error.
func (e *Encoder) checkCapacity(n int64) error {
	next := e.written + n
	if next < 0 {
		return fmt.Errorf("go-wav/pcm: %w: audio length overflows a signed 64-bit byte count",
			wav.ErrTooLarge)
	}
	if e.cont.Sized64() {
		return nil
	}
	if riff.FitsRIFF(e.lay, next) {
		return nil
	}
	// The stream has outgrown plain RIFF. Auto mode can rescue it only if
	// the sink can seek, since the ds64 must go at the front.
	if e.cfg.RF64 == RF64Auto && e.ws != nil && e.lay.DS64Offset >= 0 {
		e.cont = wav.ContainerRF64
		return nil
	}
	return fmt.Errorf("go-wav/pcm: %w: %d bytes of audio at %s, and the header cannot be upgraded",
		wav.ErrTooLarge, next, e.rescueHint())
}

// rescueHint explains why an upgrade was unavailable, so the error tells the
// caller what to change.
func (e *Encoder) rescueHint() string {
	switch {
	case e.cfg.RF64 == RF64Never:
		return "RF64Never"
	case e.ws == nil:
		return "a sink that cannot seek and no Config.TotalFrames"
	default:
		return "a header with no reserved ds64 space"
	}
}

// Close finishes the stream.
//
// It flushes a carried partial sample as an error, writes the pad byte an
// odd-length data chunk requires, and finalises the header. On a seekable sink
// that means patching the sizes, and upgrading the reserved JUNK chunk to a
// ds64 when the stream outgrew plain RIFF. On a sink that cannot seek the
// header was already final, and Close verifies that the caller wrote as many
// frames as it declared.
//
// Close is idempotent: it stores its result and returns the same value on every
// later call.
func (e *Encoder) Close() error {
	if e.closed {
		return e.err
	}
	if e.w == nil {
		return wav.ErrEncoderClosed
	}
	e.closed = true
	if e.err != nil {
		return e.err
	}

	if len(e.carry) != 0 {
		return e.fail(fmt.Errorf(
			"go-wav/pcm: Close: %d trailing bytes are not a whole sample of %d bytes",
			len(e.carry), e.cfg.bytesPerSample()))
	}

	// The data chunk is padded to an even length on disk, though the pad
	// byte is not counted in its size field.
	if e.written%2 != 0 {
		if _, err := e.w.Write([]byte{0}); err != nil {
			return e.fail(err)
		}
	}

	frames := e.frames()
	if e.cfg.TotalFrames > 0 && frames != e.cfg.TotalFrames {
		return e.fail(fmt.Errorf(
			"go-wav/pcm: Close: wrote %d frames but Config.TotalFrames declared %d",
			frames, e.cfg.TotalFrames))
	}

	if e.ws == nil {
		// Nothing to patch; the header written up front is already final.
		return nil
	}

	var err error
	if e.cont.Sized64() && e.lay.DS64Offset >= 0 && e.cfg.RF64 == RF64Auto {
		err = riff.UpgradeToRF64(e.ws, e.lay, e.cont, e.written, frames)
	} else {
		err = riff.PatchSizes(e.ws, e.lay, e.cont, e.written, frames)
	}
	if err != nil {
		return e.fail(err)
	}
	return nil
}

// frames is the number of whole inter-channel frames written.
func (e *Encoder) frames() uint64 {
	per := e.cfg.bytesPerFrame()
	if per <= 0 {
		return 0
	}
	//nolint:gosec // G115: written is non-negative.
	return uint64(e.written / per)
}

// StreamInfo describes the stream written so far.
func (e *Encoder) StreamInfo() wav.StreamInfo {
	return e.cfg.streamInfo(e.cont, e.frames())
}

// fail latches an error so that every later call reports it.
func (e *Encoder) fail(err error) error {
	if e.err == nil {
		e.err = err
	}
	return e.err
}
