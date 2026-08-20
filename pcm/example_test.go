package pcm_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"

	wav "github.com/tphakala/go-wav"
	"github.com/tphakala/go-wav/pcm"
)

// le16 packs signed 16-bit samples as the little-endian bytes an [pcm.Encoder]
// expects for a 16-bit PCM stream. It is a test helper, so it does not appear in
// the rendered documentation.
func le16(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

// Example encodes a short stereo clip and decodes it straight back, the whole
// round trip a caller holding an in-memory buffer needs.
func Example() {
	// Two stereo frames of 16-bit PCM: L, R interleaved. Format defaults to
	// integer PCM, so only the three required fields are set.
	cfg := pcm.Config{SampleRate: 44100, Channels: 2, BitDepth: 16}
	samples := le16(1000, -1000, 2000, -2000)

	var buf bytes.Buffer
	if err := pcm.EncodeInterleaved(&buf, cfg, samples); err != nil {
		log.Fatal(err)
	}

	info, audio, err := pcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s %d Hz, %d ch, %d-bit, %d bytes of audio\n",
		info.Format, info.SampleRate, info.Channels, info.BitDepth, len(audio))
	// Output: pcm 44100 Hz, 2 ch, 16-bit, 8 bytes of audio
}

// ExampleEncoder streams samples through the io.WriteCloser interface, which is
// the path for a caller that produces audio incrementally rather than all at
// once. Close must be called for the header to be correct.
func ExampleEncoder() {
	var buf bytes.Buffer
	enc, err := pcm.NewEncoder(&buf, pcm.Config{SampleRate: 8000, Channels: 1, BitDepth: 16})
	if err != nil {
		log.Fatal(err)
	}

	// Any chunk size is accepted; a partial frame is carried to the next call.
	if _, err := enc.Write(le16(100, 200)); err != nil {
		log.Fatal(err)
	}
	if _, err := enc.Write(le16(300)); err != nil {
		log.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		log.Fatal(err)
	}

	info := enc.StreamInfo()
	fmt.Printf("%s, %d frames\n", info.Container, info.TotalFrames)
	// Output: RIFF, 3 frames
}

// ExampleDecoder reads audio through the io.Reader interface. A Decoder returns
// short reads and reports the end of the stream as io.EOF, so callers loop or
// hand it to a helper like io.ReadAll.
func ExampleDecoder() {
	// A small WAVE stream to read from.
	var buf bytes.Buffer
	cfg := pcm.Config{SampleRate: 16000, Channels: 1, BitDepth: 16}
	if err := pcm.EncodeInterleaved(&buf, cfg, le16(1, 2, 3, 4)); err != nil {
		log.Fatal(err)
	}

	dec, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Fatal(err)
	}
	audio, err := io.ReadAll(dec)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d Hz, %d bytes\n", dec.Info().SampleRate, len(audio))
	// Output: 16000 Hz, 8 bytes
}

// ExampleWithConvertTo normalises the encoding on the way out. An 8-bit WAV
// stores samples unsigned with a midpoint of 128; WithConvertTo hands them back
// as signed little-endian integers of the requested width, here 16-bit.
func ExampleWithConvertTo() {
	// Three 8-bit frames: the stored midpoint 128, then one above and one below.
	var buf bytes.Buffer
	cfg := pcm.Config{SampleRate: 8000, Channels: 1, BitDepth: 8}
	if err := pcm.EncodeInterleaved(&buf, cfg, []byte{128, 200, 56}); err != nil {
		log.Fatal(err)
	}

	dec, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()), pcm.WithConvertTo(16))
	if err != nil {
		log.Fatal(err)
	}
	audio, err := io.ReadAll(dec)
	if err != nil {
		log.Fatal(err)
	}

	// Info reports the converted width; the stored width stays in SourceBitDepth.
	fmt.Printf("%d-bit source -> %d-bit output\n", dec.Info().SourceBitDepth, dec.Info().BitDepth)
	for i := 0; i < len(audio); i += 2 {
		fmt.Print(int16(binary.LittleEndian.Uint16(audio[i:])), " ")
	}
	fmt.Println()
	// Output:
	// 8-bit source -> 16-bit output
	// 0 18432 -18432
}

// ExampleDecoder_SeekToFrame jumps to a frame before reading, the random-access
// path for a source that implements io.Seeker such as an *os.File or a
// bytes.Reader.
func ExampleDecoder_SeekToFrame() {
	// Five mono frames, one distinct sample each.
	var buf bytes.Buffer
	cfg := pcm.Config{SampleRate: 8000, Channels: 1, BitDepth: 16}
	if err := pcm.EncodeInterleaved(&buf, cfg, le16(10, 20, 30, 40, 50)); err != nil {
		log.Fatal(err)
	}

	dec, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Fatal(err)
	}

	reached, err := dec.SeekToFrame(3)
	if err != nil {
		log.Fatal(err)
	}
	audio, err := io.ReadAll(dec)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("reached frame %d\n", reached)
	for i := 0; i < len(audio); i += 2 {
		fmt.Print(int16(binary.LittleEndian.Uint16(audio[i:])), " ")
	}
	fmt.Println()
	// Output:
	// reached frame 3
	// 40 50
}

// ExampleDecoder_Bext reads the Broadcast Wave Format (bext) metadata chunk a
// stream carries, parsed into the same [pcm.Bext] an Encoder writes.
func ExampleDecoder_Bext() {
	// Encode a clip that carries a bext chunk.
	var buf bytes.Buffer
	cfg := pcm.Config{
		SampleRate: 48000,
		Channels:   1,
		BitDepth:   16,
		Bext:       &pcm.Bext{Description: "field recording", Originator: "go-wav"},
	}
	if err := pcm.EncodeInterleaved(&buf, cfg, le16(0, 0)); err != nil {
		log.Fatal(err)
	}

	dec, err := pcm.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Fatal(err)
	}
	bext, err := dec.Bext()
	if err != nil {
		log.Fatal(err)
	}
	if bext == nil {
		fmt.Println("no bext chunk")
		return
	}
	fmt.Printf("%q by %q\n", bext.Description, bext.Originator)
	// Output: "field recording" by "go-wav"
}

// ExampleEncoder_rf64 forces a 64-bit RF64 container up front, the shape a
// caller uses when a recording is expected to outgrow the 4 GiB limit of the
// 32-bit RIFF size fields. On a sink that cannot seek, RF64Always needs
// Config.TotalFrames because the sizes are written before any audio.
func ExampleEncoder_rf64() {
	var buf bytes.Buffer
	cfg := pcm.Config{
		SampleRate:  48000,
		Channels:    1,
		BitDepth:    16,
		RF64:        pcm.RF64Always,
		TotalFrames: 2,
	}
	enc, err := pcm.NewEncoder(&buf, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := enc.Write(le16(1, 2)); err != nil {
		log.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		log.Fatal(err)
	}

	container, ok := wav.SniffContainer(buf.Bytes())
	fmt.Println(container, ok)
	// Output: RF64 true
}
