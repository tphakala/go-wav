// Package wav provides the shared types and error sentinels for reading and
// writing WAV audio, including the 64-bit RF64 and BW64 extensions.
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
// (EBU Tech 3306) and BW64 (ITU-R BS.2088) lift that limit by writing a
// sentinel into the 32-bit fields and carrying the real 64-bit sizes in a ds64
// chunk. BW64 is structurally identical to RF64 and differs only in its magic
// and in the metadata chunks it is expected to carry, so this package reads
// both and reports which one it saw in [StreamInfo.Container].
//
// # Sample formats
//
// Samples are always little-endian. Integer PCM is unsigned at 8 bits with a
// midpoint of 128, and signed two's complement at 16, 24 and 32 bits; 24-bit
// samples are packed into three bytes with no padding. Float samples are IEEE
// 754 at 32 or 64 bits with a nominal full scale of [-1, +1].
//
// A-law, mu-law, ADPCM and the other compressed WAVE format tags are not
// supported. A file using one is reported as [ErrUnsupported] rather than
// decoded incorrectly.
package wav
