package riff

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

func le16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func le64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// chunk builds a well formed chunk: the four character identifier, the 32-bit
// payload size, the payload, and a single pad byte when the payload length is
// odd. The pad byte is not counted in the size field.
func chunk(id string, payload []byte) []byte {
	//nolint:gosec // G115: test payloads are far below 4 GiB.
	out := cat([]byte(id), le32(uint32(len(payload))), payload)
	if len(payload)%2 != 0 {
		out = append(out, 0x00)
	}
	return out
}

// chunkUnpadded builds a chunk that omits the pad byte even when the payload
// length is odd, which is the deviation tolerance rule 1 covers.
func chunkUnpadded(id string, payload []byte) []byte {
	//nolint:gosec // G115: test payloads are far below 4 GiB.
	return cat([]byte(id), le32(uint32(len(payload))), payload)
}

// chunkLying builds a chunk whose declared size differs from the payload that
// actually follows it. No pad byte is added.
func chunkLying(id string, declared uint32, payload []byte) []byte {
	return cat([]byte(id), le32(declared), payload)
}

// fileHeader builds the twelve byte file header.
func fileHeader(magic string, size uint32, form string) []byte {
	return cat([]byte(magic), le32(size), []byte(form))
}

// fmtPayload16 builds a 16-byte fmt payload.
func fmtPayload16(tag uint16, channels, rate, bits int) []byte {
	blockAlign := (bits + 7) / 8 * channels
	return cat(
		le16(tag),
		//nolint:gosec // G115: test values are small.
		le16(uint16(channels)),
		//nolint:gosec // G115: test values are small.
		le32(uint32(rate)),
		//nolint:gosec // G115: test values are small.
		le32(uint32(blockAlign*rate)),
		//nolint:gosec // G115: test values are small.
		le16(uint16(blockAlign)),
		//nolint:gosec // G115: test values are small.
		le16(uint16(bits)),
	)
}

// fmtPayload18 builds an 18-byte fmt payload with cbSize set to zero.
func fmtPayload18(tag uint16, channels, rate, bits int) []byte {
	return cat(fmtPayload16(tag, channels, rate, bits), le16(0))
}

// fmtPayload40 builds a 40-byte WAVE_FORMAT_EXTENSIBLE fmt payload.
func fmtPayload40(channels, rate, bits, validBits int, mask uint32, guid [16]byte) []byte {
	return cat(
		fmtPayload16(tagExtensible, channels, rate, bits),
		le16(fmtExtensibleCBSze),
		//nolint:gosec // G115: test values are small.
		le16(uint16(validBits)),
		le32(mask),
		guid[:],
	)
}

// ds64Payload builds a 28-byte ds64 payload with an empty chunk size table.
func ds64Payload(riffSize, dataSize, sampleCount uint64) []byte {
	return cat(le64(riffSize), le64(dataSize), le64(sampleCount), le32(0))
}

// parseBytes runs ParseHeader over a byte slice.
func parseBytes(b []byte) (*Header, error) {
	return ParseHeader(bufio.NewReader(bytes.NewReader(b)))
}

// mustHex decodes a hex string fixture, failing the test if it is malformed.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

// diffBytes reports the first differing offset between two slices, or -1.
func diffBytes(got, want []byte) int {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := range n {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return n
	}
	return -1
}

func assertBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if at := diffBytes(got, want); at >= 0 {
		t.Errorf("header bytes differ at offset %d\n got len=%d %x\nwant len=%d %x",
			at, len(got), got, len(want), want)
	}
}

// memFile is a bytes backed io.WriteSeeker for the patch and upgrade tests.
type memFile struct {
	buf []byte
	pos int64
}

func (m *memFile) Write(p []byte) (int, error) {
	need := m.pos + int64(len(p))
	if need > int64(len(m.buf)) {
		grown := make([]byte, need)
		copy(grown, m.buf)
		m.buf = grown
	}
	n := copy(m.buf[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *memFile) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = m.pos + off
	case io.SeekEnd:
		abs = int64(len(m.buf)) + off
	default:
		return 0, fmt.Errorf("memFile: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("memFile: negative position")
	}
	m.pos = abs
	return abs, nil
}

// writeAll writes the header and dataSize bytes of audio, padded to an even
// length, and returns the file.
func newFile(t *testing.T, lay *Layout, dataSize int64) *memFile {
	t.Helper()
	m := &memFile{}
	if _, err := m.Write(lay.Bytes); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := m.Write(make([]byte, padded(dataSize))); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// 1. Golden header bytes
// ---------------------------------------------------------------------------

const (
	guidPCMHex   = "0100000000001000800000aa00389b71"
	guidFloatHex = "0300000000001000800000aa00389b71"
)

// goldenCase is one hand computed header fixture.
type goldenCase struct {
	name       string
	cfg        HeaderConfig
	wantHex    string
	wantFmtLen uint32
	extensible bool
	wantGUID   string
	wantDS64   bool
	wantDataAt int64
}

// assertFmtChunk checks the fmt chunk of an emitted header against a fixture.
func assertFmtChunk(t *testing.T, lay *Layout, tc *goldenCase) {
	t.Helper()
	at := bytes.Index(lay.Bytes, []byte(idFmt))
	if at < 0 {
		t.Fatalf("no fmt chunk in header")
	}
	if got := binary.LittleEndian.Uint32(lay.Bytes[at+4:]); got != tc.wantFmtLen {
		t.Errorf("fmt payload size = %d, want %d", got, tc.wantFmtLen)
	}
	if tc.wantFmtLen == fmtSizeExtended {
		// The 18-byte form exists only to carry an empty cbSize.
		if cb := binary.LittleEndian.Uint16(lay.Bytes[at+8+16:]); cb != 0 {
			t.Errorf("cbSize = %d, want 0 for the 18-byte fmt form", cb)
		}
	}
	if !tc.extensible {
		return
	}
	if tag := binary.LittleEndian.Uint16(lay.Bytes[at+8:]); tag != tagExtensible {
		t.Errorf("format tag = 0x%04X, want 0x%04X", tag, tagExtensible)
	}
	if cb := binary.LittleEndian.Uint16(lay.Bytes[at+8+16:]); cb != fmtExtensibleCBSze {
		t.Errorf("cbSize = %d, want %d", cb, fmtExtensibleCBSze)
	}
	if got := hex.EncodeToString(lay.Bytes[at+8+24 : at+8+40]); got != tc.wantGUID {
		t.Errorf("SubFormat GUID = %s, want %s", got, tc.wantGUID)
	}
}

// assertReservedDS64 checks the ds64 or JUNK chunk an emitted header reserves.
func assertReservedDS64(t *testing.T, lay *Layout, want bool) {
	t.Helper()
	if !want {
		if lay.DS64Offset != -1 {
			t.Errorf("DS64Offset = %d, want -1", lay.DS64Offset)
		}
		return
	}
	if lay.DS64Offset != FileHeaderSize {
		t.Errorf("DS64Offset = %d, want %d", lay.DS64Offset, FileHeaderSize)
	}
	declared := binary.LittleEndian.Uint32(lay.Bytes[lay.DS64Offset+4:])
	if declared != DS64PayloadSize {
		t.Errorf("reserved chunk payload size = %d, want %d", declared, DS64PayloadSize)
	}
	// The chunk occupies exactly 36 bytes on the wire.
	if got := int64(ChunkHeaderSize) + int64(declared); got != DS64ChunkSize {
		t.Errorf("reserved chunk wire size = %d, want %d", got, DS64ChunkSize)
	}
	if DS64ChunkSize != 36 {
		t.Errorf("DS64ChunkSize = %d, want 36", DS64ChunkSize)
	}
}

func TestBuildHeaderGoldenBytes(t *testing.T) {
	tests := []goldenCase{
		{
			name: "pcm16_mono_44100_plain_riff",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRIFF,
			},
			wantHex: "52494646" + "24000000" + "57415645" +
				"666d7420" + "10000000" +
				"0100" + "0100" + "44ac0000" + "88580100" + "0200" + "1000" +
				"64617461" + "00000000",
			wantFmtLen: fmtSizePCM,
			wantDataAt: 44,
		},
		{
			name: "pcm24_stereo_48000_auto_extensible",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 24, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRIFF,
			},
			wantHex: "52494646" + "3c000000" + "57415645" +
				"666d7420" + "28000000" +
				"feff" + "0200" + "80bb0000" + "00650400" + "0600" + "1800" +
				"1600" + "1800" + "03000000" + guidPCMHex +
				"64617461" + "00000000",
			wantFmtLen: fmtSizeExtensible,
			extensible: true,
			wantGUID:   guidPCMHex,
			wantDataAt: 68,
		},
		{
			name: "pcm16_6ch_48000_auto_extensible",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 6, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRIFF,
			},
			wantHex: "52494646" + "3c000000" + "57415645" +
				"666d7420" + "28000000" +
				"feff" + "0600" + "80bb0000" + "00ca0800" + "0c00" + "1000" +
				"1600" + "1000" + "3f000000" + guidPCMHex +
				"64617461" + "00000000",
			wantFmtLen: fmtSizeExtensible,
			extensible: true,
			wantGUID:   guidPCMHex,
			wantDataAt: 68,
		},
		{
			name: "float32_mono_44100_with_fact",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 44100, Channels: 1, BitDepth: 32, Format: wav.SampleFormatFloat},
				Container: wav.ContainerRIFF,
				DataSize:  8,
				Frames:    2,
			},
			// A non-PCM encoding carries the cbSize field, so the fmt payload
			// is the 18-byte form even though the extension is empty.
			wantHex: "52494646" + "3a000000" + "57415645" +
				"666d7420" + "12000000" +
				"0300" + "0100" + "44ac0000" + "10b10200" + "0400" + "2000" + "0000" +
				"66616374" + "04000000" + "02000000" +
				"64617461" + "08000000",
			wantFmtLen: fmtSizeExtended,
			wantDataAt: 58,
		},
		{
			name: "rf64_pcm16_stereo_48000_real_ds64",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRF64,
				DataSize:  1 << 32,
				Frames:    1 << 30,
			},
			wantHex: "52463634" + "ffffffff" + "57415645" +
				"64733634" + "1c000000" +
				"4800000001000000" + "0000000001000000" + "0000004000000000" + "00000000" +
				"666d7420" + "10000000" +
				"0100" + "0200" + "80bb0000" + "00ee0200" + "0400" + "1000" +
				"64617461" + "ffffffff",
			wantFmtLen: fmtSizePCM,
			wantDS64:   true,
			wantDataAt: 80,
		},
		{
			name: "plain_riff_with_reserved_junk",
			cfg: HeaderConfig{
				Format:      Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container:   wav.ContainerRIFF,
				ReserveDS64: true,
			},
			wantHex: "52494646" + "48000000" + "57415645" +
				"4a554e4b" + "1c000000" +
				"0000000000000000" + "0000000000000000" + "0000000000000000" + "00000000" +
				"666d7420" + "10000000" +
				"0100" + "0100" + "44ac0000" + "88580100" + "0200" + "1000" +
				"64617461" + "00000000",
			wantFmtLen: fmtSizePCM,
			wantDS64:   true,
			wantDataAt: 80,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lay, err := BuildHeader(tc.cfg)
			if err != nil {
				t.Fatalf("BuildHeader: unexpected error %v", err)
			}
			want := mustHex(t, tc.wantHex)
			assertBytes(t, lay.Bytes, want)

			if lay.DataOffset != tc.wantDataAt {
				t.Errorf("DataOffset = %d, want %d", lay.DataOffset, tc.wantDataAt)
			}
			if int64(len(lay.Bytes)) != lay.DataOffset {
				t.Errorf("len(Bytes) = %d, want DataOffset %d", len(lay.Bytes), lay.DataOffset)
			}

			assertFmtChunk(t, lay, &tc)
			assertReservedDS64(t, lay, tc.wantDS64)
		})
	}
}

func TestBuildHeaderFactChunkOnlyForFloat(t *testing.T) {
	pcm, err := BuildHeader(HeaderConfig{
		Format: Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
	})
	if err != nil {
		t.Fatalf("BuildHeader pcm: %v", err)
	}
	if pcm.FactOffset != -1 {
		t.Errorf("pcm FactOffset = %d, want -1", pcm.FactOffset)
	}
	if bytes.Contains(pcm.Bytes, []byte(idFact)) {
		t.Errorf("pcm header contains a fact chunk: %x", pcm.Bytes)
	}

	flt, err := BuildHeader(HeaderConfig{
		Format: Format{SampleRate: 48000, Channels: 1, BitDepth: 32, Format: wav.SampleFormatFloat},
	})
	if err != nil {
		t.Fatalf("BuildHeader float: %v", err)
	}
	if flt.FactOffset < 0 {
		t.Fatalf("float FactOffset = %d, want a real offset", flt.FactOffset)
	}
	if got := string(flt.Bytes[flt.FactOffset-ChunkHeaderSize : flt.FactOffset-4]); got != idFact {
		t.Errorf("chunk before FactOffset = %q, want %q", got, idFact)
	}
}

// ---------------------------------------------------------------------------
// 2. riffSizeFor correctness
// ---------------------------------------------------------------------------

// assertSizeFields checks the size fields an emitted header carries on the
// wire against the expected data size and file header size.
func assertSizeFields(t *testing.T, lay *Layout, container wav.Container, dataSize, wantRIFF int64) {
	t.Helper()
	if container.Sized64() {
		p := lay.DS64Offset + int64(ChunkHeaderSize)
		if got := binary.LittleEndian.Uint64(lay.Bytes[p:]); int64(got) != wantRIFF {
			t.Errorf("ds64 riffSize = %d, want %d", got, wantRIFF)
		}
		if got := binary.LittleEndian.Uint64(lay.Bytes[p+8:]); int64(got) != dataSize {
			t.Errorf("ds64 dataSize = %d, want %d", got, dataSize)
		}
		return
	}
	if got := binary.LittleEndian.Uint32(lay.Bytes[lay.RIFFSizeOffset:]); int64(got) != wantRIFF {
		t.Errorf("RIFF size field = %d, want %d", got, wantRIFF)
	}
	// The data chunk size field counts the payload, never the pad byte.
	if got := binary.LittleEndian.Uint32(lay.Bytes[lay.DataSizeOffset:]); int64(got) != dataSize {
		t.Errorf("data chunk size field = %d, want %d (unpadded)", got, dataSize)
	}
}

func TestRIFFSizeForMatchesFileLength(t *testing.T) {
	sizes := []int64{0, 1, 2, 3, 100, 101, 4095, 4096}
	containers := []struct {
		name      string
		container wav.Container
	}{
		{"riff", wav.ContainerRIFF},
		{"rf64", wav.ContainerRF64},
	}

	for _, c := range containers {
		for _, n := range sizes {
			t.Run(fmt.Sprintf("%s_n%d", c.name, n), func(t *testing.T) {
				lay, err := BuildHeader(HeaderConfig{
					Format:    Format{SampleRate: 44100, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
					Container: c.container,
					DataSize:  n,
					//nolint:gosec // G115: n is non-negative.
					Frames: uint64(n / 4),
				})
				if err != nil {
					t.Fatalf("BuildHeader: %v", err)
				}

				want := lay.DataOffset + padded(n) - 8
				if got := riffSizeFor(lay, n); got != want {
					t.Fatalf("riffSizeFor(lay, %d) = %d, want %d", n, got, want)
				}

				// The emitted file is the header plus the padded audio, and the
				// size field must describe exactly that, less eight bytes.
				file := cat(lay.Bytes, make([]byte, padded(n)))
				if int64(len(file))-8 != want {
					t.Fatalf("file length %d less 8 = %d, want riffSize %d", len(file), len(file)-8, want)
				}

				assertSizeFields(t, lay, c.container, n, want)

				// A parser reading the emitted bytes agrees about the data size.
				h, err := parseBytes(file)
				if err != nil {
					t.Fatalf("ParseHeader: %v", err)
				}
				wantData := n
				if n == 0 {
					// A zero size field is indistinguishable from an unpatched
					// header, so the reader reports it as unknown in every
					// container, the 32-bit field and the ds64 alike.
					wantData = sizeUnknown
				}
				if h.DataSize != wantData {
					t.Errorf("parsed DataSize = %d, want %d", h.DataSize, wantData)
				}
			})
		}
	}
}

func TestPaddedHelper(t *testing.T) {
	cases := map[int64]int64{0: 0, 1: 2, 2: 2, 3: 4, 100: 100, 101: 102}
	for in, want := range cases {
		if got := padded(in); got != want {
			t.Errorf("padded(%d) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Round trip matrix
// ---------------------------------------------------------------------------

func TestRoundTripMatrix(t *testing.T) {
	formats := []struct {
		format wav.SampleFormat
		bits   int
	}{
		{wav.SampleFormatPCM, 8},
		{wav.SampleFormatPCM, 16},
		{wav.SampleFormatPCM, 24},
		{wav.SampleFormatPCM, 32},
		{wav.SampleFormatFloat, 32},
		{wav.SampleFormatFloat, 64},
	}
	channelCounts := []int{1, 2, 6, 8}
	rates := []int{8000, 44100, 48000, 192000, 384000}

	const frames = 11

	for _, f := range formats {
		for _, ch := range channelCounts {
			for _, rate := range rates {
				name := fmt.Sprintf("%s%d_%dch_%dHz", f.format, f.bits, ch, rate)
				t.Run(name, func(t *testing.T) {
					blockAlign := int64((f.bits+7)/8) * int64(ch)
					dataSize := frames * blockAlign

					cfg := HeaderConfig{
						Format: Format{
							SampleRate: rate,
							Channels:   ch,
							BitDepth:   f.bits,
							Format:     f.format,
						},
						Container: wav.ContainerRIFF,
						DataSize:  dataSize,
						Frames:    frames,
					}
					lay, err := BuildHeader(cfg)
					if err != nil {
						t.Fatalf("BuildHeader: %v", err)
					}

					file := cat(lay.Bytes, make([]byte, padded(dataSize)))
					h, err := parseBytes(file)
					if err != nil {
						t.Fatalf("ParseHeader: %v", err)
					}

					wantExtensible := ch > 2 || (f.format == wav.SampleFormatPCM && f.bits > 16)
					var wantMask uint32
					var wantValid int
					if wantExtensible {
						wantMask = ConventionalChannelMask(ch)
						wantValid = f.bits
					}

					want := wav.StreamInfo{
						SampleRate:     rate,
						Channels:       ch,
						BitDepth:       f.bits,
						SourceBitDepth: f.bits,
						ValidBits:      wantValid,
						Format:         f.format,
						SourceFormat:   f.format,
						Container:      wav.ContainerRIFF,
						Extensible:     wantExtensible,
						ChannelMask:    wantMask,
						TotalFrames:    frames,
					}
					if h.Info != want {
						t.Errorf("StreamInfo mismatch\n got %+v\nwant %+v", h.Info, want)
					}
					if h.DataSize != dataSize {
						t.Errorf("DataSize = %d, want %d", h.DataSize, dataSize)
					}
					if int64(h.BlockAlign) != blockAlign {
						t.Errorf("BlockAlign = %d, want %d", h.BlockAlign, blockAlign)
					}
					if h.DataSizeUnknown() {
						t.Errorf("DataSizeUnknown() = true, want false")
					}

					// The file header size field describes the whole file.
					gotRIFF := binary.LittleEndian.Uint32(file[4:])
					if int64(gotRIFF) != int64(len(file))-8 {
						t.Errorf("RIFF size field = %d, want %d", gotRIFF, len(file)-8)
					}
				})
			}
		}
	}
}

// sized64File builds a file whose sizes live in a ds64 chunk: the header
// BuildHeader emits, followed by the audio it describes.
//
// BuildHeader writes only the RF64 half of the pair, so a BW64 fixture is that
// same file with its magic swapped. For a file carrying no ADM metadata that is
// the whole difference between the two, which is also the reason BW64 is read
// here and never written.
func sized64File(t *testing.T, container wav.Container, dataSize int64, frames uint64) []byte {
	t.Helper()
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 96000, Channels: 2, BitDepth: 24, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRF64,
		DataSize:  dataSize,
		Frames:    frames,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	file := cat(lay.Bytes, make([]byte, padded(dataSize)))
	if container == wav.ContainerBW64 {
		copy(file[:4], idBW64)
	}
	return file
}

// assertSized64File parses a file carrying a ds64 chunk and checks everything
// the parser must report about it.
func assertSized64File(t *testing.T, file []byte, container wav.Container, dataSize int64, frames uint64) {
	t.Helper()
	h, err := parseBytes(file)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Info.Container != container {
		t.Errorf("Container = %v, want %v", h.Info.Container, container)
	}
	if h.DataSize != dataSize {
		t.Errorf("DataSize = %d, want %d", h.DataSize, dataSize)
	}
	if h.Info.TotalFrames != frames {
		t.Errorf("TotalFrames = %d, want %d", h.Info.TotalFrames, frames)
	}
}

func TestRoundTripRF64(t *testing.T) {
	const frames = 7
	dataSize := int64(frames) * 3 * 2
	assertSized64File(t, sized64File(t, wav.ContainerRF64, dataSize, frames), wav.ContainerRF64, dataSize, frames)
}

// TestParseBW64 is the read half of the round trip above for the container this
// package does not write. Nothing about parsing a BW64 stream differs from
// parsing an RF64 one, and this is what pins that.
func TestParseBW64(t *testing.T) {
	const frames = 7
	dataSize := int64(frames) * 3 * 2
	assertSized64File(t, sized64File(t, wav.ContainerBW64, dataSize, frames), wav.ContainerBW64, dataSize, frames)
}

func TestConventionalChannelMask(t *testing.T) {
	want := map[int]uint32{
		0: 0,
		1: 0x4,
		2: 0x3,
		3: 0x7,
		4: 0x33,
		5: 0x37,
		6: 0x3F,
		7: 0x70F,
		8: 0x63F,
		9: 0,
	}
	for ch, w := range want {
		if got := ConventionalChannelMask(ch); got != w {
			t.Errorf("ConventionalChannelMask(%d) = 0x%X, want 0x%X", ch, got, w)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. RF64 upgrade
// ---------------------------------------------------------------------------

// assertUpgradedWire checks the bytes an in place RF64 upgrade left on disk:
// the new magic, both 32-bit sentinels, and the ds64 chunk that replaced the
// reserved JUNK chunk.
func assertUpgradedWire(t *testing.T, buf []byte, lay *Layout, dataSize int64, frames uint64) {
	t.Helper()
	if got := string(buf[0:4]); got != idRF64 {
		t.Errorf("magic = %q, want %q", got, idRF64)
	}
	if got := binary.LittleEndian.Uint32(buf[4:]); got != sentinel32 {
		t.Errorf("file header size = 0x%08X, want 0x%08X", got, sentinel32)
	}
	if got := string(buf[lay.DS64Offset : lay.DS64Offset+4]); got != idDS64 {
		t.Errorf("reserved chunk id = %q, want %q", got, idDS64)
	}
	if got := binary.LittleEndian.Uint32(buf[lay.DS64Offset+4:]); got != DS64PayloadSize {
		t.Errorf("ds64 payload size = %d, want %d", got, DS64PayloadSize)
	}
	if got := binary.LittleEndian.Uint32(buf[lay.DataSizeOffset:]); got != sentinel32 {
		t.Errorf("data chunk size = 0x%08X, want 0x%08X", got, sentinel32)
	}

	p := lay.DS64Offset + int64(ChunkHeaderSize)
	wantRIFF := lay.DataOffset + padded(dataSize) - 8
	if got := binary.LittleEndian.Uint64(buf[p:]); int64(got) != wantRIFF {
		t.Errorf("ds64 riffSize = %d, want %d", got, wantRIFF)
	}
	if got := binary.LittleEndian.Uint64(buf[p+8:]); int64(got) != dataSize {
		t.Errorf("ds64 dataSize = %d, want %d", got, dataSize)
	}
	if got := binary.LittleEndian.Uint64(buf[p+16:]); got != frames {
		t.Errorf("ds64 sampleCount = %d, want %d", got, frames)
	}
	if got := binary.LittleEndian.Uint32(buf[p+24:]); got != 0 {
		t.Errorf("ds64 tableLength = %d, want 0", got)
	}
}

func TestUpgradeToRF64(t *testing.T) {
	const frames = 500
	blockAlign := int64(4)
	dataSize := frames * blockAlign

	lay, err := BuildHeader(HeaderConfig{
		Format:      Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container:   wav.ContainerRIFF,
		ReserveDS64: true,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	m := newFile(t, lay, dataSize)
	endBefore := m.pos

	if err := UpgradeToRF64(m, lay, wav.ContainerRF64, dataSize, frames); err != nil {
		t.Fatalf("UpgradeToRF64: %v", err)
	}
	if m.pos != endBefore {
		t.Errorf("position after upgrade = %d, want %d", m.pos, endBefore)
	}
	if int64(len(m.buf)) != lay.DataOffset+dataSize {
		t.Errorf("file length = %d, want %d", len(m.buf), lay.DataOffset+dataSize)
	}

	assertUpgradedWire(t, m.buf, lay, dataSize, frames)

	h, err := parseBytes(m.buf)
	if err != nil {
		t.Fatalf("ParseHeader after upgrade: %v", err)
	}
	if h.Info.Container != wav.ContainerRF64 {
		t.Errorf("parsed Container = %v, want %v", h.Info.Container, wav.ContainerRF64)
	}
	if h.DataSize != dataSize {
		t.Errorf("parsed DataSize = %d, want %d", h.DataSize, dataSize)
	}
	if h.Info.TotalFrames != frames {
		t.Errorf("parsed TotalFrames = %d, want %d", h.Info.TotalFrames, frames)
	}
	if h.DataSizeUnknown() {
		t.Errorf("DataSizeUnknown() = true, want false")
	}
}

func TestUpgradeToRF64Errors(t *testing.T) {
	t.Run("no_reserved_ds64_space", func(t *testing.T) {
		lay, err := BuildHeader(HeaderConfig{
			Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			Container: wav.ContainerRIFF,
		})
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		if lay.DS64Offset != -1 {
			t.Fatalf("DS64Offset = %d, want -1", lay.DS64Offset)
		}
		m := newFile(t, lay, 16)
		if err := UpgradeToRF64(m, lay, wav.ContainerRF64, 16, 8); err == nil {
			t.Fatalf("UpgradeToRF64 without reserved space returned nil error")
		}
	})

	// Both problems at once. The container is the caller's argument and the
	// reservation is a property of the header already built, so the argument
	// is what the error must name; reporting the missing ds64 space would
	// point at a header that is not the reason the call cannot succeed.
	t.Run("bad_container_reported_before_missing_ds64_space", func(t *testing.T) {
		lay, err := BuildHeader(HeaderConfig{
			Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			Container: wav.ContainerRIFF,
		})
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		if lay.DS64Offset != -1 {
			t.Fatalf("DS64Offset = %d, want -1", lay.DS64Offset)
		}
		m := newFile(t, lay, 16)
		before := bytes.Clone(m.buf)
		err = UpgradeToRF64(m, lay, wav.ContainerBW64, 16, 8)
		if err == nil {
			t.Fatalf("UpgradeToRF64 to BW64 without reserved space returned nil error")
		}
		if !strings.Contains(err.Error(), "RF64 is the only 64-bit container written") {
			t.Errorf("error = %v, want the container to be named", err)
		}
		if strings.Contains(err.Error(), "no ds64 space was reserved") {
			t.Errorf("error = %v, want the container problem rather than the reservation", err)
		}
		if !bytes.Equal(before, m.buf) {
			t.Errorf("a rejected upgrade modified the file")
		}
	})

	// RF64 is the only container the upgrade writes. Plain RIFF is not a
	// 64-bit container at all, and BW64 is the one this package reads without
	// ever writing it, so both must be refused with the file left alone.
	for _, container := range []wav.Container{wav.ContainerRIFF, wav.ContainerBW64} {
		t.Run("upgrade_to_"+container.String(), func(t *testing.T) {
			lay, err := BuildHeader(HeaderConfig{
				Format:      Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container:   wav.ContainerRIFF,
				ReserveDS64: true,
			})
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}
			m := newFile(t, lay, 16)
			before := bytes.Clone(m.buf)
			if err := UpgradeToRF64(m, lay, container, 16, 8); err == nil {
				t.Fatalf("UpgradeToRF64 to %s returned nil error", container)
			}
			if !bytes.Equal(before, m.buf) {
				t.Errorf("a rejected upgrade modified the file")
			}
		})
	}
}

func TestPlainRIFFWithJUNKStaysReadable(t *testing.T) {
	// A reserved but unused JUNK chunk must be skipped by the reader.
	const frames = 10
	dataSize := int64(frames * 2)
	lay, err := BuildHeader(HeaderConfig{
		Format:      Format{SampleRate: 44100, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container:   wav.ContainerRIFF,
		ReserveDS64: true,
		DataSize:    dataSize,
		Frames:      frames,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	h, err := parseBytes(cat(lay.Bytes, make([]byte, dataSize)))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Info.Container != wav.ContainerRIFF {
		t.Errorf("Container = %v, want RIFF", h.Info.Container)
	}
	if h.DataSize != dataSize {
		t.Errorf("DataSize = %d, want %d", h.DataSize, dataSize)
	}
	if h.Info.TotalFrames != frames {
		t.Errorf("TotalFrames = %d, want %d", h.Info.TotalFrames, frames)
	}
}

// ---------------------------------------------------------------------------
// 5. PatchSizes
// ---------------------------------------------------------------------------

func TestPatchSizes(t *testing.T) {
	tests := []struct {
		name      string
		container wav.Container
		reserve   bool
		bits      int
		format    wav.SampleFormat
		frames    uint64
	}{
		{"riff_pcm16", wav.ContainerRIFF, false, 16, wav.SampleFormatPCM, 1000},
		{"riff_pcm16_odd", wav.ContainerRIFF, false, 8, wav.SampleFormatPCM, 999},
		{"riff_float32_fact", wav.ContainerRIFF, false, 32, wav.SampleFormatFloat, 321},
		{"rf64_pcm16", wav.ContainerRF64, false, 16, wav.SampleFormatPCM, 4242},
		{"rf64_pcm24", wav.ContainerRF64, false, 24, wav.SampleFormatPCM, 77},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const channels = 1
			blockAlign := int64((tc.bits+7)/8) * channels
			//nolint:gosec // G115: frame counts here are small.
			dataSize := int64(tc.frames) * blockAlign

			lay, err := BuildHeader(HeaderConfig{
				Format: Format{
					SampleRate: 48000, Channels: channels, BitDepth: tc.bits, Format: tc.format,
				},
				Container:   tc.container,
				ReserveDS64: tc.reserve,
				DataSize:    0,
				Frames:      0,
			})
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}

			m := newFile(t, lay, dataSize)
			end := m.pos

			if err := PatchSizes(m, lay, tc.container, dataSize, tc.frames); err != nil {
				t.Fatalf("PatchSizes: %v", err)
			}
			if m.pos != end {
				t.Errorf("position after PatchSizes = %d, want %d (end of stream)", m.pos, end)
			}
			if int64(len(m.buf)) != end {
				t.Errorf("file length = %d, want %d", len(m.buf), end)
			}

			h, err := parseBytes(m.buf)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.DataSize != dataSize {
				t.Errorf("DataSize = %d, want %d", h.DataSize, dataSize)
			}
			if h.Info.TotalFrames != tc.frames {
				t.Errorf("TotalFrames = %d, want %d", h.Info.TotalFrames, tc.frames)
			}

			wantRIFF := lay.DataOffset + padded(dataSize) - 8
			if tc.container.Sized64() {
				p := lay.DS64Offset + int64(ChunkHeaderSize)
				//nolint:gosec // G115: wantRIFF is non-negative.
				if got := binary.LittleEndian.Uint64(m.buf[p:]); got != uint64(wantRIFF) {
					t.Errorf("ds64 riffSize = %d, want %d", got, wantRIFF)
				}
			} else {
				if got := binary.LittleEndian.Uint32(m.buf[lay.RIFFSizeOffset:]); int64(got) != wantRIFF {
					t.Errorf("RIFF size = %d, want %d", got, wantRIFF)
				}
			}

			if lay.FactOffset >= 0 {
				if got := uint64(binary.LittleEndian.Uint32(m.buf[lay.FactOffset:])); got != tc.frames {
					t.Errorf("fact frames = %d, want %d", got, tc.frames)
				}
			}
		})
	}
}

// BUG: PatchSizes accepts a 64-bit container for a header that was written
// with the plain RIFF magic and half upgrades the file: it stamps both 32-bit
// sentinels and fills the reserved chunk with ds64 values, but it cannot change
// the magic or the JUNK identifier, because those are UpgradeToRF64's job. The
// result parses as a plain RIFF whose data size is 0xFFFFFFFF, so the length
// the caller just patched in is reported as unknown and the ds64 payload is
// ignored. No error is returned.
//
// PatchSizes can detect this without any API change: lay.Bytes[0:4] still holds
// the magic that was written, so a sized64 container that disagrees with it is
// always a caller error.
func TestPatchSizesRejectsContainerMismatch(t *testing.T) {
	const frames = 1000
	const dataSize = int64(4000)

	lay, err := BuildHeader(HeaderConfig{
		Format:      Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container:   wav.ContainerRIFF,
		ReserveDS64: true,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	m := newFile(t, lay, dataSize)

	if err := PatchSizes(m, lay, wav.ContainerRF64, dataSize, frames); err != nil {
		return // Rejecting the mismatch is the correct behaviour.
	}

	// It accepted the mismatch, so the file it produced must at least be
	// correct and describe the size that was just patched in.
	if got := string(m.buf[0:4]); got != idRF64 {
		t.Errorf("PatchSizes with an RF64 container left the magic as %q; "+
			"the file claims 64-bit sizes it does not carry", got)
	}
	h, err := parseBytes(m.buf)
	if err != nil {
		t.Fatalf("ParseHeader after a mismatched PatchSizes: %v", err)
	}
	if h.DataSize != dataSize {
		t.Errorf("DataSize = %d, want %d: PatchSizes reported success but the size it wrote is unreadable",
			h.DataSize, dataSize)
	}
}

func TestPatchSizesLeavesOriginalLayoutIntact(t *testing.T) {
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	before := bytes.Clone(lay.Bytes)
	m := newFile(t, lay, 40)
	if err := PatchSizes(m, lay, wav.ContainerRIFF, 40, 10); err != nil {
		t.Fatalf("PatchSizes: %v", err)
	}
	if !bytes.Equal(before, lay.Bytes) {
		t.Errorf("PatchSizes mutated the caller's Layout.Bytes")
	}
}

// ---------------------------------------------------------------------------
// 6. Tolerance rules
// ---------------------------------------------------------------------------

// stdInfo is the StreamInfo the tolerance fixtures below all describe:
// 16-bit stereo at 44100 Hz in a plain RIFF container.
func stdFmtPayload() []byte { return fmtPayload16(tagPCM, 2, 44100, 16) }

func stdInfo(dataSize int64) wav.StreamInfo {
	var frames uint64
	if dataSize > 0 {
		//nolint:gosec // G115: dataSize is non-negative here.
		frames = uint64(dataSize / 4)
	}
	return wav.StreamInfo{
		SampleRate:     44100,
		Channels:       2,
		BitDepth:       16,
		SourceBitDepth: 16,
		Format:         wav.SampleFormatPCM,
		SourceFormat:   wav.SampleFormatPCM,
		Container:      wav.ContainerRIFF,
		TotalFrames:    frames,
	}
}

func TestTolerancePadByte(t *testing.T) {
	// An odd sized auxiliary chunk followed by the fmt chunk, once with the
	// pad byte the specification requires and once without it. Both layouts
	// must yield the same stream, which is the desync check.
	odd := []byte("odd payload!!") // 13 bytes, odd
	audio := make([]byte, 8)

	padded := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk("LIST", odd),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, audio),
	)
	unpadded := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunkUnpadded("LIST", odd),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, audio),
	)

	// Sanity: the two fixtures differ by exactly one byte of length.
	if len(padded)-len(unpadded) != 1 {
		t.Fatalf("fixture lengths %d and %d differ by %d, want 1", len(padded), len(unpadded), len(padded)-len(unpadded))
	}

	hp, err := parseBytes(padded)
	if err != nil {
		t.Fatalf("padded odd chunk: ParseHeader: %v", err)
	}
	hu, err := parseBytes(unpadded)
	if err != nil {
		t.Fatalf("unpadded odd chunk: ParseHeader: %v", err)
	}
	if hp.Info != hu.Info {
		t.Errorf("padded and unpadded layouts disagree\npadded   %+v\nunpadded %+v", hp.Info, hu.Info)
	}
	want := stdInfo(8)
	if hp.Info != want {
		t.Errorf("padded StreamInfo = %+v, want %+v", hp.Info, want)
	}
	if hp.DataSize != 8 || hu.DataSize != 8 {
		t.Errorf("DataSize padded=%d unpadded=%d, want 8", hp.DataSize, hu.DataSize)
	}
}

func TestTolerancePadByteBeforeDataChunk(t *testing.T) {
	// The same disambiguation, but with the odd chunk sitting between fmt and
	// data so that a desync would land on the data chunk header.
	odd := []byte{0x41, 0x42, 0x43} // 3 bytes, printable, odd
	audio := make([]byte, 12)

	for _, tc := range []struct {
		name  string
		build func() []byte
	}{
		{"with_pad", func() []byte {
			return cat(
				fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunk("bext", odd),
				chunk(idData, audio),
			)
		}},
		{"without_pad", func() []byte {
			return cat(
				fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunkUnpadded("bext", odd),
				chunk(idData, audio),
			)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseBytes(tc.build())
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.DataSize != 12 {
				t.Errorf("DataSize = %d, want 12", h.DataSize)
			}
			if want := stdInfo(12); h.Info != want {
				t.Errorf("StreamInfo = %+v, want %+v", h.Info, want)
			}
		})
	}
}

func TestToleranceOddFmtChunkPadByte(t *testing.T) {
	// An odd sized fmt chunk is not a thing the format allows, but an odd
	// sized chunk the parser buffers (fact) is. Check both pad layouts around
	// a fact chunk, which goes through readPayload rather than skipChunk.
	oddFact := []byte{0x0A, 0x00, 0x00, 0x00, 0x00} // 5 bytes: 10 frames plus a stray byte

	for _, tc := range []struct {
		name  string
		build func() []byte
	}{
		{"with_pad", func() []byte {
			return cat(
				fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 32)),
				chunk(idFact, oddFact),
				chunk(idData, make([]byte, 40)),
			)
		}},
		{"without_pad", func() []byte {
			return cat(
				fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 32)),
				chunkUnpadded(idFact, oddFact),
				chunk(idData, make([]byte, 40)),
			)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseBytes(tc.build())
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.DataSize != 40 {
				t.Errorf("DataSize = %d, want 40", h.DataSize)
			}
			if h.Info.TotalFrames != 10 {
				t.Errorf("TotalFrames = %d, want 10", h.Info.TotalFrames)
			}
		})
	}
}

func TestTolerancePadByteChained(t *testing.T) {
	// Several odd chunks in a row, so that a single byte of desync compounds.
	odds := [][]byte{[]byte("one"), []byte("three5"), []byte("sevenXXX"), []byte("z")}
	build := func(pad bool) []byte {
		out := fileHeader(idRIFF, 0, idWAVE)
		for i, p := range odds {
			id := fmt.Sprintf("ax%02d", i)
			if pad {
				out = cat(out, chunk(id, p))
			} else {
				out = cat(out, chunkUnpadded(id, p))
			}
		}
		return cat(out, chunk(idFmt, stdFmtPayload()), chunk(idData, make([]byte, 20)))
	}

	hp, err := parseBytes(build(true))
	if err != nil {
		t.Fatalf("padded chain: %v", err)
	}
	hu, err := parseBytes(build(false))
	if err != nil {
		t.Fatalf("unpadded chain: %v", err)
	}
	if hp.Info != hu.Info || hp.DataSize != hu.DataSize {
		t.Errorf("padded and unpadded chains disagree\npadded   %+v size=%d\nunpadded %+v size=%d",
			hp.Info, hp.DataSize, hu.Info, hu.DataSize)
	}
	if want := stdInfo(20); hp.Info != want {
		t.Errorf("StreamInfo = %+v, want %+v", hp.Info, want)
	}
}

func TestToleranceDataSizeZero(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, stdFmtPayload()),
		chunkLying(idData, 0, make([]byte, 64)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !h.DataSizeUnknown() {
		t.Errorf("DataSizeUnknown() = false, want true (DataSize = %d)", h.DataSize)
	}
	if h.DataSize != sizeUnknown {
		t.Errorf("DataSize = %d, want %d", h.DataSize, sizeUnknown)
	}
	if h.Info.TotalFrames != 0 {
		t.Errorf("TotalFrames = %d, want 0", h.Info.TotalFrames)
	}
}

func TestToleranceDataSizeSentinelInPlainRIFF(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, sentinel32, idWAVE),
		chunk(idFmt, stdFmtPayload()),
		chunkLying(idData, sentinel32, make([]byte, 64)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !h.DataSizeUnknown() {
		t.Errorf("DataSizeUnknown() = false, want true (DataSize = %d)", h.DataSize)
	}
}

func TestFrameCountFallbacks(t *testing.T) {
	t.Run("fact_chunk_when_data_size_unknown", func(t *testing.T) {
		b := cat(
			fileHeader(idRIFF, 0, idWAVE),
			chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 32)),
			chunk(idFact, le32(1234)),
			chunkLying(idData, 0, make([]byte, 400)),
		)
		h, err := parseBytes(b)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if !h.DataSizeUnknown() {
			t.Errorf("DataSizeUnknown() = false, want true")
		}
		if h.Info.TotalFrames != 1234 {
			t.Errorf("TotalFrames = %d, want 1234 from the fact chunk", h.Info.TotalFrames)
		}
	})

	t.Run("data_size_beats_fact_chunk", func(t *testing.T) {
		// The data chunk size describes bytes actually present, so it wins.
		b := cat(
			fileHeader(idRIFF, 0, idWAVE),
			chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 32)),
			chunk(idFact, le32(1234)),
			chunk(idData, make([]byte, 400)),
		)
		h, err := parseBytes(b)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if h.Info.TotalFrames != 100 {
			t.Errorf("TotalFrames = %d, want 100 from the 400 byte data chunk", h.Info.TotalFrames)
		}
	})

	t.Run("ds64_sample_count_when_data_size_unknown", func(t *testing.T) {
		b := cat(
			fileHeader(idRF64, sentinel32, idWAVE),
			chunk(idDS64, ds64Payload(999, 0, 4321)),
			chunk(idFmt, stdFmtPayload()),
			chunkLying(idData, sentinel32, make([]byte, 400)),
		)
		h, err := parseBytes(b)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if !h.DataSizeUnknown() {
			t.Errorf("DataSizeUnknown() = false, want true for a zero ds64 dataSize")
		}
		if h.Info.TotalFrames != 4321 {
			t.Errorf("TotalFrames = %d, want 4321 from the ds64 sampleCount", h.Info.TotalFrames)
		}
	})

	// A ds64 whose data size was never stamped but whose sample count was is
	// the combination that reaches the fallback, so it is also the combination
	// that reaches it carrying a count nothing has checked. TotalFrames is
	// exported, and the obvious uses of it (sizing a buffer, converting to
	// int64) overflow or panic on a value near the top of the range.
	t.Run("absurd_ds64_sample_count_is_reported_as_unknown", func(t *testing.T) {
		b := cat(
			fileHeader(idRF64, sentinel32, idWAVE),
			chunk(idDS64, ds64Payload(999, 0, math.MaxUint64)),
			chunk(idFmt, stdFmtPayload()),
			chunkLying(idData, sentinel32, make([]byte, 400)),
		)
		h, err := parseBytes(b)
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if !h.DataSizeUnknown() {
			t.Fatalf("DataSizeUnknown() = false, want true for a zero ds64 dataSize")
		}
		if h.Info.TotalFrames != 0 {
			t.Errorf("TotalFrames = %d, want 0 (unknown) for a sample count no stream could hold",
				h.Info.TotalFrames)
		}
	})
}

// TestResolveFramesBoundsDeclaredCounts pins the ceiling on the two fallbacks
// directly, because the fact chunk carries a 32-bit count that no file can push
// past the limit and that boundary is therefore unreachable through the parser.
// The limit is the maxDataSize ceiling resolveDataSize applies to a measured
// length, divided by the frame width: a declared count is credible only if the
// audio it claims could fit in a stream this format is able to describe.
func TestResolveFramesBoundsDeclaredCounts(t *testing.T) {
	t.Parallel()
	const blockAlign = 4
	const limit = maxDataSize / blockAlign

	tests := []struct {
		name       string
		dataSize   int64
		blockAlign int64
		ds64       ds64Info
		haveDS64   bool
		factFrames uint64
		want       uint64
	}{
		{
			// The measured length wins outright, so the ceiling never applies
			// to it. Stating that needs a declared count present and absurd:
			// with haveDS64 false there is nothing for the measured size to
			// beat, and the case would pass even if the precedence were
			// reversed.
			name:       "measured data size beats a declared count and this rule does not apply",
			dataSize:   400,
			blockAlign: blockAlign,
			ds64:       ds64Info{sampleCount: math.MaxUint64},
			haveDS64:   true,
			want:       100,
		},
		{
			name:       "sample count at the limit is kept",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			ds64:       ds64Info{sampleCount: limit},
			haveDS64:   true,
			want:       limit,
		},
		{
			name:       "sample count one past the limit is unknown",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			ds64:       ds64Info{sampleCount: limit + 1},
			haveDS64:   true,
			want:       0,
		},
		{
			name:       "all ones sample count is unknown",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			ds64:       ds64Info{sampleCount: math.MaxUint64},
			haveDS64:   true,
			want:       0,
		},
		{
			// An implausible declaration is no declaration, so the chain
			// should carry on to the next source rather than stopping at the
			// one it just rejected. Discarding a usable fact count because a
			// sibling field was corrupt loses information for nothing.
			name:       "a rejected sample count falls through to the fact chunk",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			ds64:       ds64Info{sampleCount: limit + 1},
			haveDS64:   true,
			factFrames: 1000,
			want:       1000,
		},
		{
			name:       "fact frames at the limit are kept",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			factFrames: limit,
			want:       limit,
		},
		{
			name:       "fact frames one past the limit are unknown",
			dataSize:   sizeUnknown,
			blockAlign: blockAlign,
			factFrames: limit + 1,
			want:       0,
		},
		{
			// With no frame width there is nothing to multiply by, so the
			// count is bounded on its own. maxDataSize remains the ceiling,
			// which is what keeps int64(TotalFrames) from going negative.
			name:       "unknown block align still bounds the count",
			dataSize:   sizeUnknown,
			blockAlign: 0,
			ds64:       ds64Info{sampleCount: math.MaxUint64},
			haveDS64:   true,
			want:       0,
		},
		{
			name:       "unknown block align keeps a credible count",
			dataSize:   sizeUnknown,
			blockAlign: 0,
			ds64:       ds64Info{sampleCount: 4321},
			haveDS64:   true,
			want:       4321,
		},
		{
			name:       "unknown block align keeps a count at the bare ceiling",
			dataSize:   sizeUnknown,
			blockAlign: 0,
			ds64:       ds64Info{sampleCount: maxDataSize},
			haveDS64:   true,
			want:       maxDataSize,
		},
		{
			name:       "unknown block align rejects one past the bare ceiling",
			dataSize:   sizeUnknown,
			blockAlign: 0,
			ds64:       ds64Info{sampleCount: maxDataSize + 1},
			haveDS64:   true,
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveFrames(tt.dataSize, tt.blockAlign, tt.ds64, tt.haveDS64, tt.factFrames)
			if got != tt.want {
				t.Errorf("resolveFrames(%d, %d, %+v, %v, %d) = %d, want %d",
					tt.dataSize, tt.blockAlign, tt.ds64, tt.haveDS64, tt.factFrames, got, tt.want)
			}
		})
	}
}

func TestToleranceDeclaredSizeBeyondEOF(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, stdFmtPayload()),
		chunkLying(idData, 0x7FFFFF00, make([]byte, 16)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: unexpected error %v", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ParseHeader returned io.ErrUnexpectedEOF")
	}
	if h.DataSize != 0x7FFFFF00 {
		t.Errorf("DataSize = %d, want %d", h.DataSize, int64(0x7FFFFF00))
	}
	if h.DataSizeUnknown() {
		t.Errorf("DataSizeUnknown() = true, want false")
	}
}

func TestToleranceTrailingGarbageAfterData(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, make([]byte, 32)),
		[]byte("\x00garbage that is not a chunk at all\xff\xfe"),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.DataSize != 32 {
		t.Errorf("DataSize = %d, want 32", h.DataSize)
	}
	if want := stdInfo(32); h.Info != want {
		t.Errorf("StreamInfo = %+v, want %+v", h.Info, want)
	}
}

func TestToleranceUnknownChunksEverywhere(t *testing.T) {
	list := cat([]byte("INFO"), chunk("INAM", []byte("a title")))
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idJUNK, make([]byte, 36)),
		chunk("LIST", list),
		chunk("bext", make([]byte, 602)),
		chunk(idFmt, stdFmtPayload()),
		chunk("iXML", []byte("<BWFXML></BWFXML>")),
		chunk("cue ", le32(0)),
		chunk("smpl", make([]byte, 36)),
		chunk(idData, make([]byte, 40)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.DataSize != 40 {
		t.Errorf("DataSize = %d, want 40", h.DataSize)
	}
	if want := stdInfo(40); h.Info != want {
		t.Errorf("StreamInfo = %+v, want %+v", h.Info, want)
	}
}

func TestToleranceChunkBeforeFmt(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk("PAD ", make([]byte, 100)),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, make([]byte, 8)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if want := stdInfo(8); h.Info != want {
		t.Errorf("StreamInfo = %+v, want %+v", h.Info, want)
	}
}

func TestToleranceFmt18Bytes(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, fmtPayload18(tagPCM, 2, 44100, 16)),
		chunk(idData, make([]byte, 16)),
	)
	if got := len(fmtPayload18(tagPCM, 2, 44100, 16)); got != fmtSizeExtended {
		t.Fatalf("fixture fmt payload = %d bytes, want %d", got, fmtSizeExtended)
	}
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Info.Extensible {
		t.Errorf("Extensible = true, want false for an 18-byte fmt with cbSize 0")
	}
	if want := stdInfo(16); h.Info != want {
		t.Errorf("StreamInfo = %+v, want %+v", h.Info, want)
	}
}

func TestToleranceFmt40BytesExtensible(t *testing.T) {
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, fmtPayload40(2, 48000, 24, 20, 0x3, guidPCM)),
		chunk(idData, make([]byte, 60)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if !h.Info.Extensible {
		t.Errorf("Extensible = false, want true")
	}
	if h.Info.ValidBits != 20 {
		t.Errorf("ValidBits = %d, want 20", h.Info.ValidBits)
	}
	if h.Info.ChannelMask != 0x3 {
		t.Errorf("ChannelMask = 0x%X, want 0x3", h.Info.ChannelMask)
	}
	if h.Info.BitDepth != 24 {
		t.Errorf("BitDepth = %d, want 24", h.Info.BitDepth)
	}
	if h.Info.Format != wav.SampleFormatPCM {
		t.Errorf("Format = %v, want pcm", h.Info.Format)
	}
}

func TestToleranceZeroBlockAlignIsRepaired(t *testing.T) {
	payload := stdFmtPayload()
	copy(payload[12:14], le16(0))
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idFmt, payload),
		chunk(idData, make([]byte, 40)),
	)
	h, err := parseBytes(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.BlockAlign != 4 {
		t.Errorf("BlockAlign = %d, want 4 (repaired)", h.BlockAlign)
	}
	if h.Info.TotalFrames != 10 {
		t.Errorf("TotalFrames = %d, want 10", h.Info.TotalFrames)
	}
}

// A chunk payload cut short by the end of the file used to be zero filled out
// to its declared length, so a truncated extensible fmt chunk parsed as though
// it carried an all-zero SubFormat GUID and was reported as ErrUnsupported
// rather than ErrCorruptStream. readPayload now hands back only the bytes that
// exist.
func TestTruncatedPayloadIsNotZeroFilled(t *testing.T) {
	full := fmtPayload40(2, 48000, 24, 24, 0x3, guidPCM)

	tests := []struct {
		name  string
		input []byte
		want  error
	}{
		{
			name:  "extensible_fmt_cut_at_24_bytes",
			input: cat(fileHeader(idRIFF, 0, idWAVE), chunkLying(idFmt, fmtSizeExtensible, full[:24])),
			want:  wav.ErrCorruptStream,
		},
		{
			name:  "pcm_fmt_cut_at_10_bytes",
			input: cat(fileHeader(idRIFF, 0, idWAVE), chunkLying(idFmt, fmtSizePCM, make([]byte, 10))),
			want:  wav.ErrCorruptStream,
		},
		{
			name:  "ds64_cut_at_8_bytes",
			input: cat(fileHeader(idRF64, sentinel32, idWAVE), chunkLying(idDS64, DS64PayloadSize, make([]byte, 8))),
			want:  wav.ErrCorruptStream,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBytes(tc.input)
			if !errors.Is(err, tc.want) {
				t.Errorf("ParseHeader error = %v, want errors.Is(err, %v)", err, tc.want)
			}
		})
	}
}

func TestBuildHeaderExplicitExtensibleFields(t *testing.T) {
	t.Run("explicit_channel_mask_and_valid_bits", func(t *testing.T) {
		const dataSize = 60
		lay, err := BuildHeader(HeaderConfig{
			Format: Format{
				SampleRate: 48000, Channels: 2, BitDepth: 24, ValidBits: 20,
				Format: wav.SampleFormatPCM, ChannelMask: 0x33,
			},
			Container: wav.ContainerRIFF, DataSize: dataSize, Frames: 10,
		})
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		h, err := parseBytes(cat(lay.Bytes, make([]byte, dataSize)))
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if !h.Info.Extensible {
			t.Errorf("Extensible = false, want true")
		}
		if h.Info.ValidBits != 20 {
			t.Errorf("ValidBits = %d, want 20", h.Info.ValidBits)
		}
		if h.Info.ChannelMask != 0x33 {
			t.Errorf("ChannelMask = 0x%X, want 0x33 (the caller's layout, not the conventional one)",
				h.Info.ChannelMask)
		}
	})

	t.Run("extensible_forced_on_float", func(t *testing.T) {
		const dataSize = 40
		lay, err := BuildHeader(HeaderConfig{
			Format: Format{
				SampleRate: 48000, Channels: 1, BitDepth: 32,
				Format: wav.SampleFormatFloat, Extensible: true,
			},
			Container: wav.ContainerRIFF, DataSize: dataSize, Frames: 10,
		})
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		at := bytes.Index(lay.Bytes, []byte(idFmt))
		if got := binary.LittleEndian.Uint32(lay.Bytes[at+4:]); got != fmtSizeExtensible {
			t.Errorf("fmt payload size = %d, want %d", got, fmtSizeExtensible)
		}
		if got := hex.EncodeToString(lay.Bytes[at+8+24 : at+8+40]); got != guidFloatHex {
			t.Errorf("SubFormat GUID = %s, want the IEEE float GUID %s", got, guidFloatHex)
		}
		if lay.FactOffset < 0 {
			t.Errorf("FactOffset = %d, want a fact chunk for a float stream", lay.FactOffset)
		}
		h, err := parseBytes(cat(lay.Bytes, make([]byte, dataSize)))
		if err != nil {
			t.Fatalf("ParseHeader: %v", err)
		}
		if h.Info.Format != wav.SampleFormatFloat || !h.Info.Extensible {
			t.Errorf("Format = %v Extensible = %v, want float and true", h.Info.Format, h.Info.Extensible)
		}
	})
}

func TestPlausibleFourCC(t *testing.T) {
	cases := []struct {
		in   []byte
		want bool
	}{
		{[]byte("fmt "), true},
		{[]byte("data"), true},
		{[]byte("LIST"), true},
		{[]byte{0x00, 'f', 'm', 't'}, false},
		{[]byte{'f', 'm', 't', 0x7F}, false},
		{[]byte{0x20, 0x7E, 0x20, 0x7E}, true},
		{[]byte("abc"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := plausibleFourCC(tc.in); got != tc.want {
			t.Errorf("plausibleFourCC(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. Rejections
// ---------------------------------------------------------------------------

func TestParseHeaderRejections(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  error
	}{
		{
			name:  "non_riff_magic",
			input: cat(fileHeader("OggS", 0, idWAVE), chunk(idFmt, stdFmtPayload())),
			want:  wav.ErrNotRIFF,
		},
		{
			name:  "riff_with_non_wave_form",
			input: cat(fileHeader(idRIFF, 0, "AVI "), chunk("strh", make([]byte, 8))),
			want:  wav.ErrNotRIFF,
		},
		{
			name:  "shorter_than_file_header",
			input: []byte("RIFF\x00\x00\x00\x00WAV"),
			want:  wav.ErrNotRIFF,
		},
		{
			name:  "empty_stream",
			input: nil,
			want:  wav.ErrNotRIFF,
		},
		{
			name: "fmt_of_10_bytes",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, make([]byte, 10)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "zero_channels",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, cat(le16(tagPCM), le16(0), le32(44100), le32(176400), le16(4), le16(16))),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "zero_sample_rate",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, cat(le16(tagPCM), le16(2), le32(0), le32(0), le16(4), le16(16))),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "no_fmt_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk("LIST", []byte("INFO")),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "no_data_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunk("LIST", []byte("INFO"))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "rf64_without_ds64",
			input: cat(fileHeader(idRF64, sentinel32, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunkLying(idData, sentinel32, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "bw64_without_ds64",
			input: cat(fileHeader(idBW64, sentinel32, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunkLying(idData, sentinel32, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			name: "rf64_with_short_ds64",
			input: cat(fileHeader(idRF64, sentinel32, idWAVE),
				chunk(idDS64, make([]byte, 20)),
				chunk(idFmt, stdFmtPayload()),
				chunkLying(idData, sentinel32, make([]byte, 8))),
			want: wav.ErrCorruptStream,
		},
		{
			// A-law and mu-law are decoded, but only at the one width G.711
			// defines them for; see TestParseCompandedRejectsOtherDepths for
			// the full sweep of widths.
			name: "alaw_at_16_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagALaw, 1, 8000, 16)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "mulaw_at_16_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagMuLaw, 1, 8000, 16)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "adpcm_format_tag",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(0x0011, 1, 8000, 4)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "extensible_with_unknown_guid",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload40(2, 48000, 16, 16, 0x3, [16]byte{
					0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
					0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
				})),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "pcm_at_12_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagPCM, 1, 48000, 12)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "pcm_at_20_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagPCM, 1, 48000, 20)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "float_at_16_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 16)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
		{
			name: "float_at_24_bits",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, fmtPayload16(tagIEEEFloat, 1, 48000, 24)),
				chunk(idData, make([]byte, 8))),
			want: wav.ErrUnsupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseBytes(tc.input)
			if err == nil {
				t.Fatalf("ParseHeader returned nil error and %+v, want %v", h, tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("ParseHeader error = %v, want errors.Is(err, %v)", err, tc.want)
			}
			if h != nil {
				t.Errorf("ParseHeader returned a non-nil Header alongside an error")
			}
		})
	}
}

func TestBuildHeaderRejections(t *testing.T) {
	tests := []struct {
		name string
		cfg  HeaderConfig
		want error
	}{
		{
			name: "pcm_at_20_bits",
			cfg:  HeaderConfig{Format: Format{SampleRate: 48000, Channels: 1, BitDepth: 20, Format: wav.SampleFormatPCM}},
			want: wav.ErrUnsupported,
		},
		{
			name: "float_at_16_bits",
			cfg: HeaderConfig{
				Format: Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatFloat},
			},
			want: wav.ErrUnsupported,
		},
		{
			name: "unknown_sample_format",
			cfg:  HeaderConfig{Format: Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormat(9)}},
			want: wav.ErrUnsupported,
		},
		{
			// The two companding laws are sample formats this package
			// parses, so the refusal to write one has to be deliberate
			// rather than a side effect of the unknown-format guard. Both
			// are given the 8 bits validateDepth accepts, so that the
			// rejection can only be coming from the write-side guard.
			name: "alaw_output",
			cfg: HeaderConfig{
				Format: Format{SampleRate: 8000, Channels: 1, BitDepth: 8, Format: wav.SampleFormatALaw},
			},
			want: wav.ErrUnsupported,
		},
		{
			name: "mulaw_output",
			cfg: HeaderConfig{
				Format: Format{SampleRate: 8000, Channels: 1, BitDepth: 8, Format: wav.SampleFormatMuLaw},
			},
			want: wav.ErrUnsupported,
		},
		{
			// BW64 is a container this package parses, so the refusal to
			// write one has to be deliberate rather than a side effect of
			// the unknown-container guard.
			name: "bw64_container",
			cfg: HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerBW64,
			},
			want: wav.ErrUnsupported,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildHeader(tc.cfg); !errors.Is(err, tc.want) {
				t.Errorf("BuildHeader error = %v, want errors.Is(err, %v)", err, tc.want)
			}
		})
	}

	t.Run("zero_channels", func(t *testing.T) {
		if _, err := BuildHeader(HeaderConfig{
			Format: Format{SampleRate: 48000, Channels: 0, BitDepth: 16, Format: wav.SampleFormatPCM},
		}); err == nil {
			t.Errorf("BuildHeader with zero channels returned nil error")
		}
	})
	t.Run("zero_sample_rate", func(t *testing.T) {
		if _, err := BuildHeader(HeaderConfig{
			Format: Format{SampleRate: 0, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
		}); err == nil {
			t.Errorf("BuildHeader with zero sample rate returned nil error")
		}
	})
	t.Run("negative_data_size", func(t *testing.T) {
		if _, err := BuildHeader(HeaderConfig{
			Format:   Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			DataSize: -1,
		}); err == nil {
			t.Errorf("BuildHeader with a negative data size returned nil error")
		}
	})
}

// ---------------------------------------------------------------------------
// 8. Overflow
// ---------------------------------------------------------------------------

func TestU32Overflow(t *testing.T) {
	tests := []struct {
		name    string
		in      int64
		want    uint32
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"one", 1, 1, false},
		{"max", maxUint32, 0xFFFFFFFF, false},
		{"max_minus_one", maxUint32 - 1, 0xFFFFFFFE, false},
		{"one_past_max", maxUint32 + 1, 0, true},
		{"far_past_max", maxUint32 * 3, 0, true},
		{"negative", -1, 0, true},
		{"min_int64", -1 << 62, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := u32("test", tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("u32(%d) = %d, nil; want wav.ErrTooLarge", tc.in, got)
				}
				if !errors.Is(err, wav.ErrTooLarge) {
					t.Errorf("u32(%d) error = %v, want errors.Is(err, wav.ErrTooLarge)", tc.in, err)
				}
				if got != 0 {
					t.Errorf("u32(%d) returned %d on error, want 0 (no wraparound)", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("u32(%d): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("u32(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	// A value one past the limit must not wrap to a small number.
	if got, err := u32("test", 1<<32); err == nil {
		t.Errorf("u32(1<<32) = %d with no error; a wrap to %d would be silent corruption", got, got)
	}
}

func TestU16Overflow(t *testing.T) {
	if _, err := u16("test", 65536); !errors.Is(err, wav.ErrTooLarge) {
		t.Errorf("u16(65536) error = %v, want wav.ErrTooLarge", err)
	}
	if got, err := u16("test", 65535); err != nil || got != 65535 {
		t.Errorf("u16(65535) = %d, %v; want 65535, nil", got, err)
	}
	if _, err := u16("test", -1); !errors.Is(err, wav.ErrTooLarge) {
		t.Errorf("u16(-1) error = %v, want wav.ErrTooLarge", err)
	}
}

func TestFitsRIFFBoundary(t *testing.T) {
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	// riffSizeFor is DataOffset + padded(dataSize) - 8, so the largest data
	// size that still fits is the one that drives the file header size field
	// exactly to 0xFFFFFFFF. DataOffset is 44 here, so the overhead is 36.
	overhead := lay.DataOffset - 8
	limit := maxUint32 - overhead
	if limit%2 != 0 {
		limit-- // padding would push an odd limit one byte over.
	}

	tests := []struct {
		name string
		size int64
		want bool
	}{
		{"zero", 0, true},
		{"small", 1024, true},
		{"negative", -1, false},
		{"limit_minus_two", limit - 2, true},
		{"limit", limit, true},
		{"limit_plus_one", limit + 1, false},
		{"limit_plus_two", limit + 2, false},
		{"four_gib", 1 << 32, false},
		{"past_uint32", maxUint32 + 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FitsRIFF(lay, tc.size); got != tc.want {
				t.Errorf("FitsRIFF(lay, %d) = %v, want %v (riffSizeFor = %d, max %d)",
					tc.size, got, tc.want, riffSizeFor(lay, tc.size), maxUint32)
			}
		})
	}

	// The boundary claim must agree with what BuildHeader will accept.
	if _, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
		DataSize:  limit,
	}); err != nil {
		t.Errorf("BuildHeader at the FitsRIFF limit %d failed: %v", limit, err)
	}
	if _, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
		DataSize:  limit + 2,
	}); !errors.Is(err, wav.ErrTooLarge) {
		t.Errorf("BuildHeader past the FitsRIFF limit: error = %v, want wav.ErrTooLarge", err)
	}
}

func TestBuildHeaderTooLargeForPlainRIFF(t *testing.T) {
	tests := []struct {
		name     string
		dataSize int64
	}{
		{"exactly_four_gib", 1 << 32},
		{"just_past_uint32", maxUint32 + 1},
		{"data_fits_but_riff_size_does_not", maxUint32 - 4},
		{"far_past", 1 << 40},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildHeader(HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRIFF,
				DataSize:  tc.dataSize,
				//nolint:gosec // G115: dataSize is non-negative.
				Frames: uint64(tc.dataSize / 4),
			})
			if !errors.Is(err, wav.ErrTooLarge) {
				t.Errorf("BuildHeader(DataSize=%d) error = %v, want wav.ErrTooLarge", tc.dataSize, err)
			}
		})
	}

	// The same sizes are fine in a 64-bit container.
	if _, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRF64,
		DataSize:  1 << 40,
		Frames:    1 << 38,
	}); err != nil {
		t.Errorf("BuildHeader RF64 at 1 TiB: unexpected error %v", err)
	}
}

func TestPatchSizesTooLargeForPlainRIFF(t *testing.T) {
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	m := newFile(t, lay, 16)
	before := bytes.Clone(m.buf)
	if err := PatchSizes(m, lay, wav.ContainerRIFF, 1<<33, 1<<31); !errors.Is(err, wav.ErrTooLarge) {
		t.Errorf("PatchSizes with an oversized data size: error = %v, want wav.ErrTooLarge", err)
	}
	if !bytes.Equal(before, m.buf) {
		t.Errorf("a rejected PatchSizes still wrote to the stream")
	}
}

func TestFactFrameCountClampsToSentinel(t *testing.T) {
	lay, err := BuildHeader(HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 32, Format: wav.SampleFormatFloat},
		Container: wav.ContainerRF64,
		DataSize:  1 << 35,
		Frames:    1 << 33,
	})
	if err != nil {
		t.Fatalf("BuildHeader: %v", err)
	}
	if got := binary.LittleEndian.Uint32(lay.Bytes[lay.FactOffset:]); got != sentinel32 {
		t.Errorf("fact frames = 0x%08X, want the 0x%08X sentinel for a count past 2^32", got, sentinel32)
	}
}

// ---------------------------------------------------------------------------
// 9. Allocation safety
// ---------------------------------------------------------------------------

func TestHugeDeclaredChunkSizeDoesNotAllocate(t *testing.T) {
	fixtures := []struct {
		name  string
		input []byte
	}{
		{
			name: "unknown_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunkLying("LIST", 0xFFFFFF00, []byte("INFO")),
				chunk(idFmt, stdFmtPayload()),
				chunk(idData, make([]byte, 8))),
		},
		{
			name: "fmt_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunkLying(idFmt, 0xFFFFFF00, stdFmtPayload()),
				chunk(idData, make([]byte, 8))),
		},
		{
			name: "ds64_chunk",
			input: cat(fileHeader(idRF64, sentinel32, idWAVE),
				chunkLying(idDS64, 0xFFFFFF00, make([]byte, 28)),
				chunk(idFmt, stdFmtPayload()),
				chunk(idData, make([]byte, 8))),
		},
		{
			name: "fact_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunkLying(idFact, 0xFFFFFFFF, le32(10)),
				chunk(idData, make([]byte, 8))),
		},
		{
			name: "data_chunk",
			input: cat(fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, stdFmtPayload()),
				chunkLying(idData, 0xFFFFFF00, make([]byte, 8))),
		},
	}

	const budget = 8 << 20 // 8 MiB, far below the 4 GiB the size fields claim.

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			// Parse repeatedly so that a per-call allocation proportional to
			// the declared size would be unmissable.
			for range 20 {
				// A panic here fails the test outright.
				_, _ = parseBytes(f.input)
			}

			runtime.ReadMemStats(&after)
			used := after.TotalAlloc - before.TotalAlloc
			if used > budget {
				t.Errorf("parsing a chunk declaring 0xFFFFFF00 bytes allocated %d bytes over 20 runs, want under %d",
					used, budget)
			}
		})
	}
}

func TestTruncatedFixturesDoNotPanic(t *testing.T) {
	full := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk(idJUNK, make([]byte, 28)),
		chunk(idFmt, fmtPayload40(2, 48000, 24, 24, 0x3, guidPCM)),
		chunk(idFact, le32(10)),
		chunk(idData, make([]byte, 60)),
	)
	for n := 0; n <= len(full); n++ {
		h, err := parseBytes(full[:n])
		if err == nil && h == nil {
			t.Fatalf("truncation at %d: nil error and nil header", n)
		}
	}
}

// ---------------------------------------------------------------------------
// Regressions. Each of these covers a defect this suite found in the
// implementation; the comment records the failure the test was written against.
// ---------------------------------------------------------------------------

// BuildHeader used to accept a Container value outside the defined set and emit
// a header whose magic was the seven byte string "unknown", shifting every
// subsequent offset and producing a file no parser could read. Container.String
// returns "unknown" for an unrecognised value and BuildHeader appended that
// string verbatim rather than a fixed four byte field, so the corruption was
// silent: wav.Container(99) yielded a 47-byte header with WAVE at offset 10 and
// no error at all.
func TestBuildHeaderRejectsUnknownContainer(t *testing.T) {
	for _, c := range []wav.Container{wav.Container(99), wav.Container(-1), wav.Container(3)} {
		lay, err := BuildHeader(HeaderConfig{
			Format:    Format{SampleRate: 48000, Channels: 1, BitDepth: 16, Format: wav.SampleFormatPCM},
			Container: c,
		})
		if err != nil {
			continue // Rejecting the container outright is the correct behaviour.
		}

		t.Errorf("BuildHeader accepted wav.Container(%d) and returned no error", int(c))
		if lay.RIFFSizeOffset != 4 {
			t.Errorf("RIFFSizeOffset = %d, want 4: the magic must occupy exactly four bytes",
				lay.RIFFSizeOffset)
		}
		if got := string(lay.Bytes[8:12]); got != idWAVE {
			t.Errorf("form type at offset 8 = %q, want %q; header bytes are %x", got, idWAVE, lay.Bytes)
		}
		if _, perr := parseBytes(cat(lay.Bytes, make([]byte, 8))); perr != nil {
			t.Errorf("the emitted header cannot be parsed back: %v", perr)
		}
	}
}

// An RF64 or BW64 stream whose ds64 dataSize is zero used to be reported as a
// zero length data chunk rather than an unknown one. That is exactly the state
// a header written by BuildHeader is in before PatchSizes runs, so a recording
// interrupted before its sizes were stamped read back as silence instead of
// being recovered by reading to EOF. The equivalent plain RIFF case was already
// handled: resolveDataSize applied the "zero means never patched" rule only to
// the 32-bit branch, never to the ds64 branch.
func TestRF64ZeroDS64DataSizeIsUnknown(t *testing.T) {
	for _, container := range []wav.Container{wav.ContainerRF64, wav.ContainerBW64} {
		t.Run(container.String(), func(t *testing.T) {
			lay, err := BuildHeader(HeaderConfig{
				Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
				Container: wav.ContainerRF64,
			})
			if err != nil {
				t.Fatalf("BuildHeader: %v", err)
			}
			// 1000 frames of audio reached the disk; the sizes never did.
			file := cat(lay.Bytes, make([]byte, 4000))
			// BW64 is read and never written, so its fixture is the RF64 one
			// with the magic swapped. The ds64 mechanics under test are the
			// same for both.
			if container == wav.ContainerBW64 {
				copy(file[:4], idBW64)
			}

			h, err := parseBytes(file)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if !h.DataSizeUnknown() {
				t.Errorf("DataSizeUnknown() = false with DataSize %d; want true so the caller reads to EOF. "+
					"4000 bytes of audio follow an unpatched all-zero ds64 chunk", h.DataSize)
			}
		})
	}
}

// A positive but wrong nBlockAlign used to be trusted verbatim. For every
// format this package supports, block align is fully determined by the channel
// count and the bit depth, so a value that disagrees with them is nonsensical
// and the package documentation promises it is "repaired from the other fmt
// fields when the stream declared a nonsensical value". parseFmt repaired only
// a block align of zero or less, so a 16-bit stereo file declaring 3 yielded
// BlockAlign 3 and, for 40 bytes of audio, 13 frames instead of 10.
func TestNonsensicalBlockAlignIsRepaired(t *testing.T) {
	tests := []struct {
		name           string
		channels, bits int
		declared       uint16
		dataSize       int
		wantAlign      int
		wantFrames     uint64
	}{
		{"stereo16_declares_3", 2, 16, 3, 40, 4, 10},
		{"stereo16_declares_2", 2, 16, 2, 40, 4, 10},
		{"mono24_declares_1", 1, 24, 1, 30, 3, 10},
		{"stereo16_declares_1024", 2, 16, 1024, 40, 4, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmtPayload16(tagPCM, tc.channels, 44100, tc.bits)
			copy(payload[12:14], le16(tc.declared))
			b := cat(
				fileHeader(idRIFF, 0, idWAVE),
				chunk(idFmt, payload),
				chunk(idData, make([]byte, tc.dataSize)),
			)
			h, err := parseBytes(b)
			if err != nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if h.BlockAlign != tc.wantAlign {
				t.Errorf("BlockAlign = %d, want %d repaired from %d channels at %d bits "+
					"(the file declared %d)", h.BlockAlign, tc.wantAlign, tc.channels, tc.bits, tc.declared)
			}
			if h.Info.TotalFrames != tc.wantFrames {
				t.Errorf("TotalFrames = %d, want %d for %d bytes of audio",
					h.Info.TotalFrames, tc.wantFrames, tc.dataSize)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// I/O error propagation
// ---------------------------------------------------------------------------

var errSink = errors.New("sink failure")

// failingSeeker fails the nth Seek call, counting from one.
type failingSeeker struct {
	memFile
	failSeekOn int
	seeks      int
	failWrite  bool
}

func (f *failingSeeker) Seek(off int64, whence int) (int64, error) {
	f.seeks++
	if f.seeks == f.failSeekOn {
		return 0, errSink
	}
	return f.memFile.Seek(off, whence)
}

func (f *failingSeeker) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, errSink
	}
	return f.memFile.Write(p)
}

func TestPatchSizesPropagatesSinkErrors(t *testing.T) {
	build := func(t *testing.T) *Layout {
		t.Helper()
		lay, err := BuildHeader(HeaderConfig{
			Format:      Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
			Container:   wav.ContainerRIFF,
			ReserveDS64: true,
		})
		if err != nil {
			t.Fatalf("BuildHeader: %v", err)
		}
		return lay
	}

	tests := []struct {
		name string
		sink func() *failingSeeker
	}{
		{"seek_current_fails", func() *failingSeeker { return &failingSeeker{failSeekOn: 1} }},
		{"seek_to_start_fails", func() *failingSeeker { return &failingSeeker{failSeekOn: 2} }},
		{"seek_back_to_end_fails", func() *failingSeeker { return &failingSeeker{failSeekOn: 3} }},
		{"write_fails", func() *failingSeeker { return &failingSeeker{failWrite: true} }},
	}

	for _, tc := range tests {
		t.Run("patch_"+tc.name, func(t *testing.T) {
			lay := build(t)
			w := tc.sink()
			if err := PatchSizes(w, lay, wav.ContainerRIFF, 4000, 1000); !errors.Is(err, errSink) {
				t.Errorf("PatchSizes error = %v, want errors.Is(err, errSink)", err)
			}
		})
		t.Run("upgrade_"+tc.name, func(t *testing.T) {
			lay := build(t)
			w := tc.sink()
			if err := UpgradeToRF64(w, lay, wav.ContainerRF64, 4000, 1000); !errors.Is(err, errSink) {
				t.Errorf("UpgradeToRF64 error = %v, want errors.Is(err, errSink)", err)
			}
		})
	}
}

// errAfterReader yields n bytes, then fails with a non-EOF error.
type errAfterReader struct {
	data []byte
	pos  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errSink
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestParseHeaderPropagatesSourceErrors(t *testing.T) {
	full := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunk("LIST", make([]byte, 4000)),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, make([]byte, 8)),
	)

	// Cut at points that land inside each of the reader's consumption paths:
	// the file header, a chunk header, a buffered payload and a discarded one.
	// A cut past the data chunk header is not included, since the header is
	// complete by then and parsing it is correct.
	for _, cut := range []int{0, 6, 14, 20, 2000, len(full) - 10} {
		t.Run(fmt.Sprintf("cut_at_%d", cut), func(t *testing.T) {
			r := bufio.NewReader(&errAfterReader{data: full[:cut]})
			h, err := ParseHeader(r)
			if err == nil {
				t.Fatalf("ParseHeader succeeded on a failing source and returned %+v", h)
			}
			if h != nil {
				t.Errorf("ParseHeader returned a header alongside an error")
			}
		})
	}
}

func TestDiscardNTolerates32BitSizes(t *testing.T) {
	// A chunk claiming close to 4 GiB must be walked without overflowing int
	// on a 32-bit target and without reading 4 GiB.
	b := cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunkLying("LIST", 0xFFFFFFFF, []byte("INFO")),
	)
	br := bufio.NewReader(bytes.NewReader(b))
	if _, err := ParseHeader(br); !errors.Is(err, wav.ErrCorruptStream) {
		t.Errorf("ParseHeader error = %v, want wav.ErrCorruptStream (no fmt chunk)", err)
	}

	// The helper itself, straight through.
	br2 := bufio.NewReader(bytes.NewReader(make([]byte, 100)))
	if err := discardN(br2, 0xFFFFFFFF); err != nil {
		t.Errorf("discardN past the end of the stream: %v", err)
	}
	br3 := bufio.NewReader(bytes.NewReader(make([]byte, 100)))
	if err := discardN(br3, 40); err != nil {
		t.Errorf("discardN(40): %v", err)
	}
	if got := br3.Buffered(); got != 60 {
		t.Errorf("after discardN(40) the reader has %d bytes buffered, want 60", got)
	}
}

// ---------------------------------------------------------------------------
// 10. Fuzz
// ---------------------------------------------------------------------------

func FuzzParseHeader(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("RIFF"))
	f.Add(cat(fileHeader(idRIFF, 0, idWAVE)))
	f.Add(cat(
		fileHeader(idRIFF, 36, idWAVE),
		chunk(idFmt, stdFmtPayload()),
		chunk(idData, make([]byte, 8)),
	))
	f.Add(cat(
		fileHeader(idRF64, sentinel32, idWAVE),
		chunk(idDS64, ds64Payload(100, 40, 10)),
		chunk(idFmt, stdFmtPayload()),
		chunkLying(idData, sentinel32, make([]byte, 40)),
	))
	f.Add(cat(
		fileHeader(idBW64, sentinel32, idWAVE),
		chunk(idDS64, ds64Payload(1<<40, 1<<39, 1<<37)),
		chunk(idFmt, fmtPayload40(6, 96000, 32, 24, 0x3F, guidFloat)),
		chunk(idFact, le32(1000)),
		chunkLying(idData, sentinel32, make([]byte, 16)),
	))
	f.Add(cat(
		fileHeader(idRIFF, 0, idWAVE),
		chunkUnpadded("LIST", []byte("INFOxyz")),
		chunk(idFmt, fmtPayload18(tagIEEEFloat, 1, 48000, 32)),
		chunk(idData, make([]byte, 7)),
	))

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			if h != nil {
				t.Fatalf("error %v returned alongside a non-nil header", err)
			}
			return
		}
		if h == nil {
			t.Fatalf("nil error and nil header")
		}
		if h.Info.SampleRate <= 0 {
			t.Fatalf("nil error but SampleRate = %d", h.Info.SampleRate)
		}
		if h.Info.Channels <= 0 {
			t.Fatalf("nil error but Channels = %d", h.Info.Channels)
		}
		if h.BlockAlign <= 0 {
			t.Fatalf("nil error but BlockAlign = %d", h.BlockAlign)
		}
		if h.DataSize < sizeUnknown {
			t.Fatalf("nil error but DataSize = %d", h.DataSize)
		}
		if h.DataSizeUnknown() != (h.DataSize == sizeUnknown) {
			t.Fatalf("DataSizeUnknown() disagrees with DataSize %d", h.DataSize)
		}
	})
}
