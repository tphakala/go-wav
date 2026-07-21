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
//
// That holds for a pass-through stream, which fills it. A converting stream is
// bounded by maxConvertBatch instead, so a narrowing conversion hands the sink
// less than this per call and a larger buffer here would not change that; see
// maxConvertBatch.
const writeToBufferSize = 64 << 10

// maxConvertBatch bounds the source bytes a converting Read stages at once.
//
// It exists because the batch is otherwise sized from the caller's buffer, and
// that buffer is the caller's to choose. Without a cap, asking for a whole
// clip in one Read makes the decoder allocate srcWidth/dstWidth times that
// buffer to stage the source for it, which for a float64 file converted to
// 8-bit is eight times the amount the caller thought they were asking for. A
// cap turns that amplification into a short read, which io.Reader permits.
//
// The cap is also what makes the batch arithmetic safe. samples*srcWidth is a
// native int multiplication of a caller-controlled length, so on a 32-bit
// target a large enough buffer used to wrap it: negative, which panicked
// slicing the staging buffer, or exactly zero, which reported a clean end of
// stream partway through a file. Bounding the sample count by a constant
// before the multiplication holds the product at or below this value on every
// platform, so neither is reachable.
//
// What a caller sees is that a converting Read yields at most
// maxConvertBatch/srcWidth*dstWidth bytes however large a buffer it passes.
// The bound is on the SOURCE, so a narrowing conversion is served least per
// call: a float64 file read as 8-bit gets 8 KiB per Read, and the cap starts
// to bind at a buffer of 8193 bytes. A widening conversion is served more,
// which is also what bounds the converted staging buffer at four times this
// value, the widest ratio the package supports being 8-bit to 32-bit.
//
// It is defined as streamBufferSize rather than restating the number, so that
// one batch stays one read of the buffered reader underneath if that window
// ever moves.
const maxConvertBatch = streamBufferSize

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
// Without it the decoder is a pass-through for every source but the two named
// below, and Read yields the data chunk verbatim, which means the sample
// encoding varies with the file. Converting normalises the two traps in that:
// float sources become integers, scaled by full scale and clamped, and 8-bit
// sources become signed rather than the unsigned form WAV stores them in.
//
// An A-law or mu-law source is the one case that is converted with or without
// this option, to linear 16-bit by default. Passing the option over such a
// source chooses the width of an expansion that was happening anyway.
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

	// A companded source is expanded whether or not the caller asked for a
	// conversion, because a G.711 code is not a sample on any linear scale.
	// Handing the stored bytes back would be handing back a different codec's
	// payload: unlike the 8-bit unsigned case, where the bytes are samples in
	// a convention the caller can correct for, there is no scale on which a
	// companded byte is the sample it stands for. Sixteen bits is the width
	// the laws are defined against, so it is what an unasked-for expansion
	// lands on; a caller wanting another width asks for it as usual.
	convertTo := cfg.convertTo
	if convertTo == 0 && d.info.SourceFormat.Companded() {
		convertTo = 16
	}

	// Info describes what Read yields, so a conversion changes it.
	if convertTo != 0 {
		d.convert = convertTo
		d.info.BitDepth = convertTo
		d.info.Format = wav.SampleFormatPCM
		// The declared meaningful width described the stored samples, so it
		// does not survive a change of width. It is absent for a companded
		// source anyway, since nothing declares one.
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
// [WithConvertTo], and over a companded source whether or not the option was
// given, the BitDepth and Format fields are the converted ones and the stored
// encoding is in SourceBitDepth and SourceFormat. Info and Read never
// disagree.
func (d *Decoder) Info() wav.StreamInfo { return d.info }

// Read reads interleaved samples into p and returns how many bytes it wrote.
//
// It is an ordinary io.Reader and returns short reads. Without a conversion a
// short read is whatever the source gave, since the source is read once per
// call and need not fill p. Under [WithConvertTo] the decoder bounds itself as
// well: it converts one staged batch per call, so once p is larger than that
// batch yields, the returned count stops growing with it. A float64 source
// read as 8-bit is served 8 KiB per call for any p of 8193 bytes or more, and
// fills a smaller p as before.
//
// A short read is not the end of the stream, which is reported as io.EOF.
// Callers must loop, or hand the decoder to io.Copy, io.ReadAll or
// io.ReadFull.
//
// By default the bytes are those stored in the file, with the single exception
// below. That means the encoding varies with the source, and in particular that
// 8-bit data is UNSIGNED with a midpoint of 128 while every wider integer depth
// is signed two's complement, because that is how WAV stores them. Code that
// assumes a single signed convention throughout must either check
// [wav.StreamInfo.Format] and BitDepth or ask for [WithConvertTo].
//
// The exception is an A-law or mu-law source, which is expanded to linear
// 16-bit PCM whether or not a conversion was asked for. Those bytes are not
// samples in a convention a caller could correct for, the way an unsigned
// 8-bit byte is; they are a different codec's payload, and handing them back
// would give noise to anyone who did not decode them. [Decoder.Info] reports
// the expansion, so it still describes what this returns.
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

// convertBatchLen returns the number of source bytes a converting read should
// stage for a caller buffer of bufLen bytes, given the stored and requested
// sample widths and how many bytes of the data chunk are left. A negative
// remaining means the length is unknown, so only the buffer and the cap bound
// the batch.
//
// It takes lengths rather than slices so that the buffer sizes that used to
// wrap the batch arithmetic can be pinned in a test without allocating them.
// A zero result means there is no whole sample left to stage, which the caller
// reports as the end of the stream, or that a width was not positive, which
// the caller has already rejected before it gets here.
func convertBatchLen(bufLen, srcWidth, dstWidth int, remaining int64) int {
	// The caller checks both widths first and reports a corrupt stream, so
	// this is unreachable through it. It is kept because the division below
	// would panic without it, and because this function is entered directly
	// from its test; it deliberately does not try to report the difference,
	// since the caller owns that error and states it better.
	if srcWidth <= 0 || dstWidth <= 0 {
		return 0
	}

	// Enough samples to fill the caller's buffer, never fewer than one so a
	// buffer smaller than a single converted sample still makes progress, and
	// never more than the batch cap. Bounding the count by division before
	// the multiplication is what keeps the product from wrapping; see
	// maxConvertBatch.
	//
	// The floor on the cap itself cannot bind for any width this package
	// supports, since the widest sample is 8 bytes and the cap is 64 KiB. It
	// guards the case where the quotient is 0, which is what a sample wider
	// than the whole batch would give, so that the batch is never empty for a
	// reason the caller would read as end of stream.
	samples := max(bufLen/dstWidth, 1)
	samples = min(samples, max(maxConvertBatch/srcWidth, 1))
	want := samples * srcWidth

	// Past the end of the data chunk the chunk's own length is the smaller
	// bound, and a trailing fragment shorter than one stored sample is
	// dropped because nothing can be converted from it. Narrowing remaining
	// to an int is safe here because this branch is only reached when it is
	// below want, which the cap has already put well inside an int on every
	// platform.
	if remaining >= 0 && remaining < int64(want) {
		want = int(remaining)
		want -= want % srcWidth
	}
	return want
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

	want := convertBatchLen(len(p), srcWidth, dstWidth, d.remaining)
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
