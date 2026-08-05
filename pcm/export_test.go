package pcm

// MaxConvertBatch exposes the conversion batch cap to the pcm_test package, so
// that a test asserting how much a converting Read returns can derive the
// number from the constant instead of restating it. A hardcoded copy would
// keep passing against a changed cap, or fail with a message describing the
// wrong cause.
const MaxConvertBatch = maxConvertBatch

// WriteToBufferSize exposes WriteTo's staging-buffer size to the pcm_test
// package, so a test asserting the sink's write chunking derives the expected
// block size from the constant instead of restating 65536.
const WriteToBufferSize = writeToBufferSize

// BextFixedSize exposes the width of a bext chunk's fixed body to the
// pcm_test package, so a fuzz target can assert Serialize's output length
// against the constant rather than restating 602.
const BextFixedSize = bextFixedSize

// Serialize exposes Bext.serialize to the pcm_test package, so a fuzz target
// can call it directly on arbitrary field values rather than only observing
// its effect through a whole encoded stream.
func (b *Bext) Serialize() []byte { return b.serialize() }

// Validate exposes Bext.validate to the pcm_test package, matching Serialize
// above.
func (b *Bext) Validate(op string) error { return b.validate(op) }

// ParseBext exposes parseBext to the pcm_test package, so a fuzz target can
// drive the reader with arbitrary bodies and a round-trip test can invert
// Serialize directly rather than only through a whole decoded stream.
func ParseBext(b []byte) (*Bext, error) { return parseBext(b) }

// BextOffVersion is the byte offset of the Version field within a bext body,
// exposed so a version-gating test can lower the on-wire Version without
// restating the offset the parser derives.
const BextOffVersion = bextOffVersion
