package riff

import (
	"fmt"
	"testing"

	wav "github.com/tphakala/go-wav"
)

// TestHeaderLenMatchesBuildHeader pins the computed header length to the built
// one. HeaderLen exists so the RF64 decision costs arithmetic rather than a
// throwaway allocation, which makes it a second statement of the same layout;
// this is what stops the two drifting apart.
func TestHeaderLenMatchesBuildHeader(t *testing.T) {
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
	// Only the containers BuildHeader will emit; BW64 is read only.
	containers := []wav.Container{wav.ContainerRIFF, wav.ContainerRF64}

	for _, f := range formats {
		for _, ch := range []int{1, 2, 3, 6, 8} {
			for _, c := range containers {
				for _, reserve := range []bool{false, true} {
					for _, ext := range []bool{false, true} {
						cfg := HeaderConfig{
							Format: Format{
								SampleRate: 48000,
								Channels:   ch,
								BitDepth:   f.bits,
								Format:     f.format,
								Extensible: ext,
							},
							Container:   c,
							ReserveDS64: reserve,
						}
						name := fmt.Sprintf("%s%d_%dch_%s_reserve%v_ext%v",
							f.format, f.bits, ch, c, reserve, ext)
						t.Run(name, func(t *testing.T) {
							lay, err := BuildHeader(cfg)
							if err != nil {
								t.Fatalf("BuildHeader: %v", err)
							}
							want := int64(len(lay.Bytes))
							if got := HeaderLen(cfg); got != want {
								t.Errorf("HeaderLen = %d, BuildHeader emitted %d bytes", got, want)
							}
							if lay.DataOffset != want {
								t.Errorf("DataOffset = %d, want %d", lay.DataOffset, want)
							}
						})
					}
				}
			}
		}
	}
}

// TestFitsPlainRIFFMatchesFitsRIFF pins the computed size test to the built one.
func TestFitsPlainRIFFMatchesFitsRIFF(t *testing.T) {
	cfg := HeaderConfig{
		Format:    Format{SampleRate: 48000, Channels: 2, BitDepth: 16, Format: wav.SampleFormatPCM},
		Container: wav.ContainerRIFF,
	}
	lay, err := BuildHeader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Probe around the boundary, where the two must agree exactly.
	limit := maxUint32 - lay.DataOffset + 8
	for _, n := range []int64{0, 1, 1024, limit - 2, limit - 1, limit, limit + 1, limit + 2, maxUint32} {
		if got, want := FitsPlainRIFF(cfg, n), FitsRIFF(lay, n); got != want {
			t.Errorf("dataSize %d: FitsPlainRIFF = %v, FitsRIFF = %v", n, got, want)
		}
	}
}
