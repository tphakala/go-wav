package wav_test

import (
	"fmt"

	wav "github.com/tphakala/go-wav"
)

// ExampleSniffContainer reports the container flavour of a header without
// decoding it, for a caller dispatching on file type. It reads at most the
// first twelve bytes and returns false rather than panicking on a short slice.
func ExampleSniffContainer() {
	// The twelve-byte file header of a plain RIFF WAVE stream: the magic, a
	// size field (ignored here), then the WAVE form type.
	header := []byte("RIFF\x00\x00\x00\x00WAVE")

	container, ok := wav.SniffContainer(header)
	fmt.Println(container, ok)
	// Output: RIFF true
}

// ExampleSniff reports whether a byte slice begins with a RIFF, RF64 or BW64
// WAVE header, the yes-or-no companion to [SniffContainer].
func ExampleSniff() {
	fmt.Println(wav.Sniff([]byte("RF64\xff\xff\xff\xffWAVE")))
	fmt.Println(wav.Sniff([]byte("not a wav file")))
	// Output:
	// true
	// false
}
