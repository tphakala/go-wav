package pcm

// MaxConvertBatch exposes the conversion batch cap to the pcm_test package, so
// that a test asserting how much a converting Read returns can derive the
// number from the constant instead of restating it. A hardcoded copy would
// keep passing against a changed cap, or fail with a message describing the
// wrong cause.
const MaxConvertBatch = maxConvertBatch

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
