package wav_test

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	wav "github.com/tphakala/go-wav"
)

// header builds a 12-byte WAVE-style header: a four-byte magic, a four-byte
// size field Sniff does not inspect, and a four-byte form type. magic and form
// must each be exactly four bytes, and the helper enforces that rather than
// documenting it: a three-character argument would silently build an 11-byte
// slice, so a case meant to test form-type rejection would pass through Sniff's
// length guard instead and prove nothing.
func header(tb testing.TB, magic, form string) []byte {
	tb.Helper()
	if len(magic) != 4 || len(form) != 4 {
		tb.Fatalf("header(%q, %q): both arguments must be exactly four bytes", magic, form)
	}
	b := make([]byte, 0, 12)
	b = append(b, magic...)
	b = append(b, 0, 0, 0, 0)
	b = append(b, form...)
	return b
}

// unknownName is what both String methods document for a value outside their
// declared set. It is shared by the Container and SampleFormat tables so that
// the two default arms are pinned to one spelling.
const unknownName = "unknown"

// The two rows every enum table below ends with. Naming them keeps the tripwire
// row recognisable as the same construct wherever it appears: the value one
// past the last declared member, which becomes a real member the moment one is
// added and fails the row that still expects it to be unknown.
const (
	tripwireName   = "one past the last declared member"
	outOfRangeName = "out of range value"
)

// TestSniff pins Sniff's positive and negative cases: the three recognised
// containers, and the ways a header can fail to be one of them. Sniff exists
// so that callers do not have to hand-roll a magic check that forgets RF64
// and BW64, so the RF64 and BW64 cases matter as much as plain RIFF.
func TestSniff(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
		want bool
	}{
		{"nil slice", nil, false},
		{"empty slice", []byte{}, false},
		{"11 bytes, one short of the header", header(t, "RIFF", "WAVE")[:11], false},
		{"exactly 12 bytes, valid RIFF/WAVE", header(t, "RIFF", "WAVE"), true},
		{"exactly 12 bytes, valid RF64/WAVE", header(t, "RF64", "WAVE"), true},
		{"exactly 12 bytes, valid BW64/WAVE", header(t, "BW64", "WAVE"), true},
		{"RIFF magic, form type is not WAVE", header(t, "RIFF", "AVI "), false},
		{"unknown magic, form type WAVE", header(t, "JUNK", "WAVE"), false},
		{"12 bytes, form type WAVE, magic neither RIFF, RF64 nor BW64", header(t, "OggS", "WAVE"), false},
		// RIFF identifiers are case-sensitive four-character FOURCCs, so a
		// lowercase magic or form type is a different identifier, not the same
		// one spelled differently. These two rows are what fails if the
		// comparison is ever loosened to strings.EqualFold or routed through
		// strings.ToUpper.
		{"lowercase magic is not RIFF", header(t, "riff", "WAVE"), false},
		{"lowercase form type is not WAVE", header(t, "RIFF", "wave"), false},
		{
			"extra trailing bytes after a valid header are ignored",
			append(header(t, "RIFF", "WAVE"), []byte{'f', 'm', 't', ' ', 1, 2, 3, 4}...),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wav.Sniff(tt.b); got != tt.want {
				t.Errorf("Sniff(%q) = %v, want %v", tt.b, got, tt.want)
			}
		})
	}
}

// TestSniffRejectsShortSlices pins that every length under the twelve-byte
// header is rejected, not just a couple of spot checks. It uses a slice that
// would otherwise be a valid RIFF/WAVE header, truncated to each length in
// turn, so a false positive can only come from the length guard itself.
func TestSniffRejectsShortSlices(t *testing.T) {
	full := header(t, "RIFF", "WAVE")
	for n := range len(full) {
		t.Run(fmt.Sprintf("%d_bytes", n), func(t *testing.T) {
			if got := wav.Sniff(full[:n]); got {
				t.Errorf("Sniff of a %d-byte slice = true, want false", n)
			}
		})
	}
}

// TestStreamInfoBytesPerSample pins the ceiling division across every bit
// depth the package supports, plus a width that is not a multiple of eight.
// The contract is a storage width in whole bytes, so a bit depth that does not
// fill its last byte still costs that byte. Every supported depth is a
// multiple of eight, which means those rows alone cannot tell ceiling division
// from truncating division; the 20-bit row is the one that can. Twenty bits is
// the natural choice because the ValidBits documentation already uses 20-bit
// audio as its worked example.
func TestStreamInfoBytesPerSample(t *testing.T) {
	tests := []struct {
		name     string
		bitDepth int
		want     int
	}{
		{"zero value costs no bytes", 0, 0},
		{"8-bit", 8, 1},
		{"16-bit", 16, 2},
		{"20-bit rounds up to a whole byte", 20, 3},
		{"24-bit", 24, 3},
		{"32-bit", 32, 4},
		{"64-bit", 64, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			si := wav.StreamInfo{BitDepth: tt.bitDepth}
			if got := si.BytesPerSample(); got != tt.want {
				t.Errorf("BytesPerSample() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestStreamInfoBytesPerFrame pins BytesPerFrame across bit depth and channel
// count, the value a WAVE fmt chunk records as nBlockAlign. A frame is a whole
// number of per-channel samples, so the width is the per-sample byte count
// times the channel count, which means the rounding up happens once per sample
// and not once per frame. The 20-bit rows are the ones that say so: rounding
// per frame instead, as si.BitDepth/8*si.Channels would, gives 4 rather than 6
// for 20-bit stereo.
func TestStreamInfoBytesPerFrame(t *testing.T) {
	tests := []struct {
		name     string
		bitDepth int
		channels int
		want     int
	}{
		{"zero value", 0, 0, 0},
		{"20-bit mono", 20, 1, 3},
		{"20-bit stereo", 20, 2, 6},
		{"20-bit 8ch", 20, 8, 24},
		{"8-bit mono", 8, 1, 1},
		{"8-bit stereo", 8, 2, 2},
		{"8-bit 8ch", 8, 8, 8},
		{"16-bit mono", 16, 1, 2},
		{"16-bit stereo", 16, 2, 4},
		{"16-bit 8ch", 16, 8, 16},
		{"24-bit mono", 24, 1, 3},
		{"24-bit stereo", 24, 2, 6},
		{"24-bit 8ch", 24, 8, 24},
		{"32-bit mono", 32, 1, 4},
		{"32-bit stereo", 32, 2, 8},
		{"32-bit 8ch", 32, 8, 32},
		{"64-bit mono", 64, 1, 8},
		{"64-bit stereo", 64, 2, 16},
		{"64-bit 8ch", 64, 8, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			si := wav.StreamInfo{BitDepth: tt.bitDepth, Channels: tt.channels}
			if got := si.BytesPerFrame(); got != tt.want {
				t.Errorf("BytesPerFrame() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestStreamInfoDuration pins Duration's guards, exact and inexact frame
// counts, and the whole-seconds-plus-remainder split that keeps a large frame
// count from overflowing int64 the way a naive frames * time.Second would.
//
// The oversized rows matter because StreamInfo is an ordinary exported struct.
// The reader bounds a declared frame count before publishing it, so a crafted
// file can no longer reach the largest of these values through ParseHeader, but
// a caller assembling a StreamInfo by hand faces no such bound and Duration
// must survive whatever it is handed. Every step of the arithmetic has to hold,
// not just the conversion to int64: the
// rows below pin the conversion and both arithmetic ceilings from either side,
// so that a bound which is dropped, or moved by one in either direction, turns
// a zero into a wrapped length or a real length into a zero. Wrapping is the
// one answer the documented contract rules out, whichever sign it lands on.
// Duration's fourth bound, the one on the sample rate, has a test of its own,
// because the rates that reach it do not fit an int on every platform.
func TestStreamInfoDuration(t *testing.T) {
	tests := []struct {
		name        string
		totalFrames uint64
		sampleRate  int
		want        time.Duration
	}{
		{"zero TotalFrames reports zero regardless of sample rate", 0, 48000, 0},
		{"zero sample rate reports zero regardless of frame count", 48000, 0, 0},
		{"negative sample rate reports zero", 48000, -1, 0},
		{"the first frame count past math.MaxInt64 reports zero", math.MaxInt64 + 1, 48000, 0},
		{"an all-ones frame count reports zero", math.MaxUint64, 48000, 0},
		{"math.MaxInt64 frames survives the conversion and is rejected by the seconds ceiling", math.MaxInt64, 48000, 0},
		{
			// Past math.MaxInt64, and chosen so that the wrapped negative
			// int64 is an exact multiple of the sample rate. That leaves no
			// remainder, which is what the two later bounds happen to trip
			// over for most oversized counts, so this row is the one that
			// actually witnesses the conversion guard. Without it the answer
			// is 1708031h51m31.399183872s: 195 years, positive, and nothing
			// about it looks wrong, which is why a positive wrapped result is
			// worse than a negative one.
			"a frame count past math.MaxInt64 that wraps to a whole number of seconds reports zero",
			9223372036854783616, 48000, 0,
		},
		{
			// The longest stream Duration can describe at all, and the one
			// input that lands on math.MaxInt64 nanoseconds exactly rather
			// than stepping over it. It pins two bounds from the inside at
			// once: the conversion admits math.MaxInt64 frames rather than
			// rejecting them, and the final addition allows a total equal to
			// math.MaxInt64 rather than only one below it. Tightening either
			// comparison by one turns a stateable length into a zero.
			"math.MaxInt64 frames at 1 GHz is exactly math.MaxInt64 nanoseconds",
			math.MaxInt64, 1_000_000_000,
			math.MaxInt64 * time.Nanosecond,
		},
		{
			// A ds64 data chunk size just under the 1<<62 ceiling
			// resolveDataSize enforces, spread across four-byte frames. It is
			// not a crafted edge case: it is the largest stream the parser
			// considers valid, and it must not report the 48 years a wrapped
			// multiplication makes of its true 761,000.
			"the frame count of the largest data chunk the parser accepts reports zero",
			1152921504606846975, 48000, 0,
		},
		{
			// The whole-seconds ceiling, pinned from both sides at the same
			// rate. math.MaxInt64/int64(time.Second) is 9223372036, so
			// 9223372036 seconds of 48 kHz audio is exactly representable and
			// one second more is not.
			"the last whole second representable at 48 kHz",
			442_721_857_728_000, 48000, 9223372036 * time.Second,
		},
		{
			"one whole second past what is representable at 48 kHz",
			442_721_857_776_000, 48000, 0,
		},
		{
			// The final addition, pinned from both sides. Both frame counts
			// clear the seconds ceiling with the same 9223372036 whole
			// seconds; they differ only in the sub-second remainder, which
			// tips the second of them past math.MaxInt64 nanoseconds.
			"the largest frame count representable at 48 kHz",
			442_721_857_769_029, 48000, 9223372036854770833 * time.Nanosecond,
		},
		{
			"one frame past the largest representable at 48 kHz",
			442_721_857_769_030, 48000, 0,
		},
		{"exact whole second", 48000, 48000, time.Second},
		{"exact two seconds", 96000, 48000, 2 * time.Second},
		{"inexact: one third of a second at a 3 Hz rate", 1, 3, 333333333 * time.Nanosecond},
		{"inexact: 100 frames at 44100 Hz", 100, 44100, 2267573 * time.Nanosecond},
		{"inexact: half a second at 44100 Hz", 22050, 44100, 500 * time.Millisecond},
		{
			"a frame count large enough that naive frames * time.Second would overflow int64",
			10_000_000_000_000, 48000,
			208333333333333333 * time.Nanosecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			si := wav.StreamInfo{TotalFrames: tt.totalFrames, SampleRate: tt.sampleRate}
			if got := si.Duration(); got != tt.want {
				t.Errorf("Duration() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStreamInfoDurationSampleRateCeiling pins the last of Duration's bounds,
// the one on the sample rate, from both sides. A sub-second remainder is always
// below one sample rate, so remainder * time.Second overflows int64 once the
// rate passes math.MaxInt64/int64(time.Second). Duration reports 0 from there
// on rather than compute in wider arithmetic, and the rejected side of the
// boundary shows what that buys: the second row is a stream 582 milliseconds
// long that the unbounded arithmetic calls 45 milliseconds, quietly and with
// the right sign.
//
// This is the one bound that also refuses lengths it could have stated, so the
// first row matters as much as the second: a rate exactly at the ceiling is
// still served, remainder included, and tightening the comparison by one would
// throw that away.
//
// No file can reach either row, because a fmt chunk stores the sample rate in
// 32 bits, so both rates have to be built by hand. That is also why the ceiling
// is computed at run time: written as a constant it would not fit an int on a
// 32-bit platform, and the conversion would fail to compile there instead of
// being skipped.
func TestStreamInfoDurationSampleRateCeiling(t *testing.T) {
	ceiling := int64(math.MaxInt64) / int64(time.Second)
	if ceiling > math.MaxInt {
		t.Skip("an int is narrower than 64 bits here, so no sample rate can reach the ceiling")
	}
	tests := []struct {
		name        string
		totalFrames uint64
		sampleRate  int64
		want        time.Duration
	}{
		// 13835058054 frames is one and a half times the ceiling rate, so the
		// row exercises the remainder term there as well as the whole seconds.
		{"a sample rate at the ceiling is served, remainder and all", 13_835_058_054, ceiling, 1500 * time.Millisecond},
		{"a sample rate past the ceiling reports zero", 20_000_000_000, 1 << 35, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			si := wav.StreamInfo{TotalFrames: tt.totalFrames, SampleRate: int(tt.sampleRate)}
			if got := si.Duration(); got != tt.want {
				t.Errorf("Duration() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestContainerString pins the four-character magic each container reports,
// including the default arm reached by a value outside the declared enum.
//
// The value one past the last declared member is a deliberate tripwire. Adding
// a fourth container makes ContainerBW64+1 a real member, so this row starts
// asserting that the new member stringifies as "unknown" and fails until it is
// given a case here and an arm in String. A far-away value such as 99 would
// not do that, and neither would the exhaustive linter: .golangci.yaml sets
// default-signifies-exhaustive, so a switch with a default arm is considered
// exhaustive and a new member would silently gain an untested arm.
func TestContainerString(t *testing.T) {
	tests := []struct {
		name string
		c    wav.Container
		want string
	}{
		{"RIFF", wav.ContainerRIFF, "RIFF"},
		{"RF64", wav.ContainerRF64, "RF64"},
		{"BW64", wav.ContainerBW64, "BW64"},
		{tripwireName, wav.ContainerBW64 + 1, unknownName},
		{outOfRangeName, wav.Container(99), unknownName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestContainerSized64 pins which containers carry 64-bit sizes in a ds64
// chunk, including the default arm. As in TestContainerString, the value one
// past the last declared member is a tripwire that turns into a real member
// the moment a fourth container is added, forcing it to be classified here.
func TestContainerSized64(t *testing.T) {
	tests := []struct {
		name string
		c    wav.Container
		want bool
	}{
		{"RIFF is not 64-bit sized", wav.ContainerRIFF, false},
		{"RF64 is 64-bit sized", wav.ContainerRF64, true},
		{"BW64 is 64-bit sized", wav.ContainerBW64, true},
		{"one past the last declared member is not 64-bit sized", wav.ContainerBW64 + 1, false},
		{"out of range value is not 64-bit sized", wav.Container(99), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Sized64(); got != tt.want {
				t.Errorf("Sized64() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSampleFormatString pins the short name each sample format reports,
// including the default arm reached by a value outside the declared enum. The
// value one past the last declared member is a tripwire: adding a further
// sample format makes SampleFormatMuLaw+1 a real member and fails this row
// until the new format has both a name here and an arm in String.
//
// The tripwire is the only guard, because .golangci.yaml sets exhaustive's
// default-signifies-exhaustive, so a switch with a default arm is not reported
// when a member is added.
func TestSampleFormatString(t *testing.T) {
	tests := []struct {
		name string
		f    wav.SampleFormat
		want string
	}{
		{"PCM", wav.SampleFormatPCM, "pcm"},
		{"Float", wav.SampleFormatFloat, "float"},
		{"ALaw", wav.SampleFormatALaw, "a-law"},
		{"MuLaw", wav.SampleFormatMuLaw, "mu-law"},
		{tripwireName, wav.SampleFormatMuLaw + 1, unknownName},
		{outOfRangeName, wav.SampleFormat(99), unknownName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSampleFormatCompanded pins which formats report themselves as companded.
// Like the String table above, the row one past the last declared member is a
// tripwire against a new format being added without a decision here.
func TestSampleFormatCompanded(t *testing.T) {
	tests := []struct {
		name string
		f    wav.SampleFormat
		want bool
	}{
		{"PCM", wav.SampleFormatPCM, false},
		{"Float", wav.SampleFormatFloat, false},
		{"ALaw", wav.SampleFormatALaw, true},
		{"MuLaw", wav.SampleFormatMuLaw, true},
		{tripwireName, wav.SampleFormatMuLaw + 1, false},
		{outOfRangeName, wav.SampleFormat(99), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Companded(); got != tt.want {
				t.Errorf("Companded() = %v, want %v", got, tt.want)
			}
		})
	}
}

// errPrefix is the package prefix every sentinel message carries, so that an
// error surfacing in a caller's log names the library it came from.
const errPrefix = "go-wav: "

// TestSentinelErrors pins that every sentinel in errors.go is non-nil, carries
// the package prefix, reads exactly as documented, and is distinct from the
// others.
//
// The message text is pinned verbatim rather than merely checked for being
// non-empty, because the messages are the whole content of these values. A
// sentinel has no fields and no behaviour, so dropping the prefix or giving two
// sentinels the same wording changes what callers see while leaving every
// structural property intact. Same-wording sentinels are exactly the confusion
// the doc comments in errors.go exist to prevent, ErrNotRIFF meaning "this is
// not a WAV file" against ErrCorruptStream meaning "it is a WAV file and it is
// broken", and a test that cannot see the text cannot see that happen.
//
// Wrapping is deliberately not asserted here. errors.Is(err, err) holds for
// every comparable non-nil error, and errors.Is(fmt.Errorf("%w", err), err)
// holds because of fmt and errors, not because of anything this package does,
// so those assertions cannot fail for any change to errors.go.
func TestSentinelErrors(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
		want string
	}{
		{"ErrNotRIFF", wav.ErrNotRIFF, "go-wav: not a RIFF, RF64 or BW64 stream"},
		{"ErrCorruptStream", wav.ErrCorruptStream, "go-wav: corrupt stream"},
		{"ErrUnsupported", wav.ErrUnsupported, "go-wav: unsupported format"},
		{"ErrEncoderClosed", wav.ErrEncoderClosed, "go-wav: encoder is closed"},
		{
			"ErrTooLarge", wav.ErrTooLarge,
			"go-wav: stream exceeds the 4 GiB RIFF limit and RF64 is unavailable",
		},
		{
			"ErrSeekUnsupported", wav.ErrSeekUnsupported,
			"go-wav: seek unsupported (source is not an io.Seeker)",
		},
	}

	for _, s := range sentinels {
		t.Run(s.name, func(t *testing.T) {
			if s.err == nil {
				t.Fatal("sentinel error is nil")
			}
			got := s.err.Error()
			if !strings.HasPrefix(got, errPrefix) {
				t.Errorf("Error() = %q, want a message prefixed with %q", got, errPrefix)
			}
			if got != s.want {
				t.Errorf("Error() = %q, want %q", got, s.want)
			}
		})
	}

	t.Run("all sentinel messages are distinct", func(t *testing.T) {
		seen := make(map[string]string, len(sentinels))
		for _, s := range sentinels {
			if prev, dup := seen[s.err.Error()]; dup {
				t.Errorf("%s and %s share the message %q", prev, s.name, s.err.Error())
				continue
			}
			seen[s.err.Error()] = s.name
		}
	})

	t.Run("all sentinels are distinct", func(t *testing.T) {
		for i, a := range sentinels {
			for j, b := range sentinels {
				if i == j {
					continue
				}
				if errors.Is(a.err, b.err) {
					t.Errorf("%s and %s compare equal under errors.Is", a.name, b.name)
				}
			}
		}
	})
}

// TestVersion pins the shape of Version, and nothing more. On an ordinary run
// that is all it can do: any semver-shaped string passes, whether or not it
// matches the version actually being released. The failure that matters, a tag
// cut without bumping the constant, is caught by
// [TestVersionMatchesReleaseTag] instead, which needs the tag as input and so
// cannot run outside a release.
func TestVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(wav.Version) {
		t.Errorf("Version = %q, want a semver-shaped string", wav.Version)
	}
}

// releaseTagEnv names the environment variable the release workflow sets to the
// tag it is building, which is the only thing that can tell this package which
// version it is supposed to be.
const releaseTagEnv = "GO_WAV_RELEASE_TAG"

// TestVersionMatchesReleaseTag is the check the release runs and nothing else
// does. Version is referenced nowhere but its own declaration and these tests,
// so no ordinary build can notice it has gone stale; the tag is the missing
// input, and it exists only while a release is being built.
//
// Skipping when the variable is absent is what lets this live with the tests
// rather than as a shell step in the workflow, where it would drift from the
// constant it guards. The cost is that a green local run says nothing about
// this test, which is why the workflow runs it as its own step and fails the
// release on it rather than relying on the suite.
func TestVersionMatchesReleaseTag(t *testing.T) {
	tag, ok := os.LookupEnv(releaseTagEnv)
	if !ok {
		t.Skipf("%s is unset; this check runs only while building a release", releaseTagEnv)
	}
	want := strings.TrimPrefix(tag, "v")
	if wav.Version != want {
		t.Fatalf("Version = %q but the tag being released is %q; bump the constant in wav.go or fix the tag",
			wav.Version, tag)
	}
}
