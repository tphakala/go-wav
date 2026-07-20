//go:build linux || darwin

package pcm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	pcm "github.com/tphakala/go-wav/pcm"
)

// These tests hand ffmpeg and sox a file this package produced through the
// RF64Auto upgrade: a plain RIFF header with a JUNK reservation that was
// rewritten in place as RF64 once the stream passed 4 GiB. The rest of the
// cross-validation covers RF64Always, where the header is RF64 from its first
// byte and no rewrite ever happens, so nothing outside this file has ever
// checked that another implementation can read an upgraded header.
//
// The obstacle is that the upgrade only triggers past 4 GiB. The file here is
// therefore genuinely larger than 4 GiB but nearly all hole: the audio is
// streamed through a sink that materialises the header and one second of real
// audio at each end, and merely advances its offset for everything in between.
// The encoder is the real one, in RF64Auto with no declared frame count, so the
// JUNK chunk, the capacity check that flips the container, and UpgradeToRF64 all
// run exactly as they would for a recording that really was six hours long. The
// only thing that is faked is the payload in the middle, which lands on disk as
// a hole and reads back as digital silence.
//
// Cost when it does run, measured on ext4: 4295040080 bytes of apparent size
// against 380 KiB of blocks actually allocated, and under nine seconds of wall
// clock for the two tests together, each of which builds its own file. Building
// one is a quarter of a second and ffmpeg answers in about the same, because
// both seek; sox accounts for nearly all the rest, since it reads the hole
// through rather than seeking over it. It is still gated behind
// an opt-in environment variable, because a file with a 4 GiB apparent size is
// not something a routine "go test ./..." should produce unasked, and the
// sparseness that makes it cheap is a property of the filesystem rather than a
// guarantee.
//
// The file is Linux and macOS only. Verifying sparseness and free space needs
// syscall.Stat_t.Blocks and syscall.Statfs, which are spelled the same on both
// and differently or not at all elsewhere. Everywhere else the tests simply do
// not exist, which is the same outcome as skipping.

const (
	// rf64AutoEnv opts in. Nothing here runs without it, so neither CI nor a
	// routine local run ever creates a multi-gigabyte file by surprise.
	rf64AutoEnv = "GOWAV_RF64_CROSSVALIDATE"

	rf64AutoRate     = 48000
	rf64AutoChannels = 2
	rf64AutoDepth    = 16

	// rf64AutoBytesPerSecond is one second of the format above.
	rf64AutoBytesPerSecond = rf64AutoRate * rf64AutoChannels * rf64AutoDepth / 8

	// rf64AutoSeconds is the shortest whole number of seconds whose payload
	// cannot be described by a 32-bit size field. Whole seconds matter because
	// both tools are asked to seek to a position expressed in seconds, and an
	// integral boundary keeps that seek sample accurate.
	rf64AutoSeconds = 22370

	// rf64AutoDataSize is the resulting data chunk length, comfortably past
	// the 4294967295 bytes a 32-bit field can hold.
	rf64AutoDataSize = int64(rf64AutoSeconds) * rf64AutoBytesPerSecond

	// rf64AutoEdgeBytes is how much real audio is written at each end. One
	// second is enough to be a meaningful payload comparison and small enough
	// that the whole file costs a few hundred kilobytes of disk.
	rf64AutoEdgeBytes = rf64AutoBytesPerSecond

	// rf64AutoChunk is how much the hole is advanced per Write. Nothing is
	// copied, so this only sets the iteration count.
	rf64AutoChunk = 8 << 20

	// rf64AutoFreeSpace is the headroom demanded before starting. The test
	// really needs a fraction of this, but a filesystem that quietly declined
	// to punch the hole would need the full 4 GiB, and running out of space
	// half way through is a worse failure than a skip.
	rf64AutoFreeSpace = int64(1) << 30
)

// rf64AutoConfig is the format written. RF64Auto with no declared frame count
// is the streaming case: the length is unknown when the header goes out, so the
// header must be plain RIFF with a JUNK reservation and can only become RF64
// later.
func rf64AutoConfig() pcm.Config {
	return pcm.Config{
		SampleRate: rf64AutoRate,
		Channels:   rf64AutoChannels,
		BitDepth:   rf64AutoDepth,
		RF64:       pcm.RF64Auto,
	}
}

// requireRF64AutoOptIn skips unless this run explicitly asked for the large
// file, and says how to ask.
func requireRF64AutoOptIn(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("writes a file over 4 GiB; skipped under -short")
	}
	if os.Getenv(rf64AutoEnv) != "1" {
		t.Skipf("set %s=1 to cross-validate an upgraded RF64 file; it writes a sparse file "+
			"of about %d bytes apparent size", rf64AutoEnv, rf64AutoDataSize)
	}
}

// requireSparseSupport skips unless dir really does store a sparse file
// sparsely. It is checked rather than assumed: the answer depends on the
// filesystem behind the temporary directory, not on the operating system, and
// being wrong about it means writing 4 GiB for real.
func requireSparseSupport(t *testing.T, dir string) {
	t.Helper()
	const probeSize = 8 << 20

	path := filepath.Join(dir, "sparse-probe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("sparse probe: %v", err)
	}
	// One byte at the far end, so the file is probeSize long with nothing but
	// hole ahead of it.
	if _, err := f.WriteAt([]byte{1}, probeSize-1); err != nil {
		_ = f.Close()
		t.Fatalf("sparse probe write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("sparse probe close: %v", err)
	}
	defer func() {
		if err := os.Remove(path); err != nil {
			t.Errorf("removing the sparse probe: %v", err)
		}
	}()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("sparse probe stat: %v", err)
	}
	if fi.Size() != probeSize {
		t.Fatalf("sparse probe is %d bytes, want %d", fi.Size(), probeSize)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("cannot read the block count of a file here, so sparseness cannot be verified")
	}
	// st.Blocks is in 512 byte units by long standing convention, whatever the
	// filesystem's own block size is.
	allocated := st.Blocks * 512
	if allocated >= probeSize/2 {
		t.Skipf("%s does not store sparse files sparsely: an %d byte hole cost %d bytes",
			dir, probeSize, allocated)
	}
}

// requireFreeSpace skips when dir has less room than want.
func requireFreeSpace(t *testing.T, dir string, want int64) {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Skipf("cannot measure free space on %s: %v", dir, err)
	}
	free := st.Bavail * uint64(st.Bsize)
	if free < uint64(want) {
		t.Skipf("%s has %d bytes free, want at least %d", dir, free, want)
	}
}

// sparseSink is a seekable sink over a real file that can leave a stretch of
// the stream unwritten. While hole is set, Write accounts for its bytes and
// returns without touching the file, so the range becomes a hole once a later
// write extends the file past it.
//
// Writes go through WriteAt rather than the file's own offset, so the position
// this sink reports is the only one that matters and a skipped range cannot
// desynchronise it.
type sparseSink struct {
	f      *os.File
	pos    int64
	size   int64
	stored int64
	hole   bool
}

func (s *sparseSink) Write(p []byte) (int, error) {
	n := len(p)
	if !s.hole {
		var err error
		if n, err = s.f.WriteAt(p, s.pos); err != nil {
			return n, err
		}
		s.stored += int64(n)
	}
	s.pos += int64(n)
	if s.pos > s.size {
		s.size = s.pos
	}
	return n, nil
}

func (s *sparseSink) Seek(off int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = off
	case io.SeekCurrent:
		next = s.pos + off
	case io.SeekEnd:
		next = s.size + off
	default:
		return 0, fmt.Errorf("sparseSink: bad whence %d", whence)
	}
	if next < 0 {
		return 0, errors.New("sparseSink: negative position")
	}
	s.pos = next
	return s.pos, nil
}

// buildUpgradedRF64 writes the large file and returns its path along with the
// real audio placed at each end.
//
// Every assertion the caller needs about the file being a genuine upgrade is
// made here: that the header started as plain RIFF with a JUNK reservation, and
// that after Close it is RF64 with a correct ds64 where the JUNK used to be and
// the sentinel in both 32-bit size fields. Without that, a silently missing
// upgrade would leave the tools reading a plain RIFF file and every comparison
// below would pass while proving nothing.
func buildUpgradedRF64(t *testing.T, dir string) (path string, prefix, suffix []byte) {
	t.Helper()
	requireSparseSupport(t, dir)
	requireFreeSpace(t, dir, rf64AutoFreeSpace)

	path = filepath.Join(dir, "rf64_auto.wav")
	// Removed as soon as the test that built it is done, rather than at the
	// end of the whole run, and on failure as much as on success.
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removing the large fixture: %v", err)
		}
	})

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sink := &sparseSink{f: f}
	closeFile := func() {
		if err := f.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}

	e, err := pcm.NewEncoder(sink, rf64AutoConfig())
	if err != nil {
		closeFile()
		t.Fatalf("NewEncoder: %v", err)
	}
	headerLen := sink.pos
	assertReservedJUNK(t, path, headerLen)

	prefix = rf64AutoPattern(rf64AutoEdgeBytes, 7)
	suffix = rf64AutoPattern(rf64AutoEdgeBytes, 101)

	write := func(p []byte) {
		if _, err := e.Write(p); err != nil {
			closeFile()
			t.Fatalf("Write at offset %d: %v", sink.pos, err)
		}
	}

	write(prefix)
	sink.hole = true
	filler := make([]byte, rf64AutoChunk)
	for remaining := rf64AutoDataSize - 2*rf64AutoEdgeBytes; remaining > 0; {
		n := int64(len(filler))
		if n > remaining {
			n = remaining
		}
		write(filler[:n])
		remaining -= n
	}
	sink.hole = false
	write(suffix)

	if err := e.Close(); err != nil {
		closeFile()
		t.Fatalf("Close: %v", err)
	}
	closeFile()

	if got := sink.pos; got != headerLen+rf64AutoDataSize {
		t.Fatalf("stream ended at %d bytes, want %d", got, headerLen+rf64AutoDataSize)
	}
	t.Logf("wrote %d bytes apparent, %d bytes materialised", sink.size, sink.stored)

	assertUpgradedRF64(t, path)
	return path, prefix, suffix
}

// assertReservedJUNK checks the state the upgrade starts from, which is the
// half of the story a finished file can no longer show.
func assertReservedJUNK(t *testing.T, path string, headerLen int64) {
	t.Helper()
	head := readHead(t, path, headerLen)
	if got := magic(t, head); got != magicRIFF {
		t.Fatalf("header before any audio: magic is %q, want %q", got, magicRIFF)
	}
	if got := string(head[fileHeaderSize : fileHeaderSize+4]); got != idJUNKChunk {
		t.Fatalf("chunk after the file header: got %q, want the %q reservation", got, idJUNKChunk)
	}
}

// assertUpgradedRF64 checks that the finished file is RF64 rather than the
// plain RIFF it was written as.
func assertUpgradedRF64(t *testing.T, path string) {
	t.Helper()
	head := readHead(t, path, headKeep)
	if got := magic(t, head); got != magicRF64 {
		t.Fatalf("magic after Close: got %q want %q; the in place upgrade did not happen", got, magicRF64)
	}
	if got := string(head[fileHeaderSize : fileHeaderSize+4]); got != idDS64Chunk {
		t.Fatalf("chunk after the file header: got %q want %q; the JUNK reservation was not rewritten",
			got, idDS64Chunk)
	}
	// The magic, both sentinels, the ds64 payload and the data chunk header.
	assertRF64Shape(t, head, rf64AutoDataSize, rf64AutoFrames())
}

// readHead reads the first n bytes of a file.
func readHead(t *testing.T, path string, n int64) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close after reading the header: %v", err)
		}
	}()
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatalf("reading %d header bytes: %v", n, err)
	}
	return buf
}

// rf64AutoFrames is the inter-channel frame count of the whole stream.
func rf64AutoFrames() uint64 {
	return uint64(rf64AutoDataSize) / (rf64AutoChannels * rf64AutoDepth / 8)
}

// rf64AutoPattern builds a deterministic payload. The two ends are given
// different seeds so that a comparison cannot pass by reading the wrong one.
func rf64AutoPattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) + seed
	}
	return b
}

// TestFFmpegReadsOurUpgradedRF64 checks that ffmpeg agrees about a file whose
// RF64 header was written after the audio, not before it.
//
// The payload comparison is deliberately partial. Piping four gigabytes through
// a decoder to compare it against zeroes would cost minutes and prove nothing
// the three sampled regions do not: the first second and the last second are
// the real audio, and one second from the middle is the hole, which must read
// back as silence. What the sampled regions cannot show, the reported duration
// does, and that comes straight from the 64-bit ds64 size the upgrade wrote.
func TestFFmpegReadsOurUpgradedRF64(t *testing.T) {
	requireRF64AutoOptIn(t)
	ffmpeg := lookPath(t, "ffmpeg")
	ffprobe := lookPath(t, "ffprobe")

	path, prefix, suffix := buildUpgradedRF64(t, t.TempDir())

	probe := string(run(t, ffprobe, "-v", "error",
		"-show_entries", "stream=codec_name,sample_rate,channels,bits_per_sample,duration",
		"-of", "default=noprint_wrappers=1", path))
	want := map[string]string{
		"codec_name":      "pcm_s16le",
		"sample_rate":     strconv.Itoa(rf64AutoRate),
		"channels":        strconv.Itoa(rf64AutoChannels),
		"bits_per_sample": strconv.Itoa(rf64AutoDepth),
		"duration":        strconv.Itoa(rf64AutoSeconds) + ".000000",
	}
	for key, value := range want {
		if got := fieldOf(probe, key+"="); got != value {
			t.Errorf("ffprobe %s: got %q want %q\nfull output:\n%s", key, got, value, probe)
		}
	}

	decode := func(args ...string) []byte {
		full := append([]string{"-v", "error"}, args...)
		return run(t, ffmpeg, append(full, "-f", "s16le", "-c:a", "pcm_s16le", "-")...)
	}

	if got := decode("-i", path, "-t", "1"); !bytes.Equal(got, prefix) {
		t.Errorf("first second: ffmpeg decoded %d bytes, want the %d written", len(got), len(prefix))
	}
	// Seeking to the last second is also the only part of this that needs a
	// 64-bit file offset, since the position is past 4 GiB.
	if got := decode("-ss", strconv.Itoa(rf64AutoSeconds-1), "-i", path); !bytes.Equal(got, suffix) {
		t.Errorf("last second: ffmpeg decoded %d bytes, want the %d written", len(got), len(suffix))
	}
	mid := decode("-ss", strconv.Itoa(rf64AutoSeconds/2), "-i", path, "-t", "1")
	if len(mid) != rf64AutoEdgeBytes {
		t.Errorf("middle second: ffmpeg decoded %d bytes, want %d", len(mid), rf64AutoEdgeBytes)
	}
	if i := bytes.IndexFunc(mid, func(r rune) bool { return r != 0 }); i >= 0 {
		t.Errorf("middle second: byte %d is %#02x, want the hole to read as silence", i, mid[i])
	}
}

// TestSoxReadsOurUpgradedRF64 repeats the check with a second implementation,
// because agreeing with one reader is weaker evidence than agreeing with two,
// and sox parses ds64 with code that owes nothing to ffmpeg's.
func TestSoxReadsOurUpgradedRF64(t *testing.T) {
	requireRF64AutoOptIn(t)
	sox := lookPath(t, "sox")

	path, prefix, suffix := buildUpgradedRF64(t, t.TempDir())

	info := string(run(t, sox, "--i", path))
	for _, want := range []struct{ field, value string }{
		{"Channels", strconv.Itoa(rf64AutoChannels)},
		{"Sample Rate", strconv.Itoa(rf64AutoRate)},
		{"Precision", strconv.Itoa(rf64AutoDepth) + "-bit"},
		{"Sample Encoding", strconv.Itoa(rf64AutoDepth) + "-bit Signed Integer PCM"},
	} {
		if got := fieldOf(info, want.field); got != want.value {
			t.Errorf("sox %s: got %q want %q\nfull output:\n%s", want.field, got, want.value, info)
		}
	}
	// sox states the duration as a sample count as well as a clock time, and
	// the sample count is what the ds64 sampleCount field has to survive as.
	if got := fieldOf(info, "Duration"); !strings.Contains(got, strconv.FormatUint(rf64AutoFrames(), 10)+" samples") {
		t.Errorf("sox Duration: got %q, want %d samples\nfull output:\n%s", got, rf64AutoFrames(), info)
	}

	raw := func(trim ...string) []byte {
		args := append([]string{path, "-t", "raw", "-e", "signed-integer", "-b", "16", "-L", "-"}, trim...)
		return run(t, sox, args...)
	}

	if got := raw("trim", "0", "1"); !bytes.Equal(got, prefix) {
		t.Errorf("first second: sox decoded %d bytes, want the %d written", len(got), len(prefix))
	}
	if got := raw("trim", strconv.Itoa(rf64AutoSeconds-1)); !bytes.Equal(got, suffix) {
		t.Errorf("last second: sox decoded %d bytes, want the %d written", len(got), len(suffix))
	}
}

// fieldOf returns the value following the first occurrence of key on its own
// line, which covers both ffprobe's "key=value" and sox's "Key : value".
func fieldOf(out, key string) string {
	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), key)
		if !ok {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
	}
	return ""
}
