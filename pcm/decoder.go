package pcm

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/riff"
	"github.com/tphakala/go-wav/internal/sample"
)

var (
	_ io.Reader   = (*Decoder)(nil)
	_ io.WriterTo = (*Decoder)(nil)
)

// errNilReader reports a nil source. Like a nil writer it is a programming
// error rather than a condition a caller branches on, so it is not exported.
var errNilReader = errors.New("go-wav/pcm: nil reader")

// readBufferSize is the buffered reader's window. It must exceed the largest
// header the parser inspects, and Peek needs only four bytes, so any sensible
// size works; this one just amortises syscalls.
const readBufferSize = 64 << 10

// config holds decoder options. It is unexported, which makes Option opaque to
// callers, matching go-aac.
type config struct {
	convertTo int
	// convertSet distinguishes "no conversion asked for" from an explicit
	// request for a nonsensical width, so WithConvertTo(0) is rejected
	// rather than silently ignored.
	convertSet   bool
	ignoreLength bool
}

// Option configures a [Decoder].
type Option func(*config)

// WithConvertTo makes the decoder convert every sample to signed
// little-endian integer PCM of the given bit depth, which must be 8, 16, 24
// or 32.
//
// Without it the decoder is a pass-through and Read yields the data chunk
// verbatim, which means the sample encoding varies with the file. Converting
// normalises the two traps in that: float sources become integers, scaled by
// full scale and clamped, and 8-bit sources become signed rather than the
// unsigned form WAV stores them in.
//
// [Decoder.Info] reports the converted width, since it describes what Read
// yields; the stored width remains available as SourceBitDepth.
func WithConvertTo(bitDepth int) Option {
	return func(c *config) {
		c.convertTo = bitDepth
		c.convertSet = true
	}
}

// WithIgnoreLength makes the decoder ignore the declared data chunk size and
// read until the source is exhausted.
//
// It is the recovery path for a file whose writer crashed before patching the
// header, and is equivalent to ffmpeg's -ignore_length. The decoder already
// treats a zero or all-ones size this way; this option forces the behaviour
// even when the header claims a plausible length.
func WithIgnoreLength() Option {
	return func(c *config) { c.ignoreLength = true }
}

// Decoder reads the audio of a WAVE stream.
//
// It implements io.Reader and io.WriterTo. A Decoder is not safe for
// concurrent use.
type Decoder struct {
	br   *bufio.Reader
	src  io.Reader
	cfg  config
	hdr  *riff.Header
	info wav.StreamInfo

	// remaining counts audio bytes left in the data chunk, or -1 when the
	// length is unknown and the decoder reads to EOF.
	remaining int64

	// dataStart is the absolute offset of the first audio byte, recorded
	// once while the parser still rests exactly there. Deriving it later
	// from the current position would need a byte count the decoder does not
	// have when the data chunk length is unknown. It is -1 when the source
	// cannot seek.
	dataStart int64

	// convert is non-zero when samples are converted on the way out.
	convert int
	// srcBuf stages source bytes for conversion.
	srcBuf []byte
	// outBuf holds converted bytes not yet handed to the caller, and outOff
	// is how far into it Read has got. Staging the output is what lets a
	// converting Read accept a buffer smaller than one converted sample.
	outBuf []byte
	outOff int

	err error
}

// NewDecoder reads the header of a WAVE stream from r and returns a Decoder
// positioned at the first sample.
//
// The header is parsed eagerly, so [Decoder.Info] is valid immediately and a
// malformed stream is reported here rather than at the first Read. The decoder
// never seeks unless [Decoder.SeekToFrame] is called, so a pipe works.
func NewDecoder(r io.Reader, opts ...Option) (*Decoder, error) {
	d := &Decoder{}
	if err := d.reset("NewDecoder", r, opts...); err != nil {
		return nil, err
	}
	return d, nil
}

// Reset rebinds the decoder to a new source, discarding any previous state. It
// is the pooling entry point, and NewDecoder is a thin wrapper over it.
func (d *Decoder) Reset(r io.Reader, opts ...Option) error {
	return d.reset("Reset", r, opts...)
}

func (d *Decoder) reset(op string, r io.Reader, opts ...Option) error {
	if r == nil {
		return fmt.Errorf("go-wav/pcm: %s: %w", op, errNilReader)
	}

	var cfg config
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.convertSet {
		if err := sample.Validate(wav.SampleFormatPCM, cfg.convertTo); err != nil {
			return fmt.Errorf("go-wav/pcm: %s: WithConvertTo: %w", op, err)
		}
	}

	br := d.br
	if br == nil {
		br = bufio.NewReaderSize(r, readBufferSize)
	} else {
		br.Reset(r)
	}

	hdr, err := riff.ParseHeader(br)
	if err != nil {
		// A failed Reset must not leave a pooled decoder usable against a
		// stale stream.
		*d = Decoder{br: br}
		d.err = err
		return err
	}

	// The parser stopped on the first audio byte, so this is the one moment
	// the data chunk's absolute offset can be observed directly.
	dataStart := int64(-1)
	if seeker, ok := r.(io.Seeker); ok {
		if pos, serr := seeker.Seek(0, io.SeekCurrent); serr == nil {
			dataStart = pos - int64(br.Buffered())
		}
	}

	*d = Decoder{
		br:        br,
		src:       r,
		cfg:       cfg,
		hdr:       hdr,
		info:      hdr.Info,
		remaining: hdr.DataSize,
		dataStart: dataStart,
		srcBuf:    d.srcBuf[:0],
		outBuf:    d.outBuf[:0],
	}
	if cfg.ignoreLength {
		d.remaining = -1
		d.info.TotalFrames = 0
	}

	// Info describes what Read yields, so a conversion changes it.
	if cfg.convertTo != 0 {
		d.convert = cfg.convertTo
		d.info.BitDepth = cfg.convertTo
		d.info.Format = wav.SampleFormatPCM
		d.info.ValidBits = 0
	}
	return nil
}

// Info describes the stream.
//
// It reports what [Decoder.Read] yields, not what is stored: under
// [WithConvertTo] the BitDepth and Format fields are the converted ones, and
// the stored encoding is in SourceBitDepth and SourceFormat. Info and Read
// never disagree.
func (d *Decoder) Info() wav.StreamInfo { return d.info }

// Read fills p with interleaved samples.
//
// By default the bytes are exactly those stored in the file. That means the
// encoding varies with the source, and in particular that 8-bit data is
// UNSIGNED with a midpoint of 128 while every wider integer depth is signed
// two's complement, because that is how WAV stores them. Code that assumes a
// single signed convention throughout must either check
// [wav.StreamInfo.Format] and BitDepth or ask for [WithConvertTo].
//
// Under WithConvertTo the bytes are signed little-endian integers of the
// requested width.
func (d *Decoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if d.convert == 0 {
		return d.readRaw(p)
	}
	return d.readConverted(p)
}

// readRaw copies stored bytes straight through, bounded by the data chunk.
func (d *Decoder) readRaw(p []byte) (int, error) {
	if d.remaining == 0 {
		return 0, io.EOF
	}
	if d.remaining > 0 && int64(len(p)) > d.remaining {
		p = p[:d.remaining]
	}
	n, err := d.br.Read(p)
	if d.remaining > 0 {
		d.remaining -= int64(n)
	}
	if err != nil {
		// Running out of input early is how a truncated file ends, and is
		// reported as a clean end of stream rather than as damage.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		if errors.Is(err, io.EOF) {
			d.remaining = 0
		}
		return n, err
	}
	return n, nil
}

// readConverted reads whole source samples and converts them into p.
func (d *Decoder) readConverted(p []byte) (int, error) {
	// Serve whatever the previous conversion left over. Staging the output
	// rather than converting straight into p is what lets Read honour any
	// buffer size, including one smaller than a single converted sample.
	// Converting in place would have to refuse that, which would make the
	// converting path violate the io.Reader contract the pass-through path
	// keeps.
	if d.outOff < len(d.outBuf) {
		n := copy(p, d.outBuf[d.outOff:])
		d.outOff += n
		return n, nil
	}

	// The data chunk is exhausted. Without this the decoder would read on
	// past it and convert the pad byte of an odd-length chunk, or a
	// following chunk's header, into spurious samples.
	if d.remaining == 0 {
		return 0, io.EOF
	}
	srcWidth := (d.info.SourceBitDepth + 7) / 8
	dstWidth := (d.convert + 7) / 8
	if srcWidth <= 0 || dstWidth <= 0 {
		return 0, fmt.Errorf("go-wav/pcm: %w: sample width is not positive", wav.ErrCorruptStream)
	}

	// Convert a batch large enough to fill p, and never fewer than one
	// sample, so that a tiny p still makes progress.
	samples := max(len(p)/dstWidth, 1)
	want := samples * srcWidth
	if d.remaining > 0 && int64(want) > d.remaining {
		want = int(d.remaining)
		want -= want % srcWidth
	}
	if want == 0 {
		d.remaining = 0
		return 0, io.EOF
	}

	if cap(d.srcBuf) < want {
		d.srcBuf = make([]byte, want)
	}
	buf := d.srcBuf[:want]

	n, err := io.ReadFull(d.br, buf)
	if d.remaining > 0 {
		d.remaining -= int64(n)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	// Drop a trailing partial sample; it cannot be converted.
	n -= n % srcWidth
	if n == 0 {
		d.remaining = 0
		return 0, io.EOF
	}

	need := sample.ConvertedLen(n, d.info.SourceBitDepth, d.convert)
	if cap(d.outBuf) < need {
		d.outBuf = make([]byte, need)
	}
	d.outBuf = d.outBuf[:need]
	written, cerr := sample.Convert(d.outBuf, buf[:n], d.info.SourceFormat, d.info.SourceBitDepth, d.convert)
	if cerr != nil {
		return 0, cerr
	}
	d.outBuf = d.outBuf[:written]
	d.outOff = copy(p, d.outBuf)
	return d.outOff, nil
}

// WriteTo streams the whole of the remaining audio to w. It implements
// io.WriterTo, so io.Copy drains a Decoder in one call.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	if d.err != nil {
		return 0, d.err
	}
	buf := make([]byte, readBufferSize)
	var total int64
	for {
		n, rerr := d.Read(buf)
		if n > 0 {
			written, werr := w.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// SeekToFrame positions the decoder at an inter-channel frame index and
// returns the frame it actually reached.
//
// It requires the source to implement io.Seeker and reports
// [wav.ErrSeekUnsupported] otherwise. Seeking past the end of the audio
// positions at the end, so the next Read reports io.EOF.
func (d *Decoder) SeekToFrame(frame int64) (int64, error) {
	if d.err != nil {
		return 0, d.err
	}
	if frame < 0 {
		return 0, fmt.Errorf("go-wav/pcm: SeekToFrame: negative frame index %d", frame)
	}
	seeker, ok := d.src.(io.Seeker)
	if !ok || d.dataStart < 0 {
		return 0, wav.ErrSeekUnsupported
	}

	perFrame := int64(d.hdr.BlockAlign)
	if perFrame <= 0 {
		return 0, fmt.Errorf("go-wav/pcm: %w: block align is not positive", wav.ErrCorruptStream)
	}
	dataStart := d.dataStart

	offset := frame * perFrame
	if d.hdr.DataSize >= 0 && offset > d.hdr.DataSize {
		offset = d.hdr.DataSize - d.hdr.DataSize%perFrame
	}
	if _, err := seeker.Seek(dataStart+offset, io.SeekStart); err != nil {
		return 0, err
	}
	d.br.Reset(d.src)
	if d.hdr.DataSize >= 0 {
		d.remaining = d.hdr.DataSize - offset
	}
	d.srcBuf = d.srcBuf[:0]
	// Converted bytes staged from before the seek describe the old position,
	// so they must not be handed out afterwards.
	d.outBuf = d.outBuf[:0]
	d.outOff = 0
	return offset / perFrame, nil
}
