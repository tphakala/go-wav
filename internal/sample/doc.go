// Package sample converts WAVE sample data between the encodings this library
// supports. It is the single place where the on-disk representation of a sample
// is written down: every other package asks this one rather than reimplementing
// the rules.
//
// # Encoding rules
//
// All sample data is little-endian. Integer PCM is unsigned at 8 bits with a
// midpoint of 128 (silence), and signed two's complement at 16, 24 and 32 bits.
// A 24-bit sample occupies exactly three bytes with no padding. Float samples
// are IEEE 754 binary32 or binary64 with a nominal full scale of [-1, +1].
//
// The 8-bit asymmetry is the detail that trips up most implementations: 8-bit
// PCM is the only depth stored unsigned, so decoding a byte subtracts 128 and
// encoding one adds it back. Every value in flight through this package is a
// signed sample; the bias exists only on disk.
//
// # Conversion policy
//
// [Convert] always produces signed integer PCM, because that is the only sample
// type the decoding path hands to callers. Integer to integer conversion is a
// pure bit shift, never a multiply-divide, so widening is exact and narrowing
// discards low bits. Narrowing truncates toward negative infinity (an
// arithmetic right shift) and applies neither rounding nor dither: this is a
// format library, not a mastering tool, and a caller who wants a dithered
// down-conversion should do it before handing samples over.
//
// Float to integer conversion multiplies by the full-scale value
// 2^(bits-1), clamps to the representable range and then rounds half away from
// zero. The positive limit is one less than full scale because +1.0 scaled by
// 32768 is 32768, which does not fit in an int16. Real-world float WAV files
// routinely carry samples past full scale, so the clamp is mandatory rather
// than defensive. NaN maps to 0 and the infinities map to the corresponding
// limit, so a broken sample can never propagate a NaN into integer output.
//
// Integer to float conversion is deliberately absent: this library never emits
// float samples to a caller.
package sample
