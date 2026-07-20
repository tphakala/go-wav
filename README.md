# go-wav

[![CI](https://github.com/tphakala/go-wav/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-wav/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-wav.svg)](https://pkg.go.dev/github.com/tphakala/go-wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/tphakala/go-wav)](https://goreportcard.com/report/github.com/tphakala/go-wav)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-wav)](https://github.com/tphakala/go-wav/blob/main/go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala)](https://github.com/sponsors/tphakala)

A pure-Go library for reading and writing WAV audio, including the 64-bit RF64
and BW64 extensions for files past 4 GiB. No cgo, no runtime dependencies.

It is the WAV member of a family that also covers
[FLAC](https://github.com/tphakala/go-flac),
[Opus](https://github.com/tphakala/go-opus),
[AAC](https://github.com/tphakala/go-aac) and
[M4A](https://github.com/tphakala/go-m4a), and it presents the same API shape as
its siblings, so a program that already speaks one of them speaks this one too.

## Install

```
go get github.com/tphakala/go-wav
```

Requires Go 1.26 or newer.

## Status

- **Containers**: RIFF, RF64 (EBU Tech 3306) and BW64 (ITU-R BS.2088), read and
  written.
- **Sample formats**: integer PCM at 8, 16, 24 and 32 bits, and IEEE float at 32
  and 64 bits. `WAVE_FORMAT_EXTENSIBLE` is read, and written automatically
  wherever the format requires it.
- **Sample rates and channels**: unrestricted. 384 kHz and eight channels are
  ordinary, not special cases.
- **Validated** bit-exactly in both directions against ffmpeg and sox, across
  every supported depth and format, 8 kHz to 384 kHz, and RF64.

Not implemented: A-law, mu-law, ADPCM and the other compressed format tags; and
the metadata chunks (`bext`, `LIST`/`INFO`, `cue `, `iXML`, `axml`, `chna`).
Unknown chunks are skipped cleanly on read, so files carrying them decode
normally, their metadata simply is not exposed.

## Usage

```go
import wavpcm "github.com/tphakala/go-wav/pcm"
```

### Encoding

```go
cfg := wavpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}

// One shot, drawn from a pool and safe for concurrent use:
err := wavpcm.EncodeInterleaved(w, cfg, samples)

// Or streaming, accepting any chunk size:
e, err := wavpcm.NewEncoder(w, cfg)
_, err = io.Copy(e, src)
err = e.Close()
```

`Config` is a flat struct whose zero values are all documented, so the three
required fields make a complete configuration. Float output is a field, not a
different constructor:

```go
cfg := wavpcm.Config{SampleRate: 96000, BitDepth: 32, Channels: 2,
    Format: wav.SampleFormatFloat}
```

A plain `io.Writer` is always accepted. When the sink also implements
`io.WriteSeeker` the header is patched at `Close`, which is what allows a stream
to become RF64 once it outgrows plain RIFF.

### Decoding

```go
d, err := wavpcm.NewDecoder(r)
info := d.Info()        // valid immediately
_, err = io.Copy(w, d)  // WriteTo drains the whole stream
```

By default the decoder is a pass-through: `Read` yields the bytes as stored, so
24-bit audio stays packed in three bytes and nothing is widened behind your
back. That also means the encoding varies with the file, and in particular that
**8-bit data is unsigned while every wider integer depth is signed**, because
that is how WAV stores them. To normalise:

```go
d, err := wavpcm.NewDecoder(r, wavpcm.WithConvertTo(16))
```

which converts every source, float and 8-bit included, to signed 16-bit.

`Info()` always describes what `Read` yields rather than what is on disk, so the
two can never disagree; the stored encoding stays available as `SourceBitDepth`
and `SourceFormat`.

### Files past 4 GiB

A plain RIFF header stores sizes in 32-bit fields, so it cannot describe 4 GiB
or more. RF64 and BW64 lift that limit with a `ds64` chunk. The policy is one
config field, and the vocabulary matches ffmpeg's `-rf64` flag:

```go
cfg.RF64 = wavpcm.RF64Auto   // the default
```

`RF64Auto` writes an ordinary RIFF header with a small `JUNK` chunk reserved
after it, and if the stream outgrows 4 GiB that header is rewritten in place as
RF64. Small files stay plain RIFF and readable by anything; large ones become
valid RF64, with no second pass over the audio.

The rescue needs a seekable sink, since the `ds64` goes at the front. When the
sink cannot seek, declare the length up front instead:

```go
cfg.TotalFrames = frames // the header is then correct from the first byte
```

Declaring it is a promise, and `Close` reports an error if you write a different
number of frames.

Where no correct size can be produced, the encoder returns `wav.ErrTooLarge`. It
never writes a size field it knows to be wrong, which is the failure mode this
library was written to eliminate.

### Sniffing

```go
if wav.Sniff(header) { /* RIFF, RF64 or BW64 */ }
```

Twelve bytes are enough, and unlike a hand-rolled `RIFF` check it recognises
RF64 and BW64.

## Errors

Sentinels only, testable with `errors.Is`: `ErrNotRIFF`, `ErrCorruptStream`,
`ErrUnsupported`, `ErrEncoderClosed`, `ErrTooLarge` and `ErrSeekUnsupported`.
`ErrNotRIFF` means the input is not a WAV file; `ErrCorruptStream` means it is
one and it is broken.

The reader tolerates what real files get wrong: a missing pad byte after an odd
chunk, a data size of zero or `0xFFFFFFFF`, a declared size running past the end
of the file, trailing bytes after the audio, and chunks in any order. It does
not tolerate ambiguity about the sample format: a stream it cannot decode is
reported rather than guessed at.

## License

MIT. See [LICENSE](LICENSE).

## Sponsor

If this is useful to you, [sponsorship](https://github.com/sponsors/tphakala) is
welcome.
