package pcm_test

import (
	"bytes"
	"io"
	"testing"

	wav "github.com/tphakala/go-wav"
	pcm "github.com/tphakala/go-wav/pcm"
)

// benchClip is three seconds of 48 kHz mono 16-bit, which is the shape
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
	b.ResetTimer()
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
	b.ResetTimer()
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
	b.ResetTimer()
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
	b.ResetTimer()
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
	b.ResetTimer()
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
	b.SetBytes(int64(len(file)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		d, err := pcm.NewDecoder(bytes.NewReader(file))
		if err != nil {
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

// BenchmarkDecodeConvert is the converting decode path.
func BenchmarkDecodeConvert(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 32, Channels: 1, Format: wav.SampleFormatFloat}
	file := benchDecodeFixture(b, cfg, benchClipFrames)
	sink := make([]byte, 64<<10)
	b.SetBytes(int64(len(file)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		d, err := pcm.NewDecoder(bytes.NewReader(file), pcm.WithConvertTo(16))
		if err != nil {
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

// BenchmarkParseHeader isolates header parsing, which runs once per file but
// is the whole cost of probing a large archive.
func BenchmarkParseHeader(b *testing.B) {
	cfg := pcm.Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
	file := benchDecodeFixture(b, cfg, 16)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := pcm.NewDecoder(bytes.NewReader(file)); err != nil {
			b.Fatal(err)
		}
	}
}
