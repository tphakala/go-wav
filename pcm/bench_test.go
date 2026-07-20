package pcm_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// benchClipFrames is three seconds of 48 kHz mono, which is the shape
// BirdNET-Go writes for every detection.
const benchClipFrames = 48000 * 3

func benchPayload(frames, channels, bits int) []byte {
	p := make([]byte, frames*channels*(bits/8))
	for i := range p {
		p[i] = byte(i * 13)
	}
	return p
}

// BenchmarkEncodeInterleavedClip is the one-shot path, the way a short clip is
// actually written.
func BenchmarkEncodeInterleavedClip(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := benchPayload(benchClipFrames, 1, 16)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if err := pcm.EncodeInterleaved(io.Discard, cfg, src); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncoderStreamWrite is the streaming path with an aligned buffer,
// which should be a pure pass-through to the sink.
func BenchmarkEncoderStreamWrite(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	chunk := benchPayload(4096, 1, 16)
	e, err := pcm.NewEncoder(io.Discard, cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	var written int64
	for b.Loop() {
		if _, err := e.Write(chunk); err != nil {
			b.Fatal(err)
		}
		// A non-seekable sink cannot grow past 4 GiB, which is the guard
		// working, so rebind before the stream gets there.
		if written += int64(len(chunk)); written > 1<<30 {
			if err := e.Reset(io.Discard, cfg); err != nil {
				b.Fatal(err)
			}
			written = 0
		}
	}
}

// BenchmarkEncoderStreamWriteUnaligned exercises the carry path, where a chunk
// does not end on a frame boundary.
func BenchmarkEncoderStreamWriteUnaligned(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	chunk := make([]byte, 4097) // deliberately not a multiple of 6
	e, err := pcm.NewEncoder(io.Discard, cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	var written int64
	for b.Loop() {
		if _, err := e.Write(chunk); err != nil {
			b.Fatal(err)
		}
		if written += int64(len(chunk)); written > 1<<30 {
			if err := e.Reset(io.Discard, cfg); err != nil {
				b.Fatal(err)
			}
			written = 0
		}
	}
}

// BenchmarkNewEncoder measures per-stream setup, which dominates when many
// short clips are written.
func BenchmarkNewEncoder(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := pcm.NewEncoder(io.Discard, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewEncoderDeclared is the same with a declared frame count, which
// takes the branch that probes whether the stream fits plain RIFF.
func BenchmarkNewEncoderDeclared(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1, TotalFrames: benchClipFrames}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := pcm.NewEncoder(io.Discard, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

func benchDecodeFixture(b *testing.B, cfg pcm.Config, frames int) []byte {
	b.Helper()
	src := benchPayload(frames, cfg.Channels, cfg.BitDepth)
	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, src); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkDecodePassThrough is the default decode path, which should copy
// stored bytes without converting.
func BenchmarkDecodePassThrough(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	file := benchDecodeFixture(b, cfg, benchClipFrames)
	sink := make([]byte, 64<<10)
	wantBytes := int64(benchClipFrames * cfg.Channels * (cfg.BitDepth / 8))
	b.SetBytes(wantBytes)
	b.ReportAllocs()
	for b.Loop() {
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
			b.Fatal(err)
		}
		var got int64
		for {
			n, rerr := d.Read(sink)
			got += int64(n)
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				b.Fatalf("read: %v", rerr)
			}
		}
		// Guard against a regression that fails on the first Read and would
		// otherwise report an absurd throughput while still exiting zero.
		if got != wantBytes {
			b.Fatalf("decoded %d bytes, want %d", got, wantBytes)
		}
	}
}

// BenchmarkDecodeConvert is the converting decode path.
func BenchmarkDecodeConvert(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1, Format: wav.SampleFormatFloat}
	file := benchDecodeFixture(b, cfg, benchClipFrames)
	sink := make([]byte, 64<<10)
	// The decoder converts to 16 bit, so the output is narrower than the source.
	const convertTo = 16
	wantBytes := int64(benchClipFrames * cfg.Channels * (convertTo / 8))
	b.SetBytes(wantBytes)
	b.ReportAllocs()
	for b.Loop() {
		d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(convertTo))
		if err != nil {
			b.Fatal(err)
		}
		var got int64
		for {
			n, rerr := d.Read(sink)
			got += int64(n)
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				b.Fatalf("read: %v", rerr)
			}
		}
		// Guard against a regression that fails on the first Read and would
		// otherwise report an absurd throughput while still exiting zero.
		if got != wantBytes {
			b.Fatalf("decoded %d bytes, want %d", got, wantBytes)
		}
	}
}

// BenchmarkParseHeader isolates header parsing, which runs once per file but
// is the whole cost of probing a large archive.
func BenchmarkParseHeader(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	file := benchDecodeFixture(b, cfg, 16)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := pcm.NewDecoder(bytes.NewReader(file)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecoderResetReuse measures the pooled path: one Decoder rebound to
// stream after stream. Reuse is what Reset exists for, so a buffer this drops
// instead of carrying shows up here as an allocation per stream.
func BenchmarkDecoderResetReuse(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	file := benchDecodeFixture(b, cfg, benchClipFrames)
	sink := make([]byte, 4096)

	var d pcm.Decoder
	b.SetBytes(int64(len(file)))
	b.ReportAllocs()
	for b.Loop() {
		if err := d.Reset(bytes.NewReader(file)); err != nil {
			b.Fatal(err)
		}
		for {
			_, rerr := d.Read(sink)
			if rerr != nil {
				break
			}
		}
	}
}
