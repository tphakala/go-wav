# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (usage examples, supported formats, error semantics). This file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-wav` (root package `wav`, streaming API in `pcm`) is a pure-Go WAV reader and writer. It reads RIFF, RF64 and BW64 and writes RIFF and RF64, so files past 4 GiB get correct size fields. It is the WAV member of a family with go-flac, go-opus, go-aac and go-m4a, and it presents the same API shape as them. It aims to:

1. **Build anywhere Go builds.** No cgo, no runtime dependencies. The only module dependency is `go-ruleguard/dsl`, which is lint-only behind a build tag.
2. **Never write a size field it knows to be wrong.** Where a correct size cannot be produced the encoder returns `ErrTooLarge`. This is the failure mode the library exists to eliminate.
3. **Survive hostile input.** The reader parses untrusted bytes. Parsing is bounds-checked and total, never panics, and every allocation driven by a file's own claims is bounded. Consumers are long-running unattended services (BirdNET-Go is the main one, shipping on 32-bit ARM and 386 as well).

Packages:

| Package | Role |
|---|---|
| `wav` (root) | Shared types (`StreamInfo`, `Container`, `SampleFormat`), `Sniff` and `SniffContainer`, sentinel errors, `Version` |
| `pcm` | The API callers use: `Config`, `Encoder`, `Decoder`, `EncodeInterleaved`, `DecodeInterleaved[Limit]`, `DecodeInterleavedBytes`, `Bext`, options |
| `internal/riff` | Container half: chunk walking, fmt chunk, `ds64`, header build, in-place RF64 upgrade |
| `internal/sample` | Sample encodings and conversion: 8-bit bias, 24-bit packing, float to int, A-law and mu-law tables |
| `rules` | gocritic ruleguard matchers (build tag `ruleguard`, lint-only) |
| `tools` | Pinned tool dependencies (`tools.go`) |

Non-goals: ADPCM and other compressed format tags, writing A-law or mu-law, writing BW64, metadata chunks other than `bext` and `iXML` (`LIST`/`INFO`, `cue `, `axml`, `chna` are skipped on read), integer to float conversion.

### API stability

The module is v1. The exported API of both packages is the contract: do not change a signature, remove a symbol or change documented behaviour without calling it out as a breaking change (`feat(pcm)!:` or `fix(pcm)!:`) and a major-version discussion first. The API is deliberately aligned with the sibling libraries (`NewX` plus `Reset`, flat `Config`, variadic options, one-shot helpers); a change that breaks that alignment needs the siblings considered. `Version` in `wav.go` is checked against the tag by the release workflow, so bump it only as part of a release.

## Design rules you must not break

A change that violates one of these is wrong even if tests pass.

1. **No cgo, ever.** Everything builds with `CGO_ENABLED=0`. The module has no runtime dependencies; do not add one.
2. **The wire format is little-endian, and 8-bit is unsigned.** 8-bit PCM has a midpoint of 128; 16, 24 and 32-bit are signed; 24-bit is packed into three bytes. Only `internal/sample` writes down this representation; other packages ask it.
3. **Offsets, sizes and frame counts are `int64` or `uint64`.** `int` is 32 bits on 386 and arm, and an `int` offset overflows at 2 GiB in a library whose job is crossing 4 GiB. Every narrowing to `uint32` is guarded and reports `ErrTooLarge` rather than wrapping. The fmt chunk's 16-bit fields and the sample rate (checked against `MaxSampleRate`, 2147483647) are the only values narrowed into `int`.
4. **Parsers are total and size-capped.** A chunk size, entry count or length read from a file is checked against the bytes actually present before it drives a `make`, a loop or an offset. Chunk payload reads are capped (`riff.MaxChunkPayload`). Malformed input returns a typed error, never a panic or an index error.
5. **Typed errors with `%w`.** Map conditions to the sentinels in `errors.go` so callers can use `errors.Is`:
   - `ErrNotRIFF`: not a WAV file at all (the first twelve bytes are not a RIFF, RF64 or BW64 WAVE header).
   - `ErrCorruptStream`: a WAV file that is broken beyond what the reader works around.
   - `ErrUnsupported`: well-formed but out of scope, or an output this package will not write (compressed tags, BW64 or companded output, bit depths outside the set).
   - `ErrEncoderClosed`, `ErrTooLarge`, `ErrSeekUnsupported`.
   - `ErrDecodeLimit` (in `pcm`) is a resource limit, not a verdict on the file. Keep it distinct from `ErrCorruptStream`.
6. **The reader tolerates what real files get wrong, and nothing about the sample format.** Accepted: a missing pad byte after an odd chunk, a data size of 0 or `0xFFFFFFFF`, a declared size past the end of the file, trailing bytes, chunks in any order before fmt and data, unknown chunks anywhere, a wrong `nBlockAlign` (replaced by the derived value), a `ValidBits` wider than its container (reported absent). Refused: zero channels, zero sample rate, a rate above `MaxSampleRate`, an undecodable format. Never guess a sample format. See `internal/riff/doc.go` before touching this.
7. **`StreamInfo.TotalFrames` is a claim, not a count.** A truncated file reports the length it was given. `DataSizeKnown` says whether the claim came with a boundary the decoder bounds reads and seeks by. Do not turn either into a promise that the audio is present.
8. **RF64 policy.** `RF64Auto` (the default) writes RIFF with a `JUNK` chunk sized exactly like a `ds64`, and upgrades the header in place if the stream outgrows 4 GiB; that needs a seekable sink. A non-seekable sink needs `Config.TotalFrames` declared up front, and `Close` errors if the written count differs. Nothing here is allowed to write a header that disagrees with the audio.
9. **`Info()` describes what `Read` yields, not what is on disk.** The stored encoding stays in `SourceFormat` and `SourceBitDepth`. A-law and mu-law are always expanded to linear 16-bit, with or without `WithConvertTo`. Default decode is otherwise a pass-through of stored bytes.
10. **Bounded one-shot decodes.** `DecodeInterleaved` stops at `DefaultMaxDecodedBytes` (1 GiB) with a wrapped `ErrDecodeLimit`; `DecodeInterleavedLimit` takes the ceiling. The up-front reservation derived from the header's frame count is capped (`maxPreSize`) and by the ceiling, because that count is an unchecked claim.
11. **`DecodeInterleavedBytes` aliasing is documented behaviour.** When the stored bytes are returned unchanged, the result aliases the caller's slice; under `WithConvertTo`, or for a companded source, it is a fresh buffer. Keep the README and godoc statement true when you change either path. The `io.Reader` form always allocates.
12. **`bext` and `iXML` round-trip.** A decoded `*Bext` can be set back on `Config.Bext`, and `IXML()` text back on `Config.IXML`. `Bext()` reads leniently; the encoder validates strictly and names the offending field. `Config` stays comparable (`Bext` is a pointer, `IXML` a string). A nil `Bext` and empty `IXML` write no chunk, so existing output bytes do not change.
13. **Concurrency.** An `Encoder` or `Decoder` is not safe for concurrent use; the one-shot helpers are. The package holds no mutable global state. Pooled objects must be fully reset on reuse.
14. **Interop is the oracle.** Output is validated bit-exactly against ffmpeg and sox in both directions, across every supported depth and format, 8 kHz to 384 kHz, and RF64.

## Layout

```
wav.go               Version, Container, SampleFormat, StreamInfo, Sniff, SniffContainer
errors.go            Sentinel errors shared by both layers
doc.go               Package overview (containers, sample formats)

pcm/pcm.go           Config, RF64Mode, validation
pcm/encoder.go       Encoder (io.WriteCloser), header patch and RF64 upgrade at Close
pcm/encode_oneshot.go  EncodeInterleaved (pooled)
pcm/decoder.go       Decoder (io.Reader, io.WriterTo, seeking), options, Info, Bext, IXML
pcm/decode_oneshot.go  DecodeInterleaved, DecodeInterleavedLimit, DecodeInterleavedBytes, ErrDecodeLimit, cappedWriter
pcm/bext.go          Bext type, parse and marshal, versioned fields

internal/riff/       chunk.go (limits, sentinels), reader.go (chunk walk, header parse),
                     format.go (fmt chunk, format tags), writer.go (BuildHeader, UpgradeToRF64)
internal/sample/     sample.go (conversion), companded.go (G.711 tables built from the law)
rules/               gocritic ruleguard matchers (lint-only)
```

`internal/sample` states its rules twice on purpose: the conversion loops carry one statement and the tests carry an independent reference implementation, so a mistake in either shows up as a disagreement. Keep both when changing a conversion.

## Testing approach

Tests run without audio hardware. The ffmpeg and sox cross-validation tests (`pcm/crossvalidate*_test.go`) skip when the tool is absent.

- **Regression tests.** New behaviour gets a test. A bug fix gets a regression test that fails before the fix. A test must be able to fail: break the line it pins and confirm it fails, and avoid fixtures where every path gives the same result.
- **Fuzzing.** CI runs a short smoke of `FuzzParseHeader` (`internal/riff`), `FuzzDecode`, `FuzzDecodeSeek`, `FuzzParseBext`, `FuzzRoundTrip` (`pcm`) and `FuzzConvertCompanded` (`internal/sample`). A `-fuzz` pattern that matches nothing passes without running, so run a new target in the package that defines it. New parsing paths get a fuzz target or a seed under `testdata/fuzz`. Discovered crashers are committed as regression data.
- **Cross-validation.** Anything that changes emitted bytes or decoded samples is checked against ffmpeg and sox over the supported depths and rates. Both companding laws are checked over all 256 codes.
- **Large files.** The 4 GiB behaviour is tested without writing 4 GiB (sparse or synthetic sinks and sources, `rf64_test.go`, `seek_unknown_length_test.go`). Keep new large-size tests off real disk.
- **Plan consistency.** `plan_consistency_test.go` pins that the header length the encoder plans agrees with its capacity check; a change to header layout or chunk placement extends it.
- **Architectures.** The suite also runs as `GOARCH=386` binaries, because size and offset arithmetic must hold on 32-bit.
- **Benchmarks.** `pcm/bench_test.go` and `internal/sample/bench_test.go`. Hot paths (convert loops, the pooled encode, the zero-copy decode) are allocation-sensitive; run `task bench` before and after a change there.

## Commands

Go version is read from `go.mod` (currently 1.26); the golangci-lint version is pinned in `.github/workflows/ci.yml` (`GOLANGCI_LINT_VERSION`) and must match the local toolchain, so the git hooks, `task lint` and CI run the same linter.

```
task build         # go build ./...
task test          # go test -v ./...
task test:short    # go test ./...
task test:race     # go test -race ./...
task bench         # all benchmarks with allocation counts
task vet           # go vet for amd64 and arm64
task lint          # golangci-lint run ./...
task tidy          # go mod tidy and verify
task check         # build, vet, lint, race tests
task hooks:install # git config core.hooksPath .githooks
```

Run `task check` before pushing. CI additionally runs the 386 test job, the vet and cross-build matrix (amd64, arm64, 386, arm; Linux, macOS, Windows), the fuzz smoke job, govulncheck and OpenSSF Scorecard.

## Conventions

- **Lint.** Config is `.golangci.yaml`; strict by default. gocritic runs the ruleguard matchers in `rules/` for modern-Go patterns.
- **No `//nolint` without a linter name and a reason.** If a linter is wrong often enough to silence globally, change `.golangci.yaml` with a comment saying why.
- **Errors.** Error strings carry the `go-wav:` prefix (`go-wav/pcm:` in the `pcm` package). Wrap with `%w`; use `errors.New` for constant messages.
- **Comments.** Explain why: the chunk layout and spec clause where it is not obvious, the failure being avoided, the measurement behind a constant. Match the existing density; the long rationale comments on `DefaultMaxDecodedBytes` and `maxPreSize` are the model for a limit's documentation.
- **Docs follow code.** A behaviour change updates the package doc (`doc.go`, `pcm/doc.go`, `internal/*/doc.go`), the README and the sentinel's comment in `errors.go` in the same change. The README and godoc make precise claims (aliasing, which formats are written, which limits apply); a sentence that is false for some format or container is a defect.
- **Commits** use an imperative subject with a type prefix (`feat:`, `fix:`, `docs:`, `build:`, `ci:`, `chore:`, `refactor:`, `test:`), optionally scoped (`feat(pcm): ...`), with `!` for a breaking change.
- **Git hooks.** `.githooks/pre-commit` runs gofmt and golangci-lint on staged Go files; `pre-push` guards the commit identity (only the GitHub noreply address is allowed). Do not bypass either.
- **Dependencies.** Dependabot keeps Go modules and GitHub Actions current. Workflow actions are pinned.

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a caller of the library would see.

Flag these, they are real defects here:

- Any break of the design rules above: cgo, a new runtime dependency, a parser that trusts a size or count from the file, an `int` offset or size that overflows on 32-bit.
- A panic, index error or unbounded allocation reachable from file bytes, in the reader, the `riff` parsers, `Bext` parsing or the decoder.
- A written header whose size fields disagree with the audio, or a path that returns success instead of `ErrTooLarge` when no correct size can be produced.
- A resource limit reported as `ErrCorruptStream`, or a corrupt file reported as a limit or as `ErrUnsupported`.
- A change to emitted bytes (header layout, `JUNK` or `ds64` sizing, `fmt` or extensible encoding, `bext` and `iXML` placement) without updated byte-pinning tests and cross-validation.
- A sample conversion that disagrees with the reference implementation in the tests, or that changes the 8-bit bias, 24-bit packing, float clamping or companded expansion rules.
- A documented aliasing, ownership or pooling guarantee that the code no longer keeps (a pooled encoder returned dirty, a returned slice aliasing when the doc says it copies).
- A test that cannot fail on the behaviour it names, or a doc sentence that is false for some container or format.

Intentional, do not flag:

- Narrowing truncation (arithmetic right shift, no rounding, no dither) for integer to integer conversion; float to integer rounds half away from zero after clamping, NaN maps to 0.
- `DefaultMaxDecodedBytes` (1 GiB), `maxPreSize` (32 MiB) and `riff.MaxChunkPayload`: measured trades, documented where they are defined.
- The sample rate ceiling of 2147483647 being tighter than the fmt field; it keeps the rate a positive `int` on 32-bit and applies on read and write alike.
- BW64 being read but not written, and the companding laws being read but not written.
- Tolerant reading (missing pad byte, bogus data size, trailing bytes) alongside strict writing.
- `TotalFrames` reporting what a truncated file declared.
- The `pcm` package name colliding with its siblings; callers alias the import.
- `Bext()` accepting values the encoder refuses.

## Things that do not belong in the repo

- Design docs, specs, plans, research notes and handoff prompts. `docs/` is gitignored on purpose; those live locally and in the private tracker.
- Raw captures or scratch media. Committed media is limited to small, scrubbed fixtures under `testdata/`.
- Build outputs (`*.test`, `*.out`, `*.exe`), coverage and profile files, `.serena/` and `.codegraph/`.
