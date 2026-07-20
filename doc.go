// Package wav provides the shared types and error sentinels for reading and
// writing WAV audio. It reads the 64-bit RF64 and BW64 extensions and writes
// RF64.
//
// The streaming API lives in the pcm subpackage, so that this package can hold
// the types both layers need without an import cycle. Callers who want to
// encode or decode audio want [github.com/tphakala/go-wav/pcm]; this package is
// what that one returns and the errors it reports.
//
// # Containers
//
// Three container flavours share one chunk layout. Plain RIFF stores sizes in
// 32-bit fields and therefore cannot describe a file of 4 GiB or more. RF64
// (EBU Tech 3306) and BW64 (ITU-R BS.2088) lift that limit with a sentinel in
// the 32-bit fields and the real 64-bit sizes carried in a ds64 chunk. BW64 is
// structurally identical to RF64 and differs only in its magic and in the
// metadata chunks it is expected to carry, so this package reads both and
// reports which one it saw in [StreamInfo.Container].
//
// Writing is RIFF or RF64. BW64 is read only, because the ADM metadata in its
// axml and chna chunks is what makes a file BW64 rather than RF64 and this
// library writes no such chunk; see [ContainerBW64].
//
// # Sample formats
//
// Samples are always little-endian. Integer PCM is unsigned at 8 bits with a
// midpoint of 128, and signed two's complement at 16, 24 and 32 bits; 24-bit
// samples are packed into three bytes with no padding. Float samples are IEEE
// 754 at 32 or 64 bits with a nominal full scale of [-1, +1].
//
// The two G.711 companding laws, A-law and mu-law, are decoded. A companded
// byte is not a sample on any linear scale, so it is always expanded to linear
// 16-bit PCM on the way out rather than handed back as stored: [StreamInfo]
// reports the expansion in Format and BitDepth and the stored encoding in
// SourceFormat and SourceBitDepth. Neither law is written, because nothing here
// compands linear samples; see [SampleFormatALaw].
//
// ADPCM and the other compressed WAVE format tags are not supported. A file
// using one is reported as [ErrUnsupported] rather than decoded incorrectly.
package wav
