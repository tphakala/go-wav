package pcm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/sample"
)

// DefaultMaxDecodedBytes is the ceiling [DecodeInterleaved] applies to its
// output. Decoded PCM is roughly the size of the stream it came from, so unlike
// a FLAC or Opus decoder this one cannot be driven to a huge allocation by a
// tiny crafted input. The ceiling still matters because the one-shot path holds
// the whole result in memory at once and reads from an [io.Reader] whose length
// it cannot know in advance: a stream that declares no length, one decoded under
// [WithIgnoreLength], or one whose header overstates the audio all read on until
// the source ends. The ceiling bounds that peak rather than trusting the source
// to be as long as it claims. It is 1 GiB, about 101 minutes of CD-quality
// (44.1 kHz, 16-bit) stereo.
//
// A caller decoding a stream it did not produce, or one legitimately longer than
// this, uses [NewDecoder] and streams the audio instead, which bounds memory to
// a single reusable buffer regardless of length.
const DefaultMaxDecodedBytes = 1 << 30

// ErrDecodeLimit reports that a one-shot decode was stopped because its output
// would exceed the byte ceiling in effect. Test for it with [errors.Is]. See
// [DefaultMaxDecodedBytes] for why the limit exists.
var ErrDecodeLimit = errors.New("go-wav/pcm: decoded size limit exceeded")

// DecodeInterleaved reads an entire WAVE stream from r and returns the decoded
// interleaved little-endian PCM together with the stream info. It is the
// one-shot mirror of [EncodeInterleaved] and matches the io.Reader decode
// contract of the sibling go-audio packages (go-flac, go-aac and go-opus each
// expose the same DecodeInterleaved and DecodeInterleavedLimit), so a caller can
// dispatch across codecs on the shared signature.
//
// It stops at [DefaultMaxDecodedBytes] and returns a wrapped [ErrDecodeLimit] if
// the output would exceed it. For a different ceiling use
// [DecodeInterleavedLimit]; for a stream of unknown or unbounded length use
// [NewDecoder], which streams the audio in memory proportional to a single
// buffer. When the whole file is already in memory, [DecodeInterleavedBytes]
// decodes it without a copy where the stored bytes can be handed back as they
// are.
//
// Options such as [WithConvertTo] are forwarded to the underlying decoder, so
// the returned StreamInfo and bytes describe the converted stream just as
// [Decoder.Info] and [Decoder.Read] would. Metadata chunks (bext, iXML) are not
// exposed on this path; a caller that wants them opens the stream with
// [NewDecoder].
func DecodeInterleaved(r io.Reader, opts ...Option) ([]byte, wav.StreamInfo, error) {
	return DecodeInterleavedLimit(r, DefaultMaxDecodedBytes, opts...)
}

// DecodeInterleavedLimit is [DecodeInterleaved] with a caller-chosen ceiling.
// maxBytes is the largest decoded output it will return; a decode that would
// exceed it stops and returns a wrapped [ErrDecodeLimit], with the bytes decoded
// so far discarded. A maxBytes of zero or less removes the ceiling, which is
// only safe for a stream the caller produced or has otherwise bounded. Options
// are forwarded to the underlying decoder.
func DecodeInterleavedLimit(r io.Reader, maxBytes int, opts ...Option) ([]byte, wav.StreamInfo, error) {
	d, err := NewDecoder(r, opts...)
	if err != nil {
		return nil, wav.StreamInfo{}, err
	}
	info := d.Info()

	var buf bytes.Buffer
	// Pre-size from the declared length so the common case allocates once
	// instead of growing by doubling. presizeHint bounds the reservation; see
	// there for why the declared count is not trusted directly.
	if n := presizeHint(info, maxBytes); n > 0 {
		buf.Grow(n)
	}

	cw := &cappedWriter{buf: &buf, max: maxBytes}
	if _, err := d.WriteTo(cw); err != nil {
		return nil, info, err
	}
	return buf.Bytes(), info, nil
}

// maxPreSize caps the up-front buffer reservation the one-shot makes from the
// header-declared frame count. That count is a claim the reader has not checked
// against the audio, so without a cap a file declaring a huge total would drive
// a reservation of up to the whole ceiling before a single sample is decoded,
// which is the very small-input-large-allocation hazard the ceiling exists to
// stop. cappedWriter still enforces the real ceiling on the bytes actually
// produced, so this only bounds the initial guess: a genuinely large stream
// grows past it by doubling.
const maxPreSize = 32 << 20 // 32 MiB

// presizeHint returns how many bytes to reserve up front for a decode of the
// given stream, or 0 to skip pre-sizing. It uses BitDepth and Channels, which
// the decoder has already set to the post-conversion width, so a companded or
// converted stream is sized for what it expands to. The reservation is bounded
// by maxPreSize and, when a positive ceiling is set, by maxBytes, so a declared
// length can never drive it past those.
func presizeHint(info wav.StreamInfo, maxBytes int) int {
	bytesPerSample := sample.BytesPerSample(info.BitDepth)
	if info.TotalFrames == 0 || info.Channels <= 0 || bytesPerSample <= 0 {
		return 0
	}
	hint := int64(info.TotalFrames) * int64(info.Channels) * int64(bytesPerSample)
	if hint <= 0 { // zero, or a wrapped-negative from an implausible declared total
		return 0
	}
	if hint > maxPreSize {
		hint = maxPreSize
	}
	if maxBytes > 0 && hint > int64(maxBytes) {
		hint = int64(maxBytes)
	}
	return int(hint)
}

// cappedWriter accumulates into buf and refuses a write that would carry the
// total past max. It writes nothing on the failing call, so buf holds only the
// whole blocks WriteTo delivered up to the point the limit was hit. A max of
// zero or less is unbounded.
type cappedWriter struct {
	buf *bytes.Buffer
	n   int
	max int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.max > 0 && c.n > c.max-len(p) {
		return 0, fmt.Errorf(
			"go-wav/pcm: DecodeInterleaved: %w: output would exceed %d bytes",
			ErrDecodeLimit, c.max)
	}
	n, err := c.buf.Write(p)
	c.n += n
	return n, err
}

// oneshotDecoder pairs a decoder with the reader it parses through. Holding the
// reader by value rather than building one per call is what lets the one-shot
// path recycle both halves together, since a pooled decoder is useless without
// the reader it is bound to.
type oneshotDecoder struct {
	d Decoder
	r bytes.Reader
}

// decoderPool recycles decoders for the one-shot path, so that decoding many
// short clips does not allocate a fresh decoder, reader and header buffer each
// time.
var decoderPool = sync.Pool{New: func() any { return new(oneshotDecoder) }}

// DecodeInterleavedBytes reads a complete WAVE stream from b and returns its
// interleaved samples together with what the stream is.
//
// It is the zero-copy fast path for callers that already hold the whole file in
// memory: where the stored bytes can be handed back unchanged it returns a
// window onto b rather than a copy (see below). [DecodeInterleaved] is the
// streaming, [io.Reader]-based form and the entry point shared with the sibling
// go-audio packages; this one trades that shared signature for the no-copy
// return. Like [EncodeInterleaved] it is safe for concurrent use, because it
// draws its decoder from a pool.
//
// The returned slice ALIASES the audio data within b exactly when the bytes
// handed back are the bytes as stored, which is the case when no conversion
// option was given AND the source is neither of the companding laws. No copy
// is made then: modifying b afterwards changes the returned samples and vice
// versa, so a caller that needs an independent copy must make one. The slice's
// capacity stops at the end of the audio, so appending to it allocates rather
// than overwriting whatever b holds beyond the data chunk.
//
// Every other stream comes back in a freshly allocated buffer that aliases
// nothing. That covers a conversion option such as [WithConvertTo], and it
// also covers an A-law or mu-law file decoded with no option at all: a
// companded byte is expanded to linear 16-bit PCM whether or not a conversion
// was asked for, so the result is about twice the stored audio and could not
// be a window onto it.
//
// That buffer is proportional to b and can be larger than it, by at most four
// times, which is 8-bit audio widened to 32-bit. Unlike [Decoder], which
// streams through a bounded batch, this holds the whole result at once, so a
// caller deciding how much to read into b is also deciding the peak
// allocation. The options alone therefore do not tell a caller which
// case it is in. Code that writes through the returned slice to edit its own
// buffer, or that counts on there being no allocation, has to rule the
// expansion out rather than infer it from the absence of an option:
// [wav.SampleFormat.Companded] over the returned StreamInfo's SourceFormat
// answers that.
//
// Either way the returned slice's capacity equals its length, so appending to
// it can never reach memory the caller did not expect to be written.
//
// The returned [wav.StreamInfo] describes the returned bytes, not necessarily
// the stored encoding, matching [Decoder.Info]. Its TotalFrames is the count
// the header declares, which a file cut short overstates, and is 0 whenever no
// credible count is available, including under [WithIgnoreLength]; the length
// of the returned slice is what actually came back.
//
// A widening conversion of a large enough file can be refused outright, when
// the converted result would be longer than this platform can express as a
// length. Only a 32-bit build can reach it: the smallest source that does is
// 512 MiB of 8-bit audio widened to 32-bit, and a narrower widening needs more
// still, up to the point where the source itself would no longer be an
// addressable slice. On a 64-bit build the smallest such source is two
// exabytes, so no file that could trigger it can be held in memory to begin
// with.
//
// That error deliberately wraps no sentinel, so errors.Is will not match it
// against anything. Every sentinel this package has describes something a
// caller can respond to, and there is nothing to respond to here: no buffer of
// the caller's to grow, because this call takes none, and no smaller request
// to retry, because the size follows from b and the requested width. The
// remedy is not a different argument but a different API, so the message names
// the call and the sizes rather than inviting a match. A caller that must
// handle such files uses [NewDecoder] instead, whose conversion is bounded by
// a fixed batch and therefore cannot reach this limit at any file size.
//
// b must hold the whole stream, because there is no source left to read on
// from. A partial file therefore decodes as a truncated one: the audio that is
// present comes back and the shortfall is not reported, which is how
// [Decoder.Read] treats a stream that ends early. Anything stored after the
// audio, such as a trailing LIST or id3 chunk, is left out rather than
// rejected, since a trailer is legal and common; only a stream that declares no
// length, or one decoded under [WithIgnoreLength], hands back everything that
// follows the header, as that option documents.
//
// This path exposes no bext or iXML chunk; a caller that wants the metadata a
// stream carries opens it with [NewDecoder] and calls [Decoder.Bext] or
// [Decoder.IXML].
func DecodeInterleavedBytes(b []byte, opts ...Option) ([]byte, wav.StreamInfo, error) {
	o, _ := decoderPool.Get().(*oneshotDecoder)
	defer func() {
		// Drop the caller's buffer and the parsed header before pooling. A
		// pooled decoder is only dropped when the GC gets to it, and one still
		// bound to b, or holding a captured bext or iXML chunk of up to the
		// reader's cap, would keep that memory alive for as long as it sits in
		// the pool.
		o.r.Reset(nil)
		o.d.hdr = nil
		decoderPool.Put(o)
	}()

	o.r.Reset(b)
	if err := o.d.reset("DecodeInterleavedBytes", &o.r, opts...); err != nil {
		return nil, wav.StreamInfo{}, err
	}
	d := &o.d

	// The parser stopped on the first audio byte and recorded where that was,
	// which is the offset the audio has to be sliced from. Reading it back is
	// the whole reason this path goes through a Decoder rather than parsing the
	// header a second time.
	start := d.dataStart
	// A bytes.Reader can always seek and the parser can never stop past the end
	// of what it read, so this holds by construction. It is checked anyway
	// because it is the bound of a slice expression: if either of those ever
	// stops being true, an error is a better answer than a panic in a caller's
	// decode loop.
	if start < 0 || start > int64(len(b)) {
		return nil, wav.StreamInfo{}, fmt.Errorf(
			"go-wav/pcm: DecodeInterleavedBytes: %w: audio begins at offset %d of a %d byte stream",
			wav.ErrCorruptStream, start, len(b))
	}

	// The end is whichever comes first: the length the header declared, or the
	// end of the buffer. A declared length is only a claim, so slicing by it
	// alone would panic on a file that was cut short or whose header lies, and
	// there is no declared length at all when the writer never patched the size
	// field. Taking the lower of the two also excludes the pad byte an odd
	// length data chunk carries, which is alignment rather than audio.
	end := int64(len(b))
	if d.remaining >= 0 && start+d.remaining < end {
		end = start + d.remaining
	}
	// A three-index slice keeps the result from being appended into whatever
	// follows the audio in the caller's own buffer.
	audio := b[start:end:end]

	if d.convert == 0 {
		return audio, d.info, nil
	}

	// Converting allocates, because the converted samples are a different
	// width from the stored ones and cannot be written back over the caller's
	// buffer. This is also the path a companded source takes with no option at
	// all, since the decoder expands one whether or not it was asked to, which
	// is why the pass-through above is not simply the no-option case. A
	// trailing fragment shorter than one stored sample cannot be converted;
	// sample.Convert ignores it, which is the same thing [Decoder.Read] does
	// when a source runs out mid-sample.
	srcWidth := sample.BytesPerSample(d.info.SourceBitDepth)
	dstWidth := sample.BytesPerSample(d.convert)
	if srcWidth <= 0 || dstWidth <= 0 {
		return nil, wav.StreamInfo{}, fmt.Errorf(
			"go-wav/pcm: %w: sample width is not positive", wav.ErrCorruptStream)
	}
	// Widening a whole file in one call is the only place this package asks
	// for a buffer whose length may not be expressible. The streaming
	// converter cannot reach the limit because maxConvertBatch bounds every
	// batch it stages; here the whole file is the batch, and the file's size
	// is not this package's to choose.
	if !convertedBytesFit(len(audio)/srcWidth, dstWidth, math.MaxInt) {
		return nil, wav.StreamInfo{},
			errUnrepresentableSize(len(audio), d.info.SourceBitDepth, d.convert)
	}
	out := make([]byte, sample.ConvertedLen(len(audio), d.info.SourceBitDepth, d.convert))
	n, err := sample.Convert(out, audio, d.info.SourceFormat, d.info.SourceBitDepth, d.convert)
	if err != nil {
		return nil, wav.StreamInfo{}, err
	}
	// Three-indexed like the pass-through result, so both cases hand back a
	// slice whose capacity ends at its length. Convert writes exactly
	// ConvertedLen bytes today, so this trims nothing; it is here so that the
	// promise holds without depending on that.
	return out[:n:n], d.info, nil
}

// errUnrepresentableSize reports a conversion whose result cannot be expressed
// as a length on this platform.
//
// It is a function rather than an inline fmt.Errorf so that the error can be
// asserted in a test. The branch that raises it needs a source larger than a
// 64-bit machine can allocate, so it is unreachable from a decode on the
// platform the tests run on, and the property worth pinning is not that the
// branch fires but what the error it produces is: an error wrapping nothing.
// Without a test, the decision to wrap nothing is one line away from being
// reversed by someone who reads only the sibling refusal in internal/sample.
//
// See [DecodeInterleavedBytes] for why it wraps nothing.
func errUnrepresentableSize(audioLen, srcBits, dstBits int) error {
	return fmt.Errorf(
		"go-wav/pcm: DecodeInterleavedBytes: converting %d bytes of %d bit audio to %d bit needs more bytes than this platform can address",
		audioLen, srcBits, dstBits)
}

// convertedBytesFit reports whether converting the given number of samples into
// destination samples of dstWidth bytes each yields a length no larger than
// limit, which callers set to the largest length the platform can address.
//
// It divides rather than multiplying, because the multiplication is the thing
// it guards: on a 32-bit target a long enough widening conversion leaves the
// int range, and a length that cannot even be expressed could never have been
// allocated. It is the only case this guard covers: a length that is
// expressible but still larger than the memory available reaches make and fails
// there, the way any allocation too large to satisfy does.
//
// [sample.ConvertedLen] applies the same ceiling and sample.Convert refuses a
// source that exceeds it, so nothing here depends on this check for
// correctness. It stays because it answers before anything is allocated, and
// because it can name the exported call and the sizes involved, which an error
// raised from inside the conversion cannot.
//
// The two refusals are not interchangeable, and the difference is deliberate
// rather than an oversight. Convert wraps io.ErrShortBuffer, which fits a
// function holding a dst that could in principle have been longer;
// [errUnrepresentableSize] wraps nothing, because DecodeInterleavedBytes takes
// no destination and there is nothing for a caller to grow. Which one a caller
// sees does not arise in practice, because this check is the strictly earlier
// of the two, so the one inside the conversion is unreachable from this path;
// both halves are pinned by tests so that neither the precedence nor the
// sentinel choice can drift.
func convertedBytesFit(samples, dstWidth, limit int) bool {
	if samples <= 0 || dstWidth <= 0 {
		return true
	}
	return samples <= limit/dstWidth
}
