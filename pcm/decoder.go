package pcm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"

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

// readBufferSize is the buffered reader's window.
//
// It is sized for header parsing and nothing else. The only window-sensitive
// operation there is the Peek(4) that resolves a missing pad byte; chunk
// payloads are read into their own buffer, so a chunk larger than this window
// parses fine. Audio never comes through it, because reading samples switches
// to the wider streamBufferSize window below.
//
// Keeping it small is what makes opening a file just to read its Info cheap,
// which is the shape of any tool that scans a directory of recordings.
const readBufferSize = 512

// streamBufferSize is the window used once a caller starts reading audio. It
// is wide because a small one would be refilled once per caller read for any
// caller reading in smaller blocks, multiplying the reads reaching the source.
const streamBufferSize = 64 << 10

// writeToBufferSize is the staging buffer WriteTo streams through. It is
// independent of readBufferSize because here a larger block genuinely does
// reduce the number of round trips.
const writeToBufferSize = 64 << 10

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
//
// Reading to the end of the source means exactly that: anything stored after
// the data chunk, such as a trailing LIST or id3 chunk, is handed back as
// audio, because without a length there is nothing to tell audio and trailer
// apart. That is the price of recovering a stream whose header lies, so the
// option belongs on files known to need it rather than on well-formed ones.
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

	// stream is a wider buffer layered over br, created on the first read of
	// audio. Header parsing wants a small window because a Decoder opened only
	// to read Info pays for it; streaming wants a wide one because a caller
	// reading in small blocks would otherwise refill a small window constantly.
	// Deferring it gives each case what it needs, and costs the probe nothing.
	stream *bufio.Reader

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

	// The streaming window is carried across, like the other buffers. Dropping
	// it would make every Reset reallocate 64 KiB, which is the opposite of
	// what pooling a Decoder is for.
	stream := d.stream
	if stream != nil {
		stream.Reset(br)
	}

	*d = Decoder{
		br:        br,
		stream:    stream,
		src:       r,
		cfg:       cfg,
		hdr:       hdr,
		info:      hdr.Info,
		remaining: hdr.DataSize,
		dataStart: dataStart,
		srcBuf:    d.srcBuf[:0],
		outBuf:    d.outBuf[:0],
	}
	// A decoder with no length to bound reads by runs to the end of the
	// source, which is what a remaining of -1 means. Routing this through
	// lengthKnown rather than through cfg.ignoreLength alone keeps the
	// unknown-length routes and the option answering to one predicate.
	if !d.lengthKnown() {
		d.remaining = -1
	}
	if cfg.ignoreLength {
		// TotalFrames may have been derived from the declared size, so
		// ignoring that size makes the count untrustworthy. It can also have
		// come from a fact chunk or a ds64 sampleCount on a stream whose byte
		// length was already unknown; that count is discarded here too, since
		// the option's whole point is to trust the source over the header.
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

// lengthKnown reports whether the decoder has a data chunk length it may bound
// reads and seeks by.
//
// The header's own field is not the whole answer. WithIgnoreLength does not
// touch hdr.DataSize, because the header can perfectly well declare a plausible
// size that the option exists to bypass; consulting the field on its own would
// silently reimpose the exact length the caller asked to ignore.
func (d *Decoder) lengthKnown() bool {
	return !d.hdr.DataSizeUnknown() && !d.cfg.ignoreLength
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

// audio returns the reader to take sample data from, widening the buffer the
// first time it is asked. Layering over br rather than over the source keeps
// whatever br still holds, so no bytes are stranded.
func (d *Decoder) audio() *bufio.Reader {
	if d.stream == nil {
		d.stream = bufio.NewReaderSize(d.br, streamBufferSize)
	}
	return d.stream
}

// readRaw copies stored bytes straight through, bounded by the data chunk.
func (d *Decoder) readRaw(p []byte) (int, error) {
	if d.remaining == 0 {
		return 0, io.EOF
	}
	if d.remaining > 0 && int64(len(p)) > d.remaining {
		p = p[:d.remaining]
	}
	n, err := d.audio().Read(p)
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

	n, err := io.ReadFull(d.audio(), buf)
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
	buf := make([]byte, writeToBufferSize)
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
// returns the frame it reached.
//
// It requires the source to implement io.Seeker and reports
// [wav.ErrSeekUnsupported] otherwise.
//
// What a seek past the end does depends on whether the stream declared a
// length the decoder may trust. When it did, a request beyond the audio is
// clamped to the end of the data chunk and the returned frame is the one
// actually reached, so a returned frame lower than the requested one means the
// stream ran out. When the length is unknown, or [WithIgnoreLength] is
// discarding it, there is no boundary to clamp against: the seek is performed
// as asked and the requested frame is returned even if it lies past the audio.
// The next Read then reports io.EOF, which is the only end-of-stream signal
// available on that path.
//
// The decoder deliberately does not measure the source to recover a boundary
// for the unknown-length case. Doing so would cost a seek to the end on every
// call, and would be wrong for a file still being appended to, which is the
// recovery case WithIgnoreLength exists for.
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

	// The frame index becomes a byte offset by multiplying, and both operands
	// are int64, so a large enough index wraps. A wrapped offset is worse than
	// an out-of-range one, because the clamp below can no longer recognise it:
	// a wrap to a negative value seeks in front of the audio and hands the
	// caller the file header as if it were samples, and a wrap to a small
	// positive value quietly lands somewhere inside the first few frames. No
	// such frame can exist in a stream addressable by an int64 byte offset, so
	// the honest answer is to refuse it rather than to seek somewhere else.
	maxFrame := (math.MaxInt64 - dataStart) / perFrame
	if frame > maxFrame {
		return 0, fmt.Errorf(
			"go-wav/pcm: SeekToFrame: frame index %d exceeds the largest addressable frame %d",
			frame, maxFrame)
	}

	lengthKnown := d.lengthKnown()

	offset := frame * perFrame
	if lengthKnown && offset > d.hdr.DataSize {
		offset = d.hdr.DataSize - d.hdr.DataSize%perFrame
	}
	if _, err := seeker.Seek(dataStart+offset, io.SeekStart); err != nil {
		return 0, err
	}
	d.br.Reset(d.src)
	// The streaming buffer holds bytes from before the seek, so it has to be
	// emptied. Reset it onto br rather than dropping it: discarding would make
	// every seek allocate a fresh 64 KiB window, and random access is exactly
	// the workload that seeks repeatedly.
	if d.stream != nil {
		d.stream.Reset(d.br)
	}
	if lengthKnown {
		d.remaining = d.hdr.DataSize - offset
	} else {
		// The length is unknown, or is being ignored, so there is no bound to
		// clamp remaining to. A Read after this seek runs to the real end of
		// the source, exactly as it would have without any seek at all.
		d.remaining = -1
	}
	d.srcBuf = d.srcBuf[:0]
	// Converted bytes staged from before the seek describe the old position,
	// so they must not be handed out afterwards.
	d.outBuf = d.outBuf[:0]
	d.outOff = 0
	return offset / perFrame, nil
}
