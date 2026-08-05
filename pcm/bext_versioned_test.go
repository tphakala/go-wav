package pcm

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// bextUMIDOffset and bextLoudnessOffset are where UMID and the first loudness
// field land in the fixed body, derived from the field widths ahead of them
// rather than restated, so a layout change moves them in lockstep with
// serialize.
const (
	bextUMIDOffset = bextDescriptionSize + bextOriginatorSize + bextOriginatorReferenceSize +
		bextOriginationDateSize + bextOriginationTimeSize + bextTimeReferenceSize + bextVersionSize
	bextLoudnessOffset = bextUMIDOffset + bextUMIDSize
	bextReservedOffset = bextLoudnessOffset + bextLoudnessFieldSize*bextLoudnessFieldCount
)

// TestBextSerializesUMIDAndLoudness checks that a version 2 descriptor carrying
// a UMID and the five loudness fields writes each one at its wire offset, that
// the loudness fields round-trip as signed little-endian int16, and that the
// trailing Reserved region stays zero.
func TestBextSerializesUMIDAndLoudness(t *testing.T) {
	var umid [bextUMIDSize]byte
	for i := range umid {
		umid[i] = byte(i + 1) // a distinctive, all-nonzero pattern
	}
	b := &Bext{
		Version:              2,
		UMID:                 umid,
		LoudnessValue:        -2301, // -23.01 LUFS
		LoudnessRange:        750,   // 7.50 LU
		MaxTruePeakLevel:     -105,  // -1.05 dBTP
		MaxMomentaryLoudness: -1999,
		MaxShortTermLoudness: 1234,
	}
	if err := b.validate("test"); err != nil {
		t.Fatalf("a consistent version 2 descriptor was rejected: %v", err)
	}
	body := b.serialize()
	if len(body) != bextFixedSize {
		t.Fatalf("serialize() = %d bytes, want %d", len(body), bextFixedSize)
	}

	if got := body[bextUMIDOffset : bextUMIDOffset+bextUMIDSize]; !bytes.Equal(got, umid[:]) {
		t.Errorf("UMID at offset %d: got %x, want %x", bextUMIDOffset, got, umid)
	}

	loud := []struct {
		name string
		want int16
	}{
		{bextLoudnessValueName, b.LoudnessValue},
		{bextLoudnessRangeName, b.LoudnessRange},
		{bextMaxTruePeakLevelName, b.MaxTruePeakLevel},
		{bextMaxMomentaryLoudnessName, b.MaxMomentaryLoudness},
		{bextMaxShortTermLoudnessName, b.MaxShortTermLoudness},
	}
	for i, f := range loud {
		off := bextLoudnessOffset + i*bextLoudnessFieldSize
		got := int16(binary.LittleEndian.Uint16(body[off : off+bextLoudnessFieldSize]))
		if got != f.want {
			t.Errorf("%s at offset %d: got %d, want %d", f.name, off, got, f.want)
		}
	}

	if !allZero(body[bextReservedOffset:bextFixedSize]) {
		t.Errorf("Reserved region %d:%d is not all zero", bextReservedOffset, bextFixedSize)
	}
}

// TestBextZeroValueSerializesToAllZero pins the byte-for-byte guarantee: a
// zero-value descriptor still writes 602 NUL bytes, exactly as it did before
// UMID and the loudness fields were exposed.
func TestBextZeroValueSerializesToAllZero(t *testing.T) {
	body := (&Bext{}).serialize()
	if len(body) != bextFixedSize {
		t.Fatalf("serialize() = %d bytes, want %d", len(body), bextFixedSize)
	}
	if !allZero(body) {
		t.Error("a zero-value Bext no longer serializes to an all-zero body")
	}
}

// TestBextValidateVersionGating checks that UMID needs Version >= 1 and the
// loudness fields need Version >= 2, that each of the five loudness fields
// triggers the guard on its own, and that a zero-valued field is accepted at
// any version.
func TestBextValidateVersionGating(t *testing.T) {
	nonZeroUMID := func() [bextUMIDSize]byte {
		var u [bextUMIDSize]byte
		u[0] = 1
		return u
	}

	t.Run("UMID needs version >= 1", func(t *testing.T) {
		if err := (&Bext{Version: 0, UMID: nonZeroUMID()}).validate("test"); err == nil {
			t.Error("a UMID set with Version 0 was accepted")
		}
		for _, v := range []uint16{1, 2} {
			if err := (&Bext{Version: v, UMID: nonZeroUMID()}).validate("test"); err != nil {
				t.Errorf("a UMID set with Version %d was rejected: %v", v, err)
			}
		}
		if err := (&Bext{Version: 0}).validate("test"); err != nil {
			t.Errorf("a zero UMID at Version 0 was rejected: %v", err)
		}
	})

	t.Run("each loudness field needs version >= 2", func(t *testing.T) {
		fields := []struct {
			name string
			set  func(*Bext)
		}{
			{bextLoudnessValueName, func(b *Bext) { b.LoudnessValue = 1 }},
			{bextLoudnessRangeName, func(b *Bext) { b.LoudnessRange = 1 }},
			{bextMaxTruePeakLevelName, func(b *Bext) { b.MaxTruePeakLevel = 1 }},
			{bextMaxMomentaryLoudnessName, func(b *Bext) { b.MaxMomentaryLoudness = 1 }},
			{bextMaxShortTermLoudnessName, func(b *Bext) { b.MaxShortTermLoudness = 1 }},
		}
		for _, f := range fields {
			t.Run(f.name, func(t *testing.T) {
				for _, v := range []uint16{0, 1} {
					b := &Bext{Version: v}
					f.set(b)
					if err := b.validate("test"); err == nil {
						t.Errorf("%s set with Version %d was accepted", f.name, v)
					}
				}
				b := &Bext{Version: 2}
				f.set(b)
				if err := b.validate("test"); err != nil {
					t.Errorf("%s set with Version 2 was rejected: %v", f.name, err)
				}
			})
		}
	})

	t.Run("a negative loudness value still trips the guard", func(t *testing.T) {
		if err := (&Bext{Version: 1, LoudnessValue: -1}).validate("test"); err == nil {
			t.Error("a negative LoudnessValue at Version 1 was accepted")
		}
	})

	t.Run("version 2 with zero loudness is allowed", func(t *testing.T) {
		if err := (&Bext{Version: 2}).validate("test"); err != nil {
			t.Errorf("a Version 2 descriptor with zero loudness was rejected: %v", err)
		}
	})
}
