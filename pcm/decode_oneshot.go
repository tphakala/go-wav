package pcm

import (
	"bytes"
	"fmt"
	"math"
	"sync"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/internal/sample"
)

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

// DecodeInterleaved reads a complete WAVE stream from b and returns what the
// stream is together with its interleaved samples.
//
// It is the one-shot counterpart to [NewDecoder], for callers that already hold
// the whole file. Like [EncodeInterleaved] it is safe for concurrent use,
// because it draws its decoder from a pool.
//
// When no conversion is requested the returned slice ALIASES the audio data
// within b, so no copy is made. Modifying b afterwards changes the returned
// samples and vice versa; a caller that needs an independent copy must make
// one. The slice's capacity stops at the end of the audio, so appending to it
// allocates rather than overwriting whatever b holds beyond the data chunk.
//
// Under a conversion option such as [WithConvertTo] the returned slice is a
// newly allocated buffer holding the converted samples, and aliases nothing.
//
// The returned [wav.StreamInfo] describes the returned bytes, not necessarily
// the stored encoding, matching [Decoder.Info]. Its TotalFrames is the count
// the header declares, which a file cut short overstates; the length of the
// returned slice is what actually came back.
//
// b must hold the whole stream, because there is no source left to read on
// from. A partial file therefore decodes as a truncated one: the audio that is
// present comes back and the shortfall is not reported, which is how
// [Decoder.Read] treats a stream that ends early. Anything stored after the
// audio, such as a trailing LIST or id3 chunk, is left out rather than
// rejected, since a trailer is legal and common; only a stream that declares no
// length, or one decoded under [WithIgnoreLength], hands back everything that
// follows the header, as that option documents.
func DecodeInterleaved(b []byte, opts ...Option) (wav.StreamInfo, []byte, error) {
	o, _ := decoderPool.Get().(*oneshotDecoder)
	defer func() {
		// Drop the caller's buffer before pooling. A pooled decoder is only
		// dropped when the GC gets to it, and one still bound to b would keep a
		// whole file alive for as long as it sits in the pool.
		o.r.Reset(nil)
		decoderPool.Put(o)
	}()

	o.r.Reset(b)
	if err := o.d.reset("DecodeInterleaved", &o.r, opts...); err != nil {
		return wav.StreamInfo{}, nil, err
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
		return wav.StreamInfo{}, nil, fmt.Errorf(
			"go-wav/pcm: DecodeInterleaved: %w: audio begins at offset %d of a %d byte stream",
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
		return d.info, audio, nil
	}

	// Converting allocates, because the converted samples are a different
	// width from the stored ones and cannot be written back over the caller's
	// buffer. A trailing fragment shorter than one stored sample cannot be
	// converted; sample.Convert ignores it, which is the same thing
	// [Decoder.Read] does when a source runs out mid-sample.
	srcWidth := (d.info.SourceBitDepth + 7) / 8
	dstWidth := (d.convert + 7) / 8
	if srcWidth <= 0 || dstWidth <= 0 {
		return wav.StreamInfo{}, nil, fmt.Errorf(
			"go-wav/pcm: %w: sample width is not positive", wav.ErrCorruptStream)
	}
	// Widening a whole file in one call is the only place this package asks
	// for a buffer whose length may not be expressible: a streaming converter
	// never holds more than one batch, so it can never get near the limit.
	if !convertedBytesFit(len(audio)/srcWidth, dstWidth, math.MaxInt) {
		return wav.StreamInfo{}, nil, fmt.Errorf(
			"go-wav/pcm: DecodeInterleaved: converting %d bytes of %d bit audio to %d bit needs more bytes than this platform can address",
			len(audio), d.info.SourceBitDepth, d.convert)
	}
	out := make([]byte, sample.ConvertedLen(len(audio), d.info.SourceBitDepth, d.convert))
	n, err := sample.Convert(out, audio, d.info.SourceFormat, d.info.SourceBitDepth, d.convert)
	if err != nil {
		return wav.StreamInfo{}, nil, err
	}
	return d.info, out[:n], nil
}

// convertedBytesFit reports whether converting the given number of samples into
// destination samples of dstWidth bytes each yields a length no larger than
// limit, which callers set to the largest length the platform can address.
//
// It divides rather than multiplying, because the multiplication is the thing
// it guards: on a 32-bit target a long enough widening conversion wraps to a
// small positive length, and a wrapped length is a short buffer that
// [sample.Convert] would fill with the leading fraction of the audio and then
// report as a success. A refusal is the honest answer, since a length that
// cannot even be expressed could never have been allocated. It is the only case
// this guard covers: a length that is expressible but still larger than the
// memory available reaches make and fails there, the way any allocation too
// large to satisfy does.
func convertedBytesFit(samples, dstWidth, limit int) bool {
	if samples <= 0 || dstWidth <= 0 {
		return true
	}
	return samples <= limit/dstWidth
}
